package conductor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nwylynko/slopball-cli/contracts"
	"github.com/nwylynko/slopball-cli/harness"
)

// docFiles are repo docs the merger reads to understand the direction of
// changes (§6.2) — plan/README/contract files, never code. The brief is in
// there so conflicts get resolved with the project's actual direction in hand.
var docFiles = []string{"README.md", "AGENTS.md", "CLAUDE.md", "MASTERPLAN.md", contracts.BriefFile}

// mergerSystem is the merger role-agent's standing instruction (§6.2): bias
// toward the incoming change, keep the tree runnable.
const mergerSystem = `You are slopball's merger — one role-agent in a fleet that keeps many AI agents' code merged into one runnable product. You resolve a single git merge conflict.

Rules:
- Bias STRONGLY toward the incoming change (the "theirs" side, between ======= and >>>>>>>) as the correct intent. Rearrange the surrounding existing code so the incoming change slots in and nothing breaks.
- Never leave conflict markers (<<<<<<<, =======, >>>>>>>) in the output.
- Keep the file syntactically valid and runnable.
- Output ONLY the full resolved file contents. No commentary, no explanation, no markdown code fences.`

// HarnessResolver returns a ConflictResolver backed by a harness CLI (or Fake).
// A nil client yields a nil resolver (mechanical --theirs fallback).
func HarnessResolver(c *harness.Client) ConflictResolver {
	if c == nil {
		return nil
	}
	return func(ctx context.Context, workDir, branch, intent string, conflicted []string) (map[string]string, error) {
		// Run the CLI in the tree being merged, not wherever slopball was
		// started from. Per-call rather than on the shared client: roles hold
		// the same *harness.Client when the session falls back to this
		// machine's agent, and they tick concurrently.
		c := withDir(c, workDir)
		docs := readDocs(workDir)
		out := make(map[string]string, len(conflicted))
		for _, path := range conflicted {
			body, err := os.ReadFile(filepath.Join(workDir, path))
			if err != nil {
				return nil, err
			}
			user := fmt.Sprintf(
				"Repo docs (direction of the project):\n%s\n\nThe change came from branch %q. The pushing agent's intent note:\n%s\n\nFile %q has git merge conflict markers. Resolve them and return the full file:\n\n%s",
				docs, branch, orNone(intent), path, string(body),
			)
			resolved, err := c.Complete(ctx, mergerSystem, user)
			if err != nil {
				return nil, err
			}
			out[path] = stripFences(resolved)
		}
		return out, nil
	}
}

// fixSystem instructs the error-watcher role-agent to emit one machine-readable
// fix patch for the runtime error it was shown.
const fixSystem = `You are slopball's error-watcher — one role-agent in a fleet keeping the shared product runnable. The dev server logged a runtime or compile error. Given the error and the current source, produce the single smallest edit that fixes it.

Respond with ONLY a JSON object, no markdown fences, no prose:
{"path": "<repo-relative file to write>", "content": "<the full new contents of that file>"}`

// HarnessFixer returns a Fixer backed by a harness CLI. Nil client → nil Fixer
// (mechanical marker-file fallback).
func HarnessFixer(c *harness.Client) Fixer {
	if c == nil {
		return nil
	}
	return func(ctx context.Context, errLog string, files map[string]string) (string, string, error) {
		user := fmt.Sprintf("Runtime error from the dev server logs:\n%s\n\nCurrent source files:\n%s", errLog, renderTree(files))
		raw, err := c.Complete(ctx, fixSystem, user)
		if err != nil {
			return "", "", err
		}
		var patch struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(stripFences(raw)), &patch); err != nil {
			return "", "", fmt.Errorf("error-watcher: bad patch json: %w", err)
		}
		if patch.Path == "" {
			return "", "", fmt.Errorf("error-watcher: patch missing path")
		}
		return patch.Path, patch.Content, nil
	}
}

// HarnessScaffolder returns a Scaffolder backed by an agentic harness run. A
// nil client yields a nil Scaffolder, which disables the setup role — there is
// no mechanical way to turn a sentence into a project, and a stub index.html
// pretending to be one is worse than an empty repo with an honest log line.
func HarnessScaffolder(c *harness.Client) Scaffolder {
	if c == nil {
		return nil
	}
	return func(ctx context.Context, dir, prompt string, onActivity func(string)) error {
		// A copy per run: OnEvent closes over this call's activity sink, and the
		// client is shared with whatever else the fleet built from it.
		run := *c
		if onActivity != nil {
			run.OnEvent = func(e harness.Event) {
				if a := e.Activity(); a != "" {
					onActivity(a)
				}
			}
		}
		return run.Agent(ctx, dir, prompt, &logWriter{})
	}
}

// logWriter streams the agent's output into the fleet's log a line at a time.
// A 90-second scaffold behind a buffered pipe is indistinguishable from a hang,
// and on a cloud box this log is the only place anyone can watch it work.
type logWriter struct{ partial string }

func (w *logWriter) Write(p []byte) (int, error) {
	w.partial += string(p)
	for {
		nl := strings.IndexByte(w.partial, '\n')
		if nl < 0 {
			break
		}
		if line := strings.TrimSpace(w.partial[:nl]); line != "" {
			setupLog.Infof("%s", line)
		}
		w.partial = w.partial[nl+1:]
	}
	return len(p), nil
}

// withDir returns a shallow copy of c bound to dir, leaving the caller's client
// untouched. Nil-safe so the mechanical-fallback path stays a plain nil check.
func withDir(c *harness.Client, dir string) *harness.Client {
	if c == nil || dir == "" {
		return c
	}
	cp := *c
	cp.Dir = dir
	return &cp
}

func readDocs(workDir string) string {
	var b strings.Builder
	for _, name := range docFiles {
		if data, err := os.ReadFile(filepath.Join(workDir, filepath.FromSlash(name))); err == nil {
			fmt.Fprintf(&b, "== %s ==\n%s\n", name, string(data))
		}
	}
	if b.Len() == 0 {
		return "(no repo docs)"
	}
	return b.String()
}

func renderTree(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "== %s ==\n%s\n", p, files[p])
	}
	return b.String()
}

func stripFences(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return s
	}
	if nl := strings.IndexByte(t, '\n'); nl >= 0 {
		t = t[nl+1:]
	}
	t = strings.TrimSuffix(strings.TrimRight(t, " \n"), "```")
	return t
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none provided)"
	}
	return s
}
