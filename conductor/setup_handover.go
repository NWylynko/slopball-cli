package conductor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nwylynko/slopball-cli/contracts"
)

// This file is plan 31: how slopball's own files get out of a generator's way
// and back again.
//
// The setup role is told to prefer the ecosystem's generator over hand-writing
// files, and every one of them refuses to run in a directory holding anything
// outside a small tolerated list. `README.md`, `.gitignore`, `.git` and
// `LICENSE` are on that list. `AGENTS.md`, `CLAUDE.md`, `.cursor/` and
// `.slopball/` are not — and since plan 31 those are on main before the role
// ever runs. So they move aside for the scaffold turn.
//
// Putting them back is the interesting half, because a modern generator may
// write its **own** `AGENTS.md`, which is good and must survive. A blind restore
// would destroy exactly the thing worth keeping.

// privateDir is slopball's own namespace. Nothing else writes there — the agent
// is told so in as many words — so it is restored mechanically and is never an
// agent's decision. Losing brief.md would disable the setup role for the rest of
// the session.
const privateDir = ".slopball"

// incomingDir is where a colliding contract waits for the merge turn. It sits
// inside the work tree because that is the only place the agent can read, and
// it must be gone before the commit — which verify enforces.
const incomingDir = ".slopball-incoming"

// heldFiles is what was moved out of the work tree for the scaffold turn.
type heldFiles struct {
	dir       string            // holding area, outside the work tree
	private   bool              // .slopball was moved
	contracts map[string]string // work-tree-relative path → held absolute path
}

// setAside empties the work tree of slopball's own files so a generator will
// run in it. Only called in scaffold mode: adapt mode has a real project, no
// generator is involved, and nothing collides.
func setAside(work string) (*heldFiles, error) {
	hold, err := os.MkdirTemp("", "slopball-held-*")
	if err != nil {
		return nil, err
	}
	h := &heldFiles{dir: hold, contracts: map[string]string{}}
	if src := filepath.Join(work, privateDir); exists(src) {
		if err := os.Rename(src, filepath.Join(hold, privateDir)); err != nil {
			return nil, fmt.Errorf("set aside %s: %w", privateDir, err)
		}
		h.private = true
	}
	for _, rel := range contracts.Files {
		src := filepath.Join(work, filepath.FromSlash(rel))
		if !exists(src) {
			continue
		}
		dst := filepath.Join(hold, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := os.Rename(src, dst); err != nil {
			return nil, fmt.Errorf("set aside %s: %w", rel, err)
		}
		h.contracts[rel] = dst
		// An emptied `.cursor/` is still a directory entry, and the generator
		// counts entries, not files.
		pruneEmptyDirs(work, filepath.Dir(src))
	}
	return h, nil
}

// restore gives the held files back and reports which ones the generator had
// already written for itself. Those are staged under incomingDir for the merge
// turn; everything else is simply moved home, because "put this file back where
// it was" is not a decision worth an agent turn.
func (h *heldFiles) restore(work string) ([]string, error) {
	if h.private {
		dst := filepath.Join(work, privateDir)
		// The agent was told not to touch this namespace. If it did anyway,
		// slopball's copy is the authoritative one.
		if err := os.RemoveAll(dst); err != nil {
			return nil, err
		}
		if err := os.Rename(filepath.Join(h.dir, privateDir), dst); err != nil {
			return nil, fmt.Errorf("restore %s: %w", privateDir, err)
		}
	}
	var collided []string
	for _, rel := range contracts.Files {
		held, ok := h.contracts[rel]
		if !ok {
			continue
		}
		dst := filepath.Join(work, filepath.FromSlash(rel))
		if exists(dst) {
			in := filepath.Join(work, incomingDir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(in), 0o755); err != nil {
				return nil, err
			}
			if err := os.Rename(held, in); err != nil {
				return nil, fmt.Errorf("stage %s for merge: %w", rel, err)
			}
			collided = append(collided, rel)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := os.Rename(held, dst); err != nil {
			return nil, fmt.Errorf("restore %s: %w", rel, err)
		}
	}
	sort.Strings(collided)
	return collided, nil
}

// cleanup drops the holding area. The work tree is the source of truth by now.
func (h *heldFiles) cleanup() {
	if h != nil && h.dir != "" {
		_ = os.RemoveAll(h.dir)
	}
}

// verify is the guard. A merge that quietly dropped the sync protocol is worse
// than one that failed: the session looks healthy and nobody's work ever lands,
// because no agent was ever told to run `slopball sync`. Same posture as
// guardStaged — refuse, name the file, leave main where it was.
func (h *heldFiles) verify(work string) error {
	if exists(filepath.Join(work, incomingDir)) {
		return fmt.Errorf("%s still holds files the merge turn did not fold in", incomingDir)
	}
	for _, rel := range contracts.Files {
		if _, held := h.contracts[rel]; !held {
			continue // never on main to begin with; nothing was promised
		}
		body, err := os.ReadFile(filepath.Join(work, filepath.FromSlash(rel)))
		if err != nil {
			return fmt.Errorf("%s went missing during the merge: %w", rel, err)
		}
		// Per-file, because the contracts no longer say the same thing: AGENTS.md
		// carries the protocol and CLAUDE.md carries the pointer at it. A merge
		// that drops either one leaves a file that still looks like a contract
		// and instructs nobody.
		if needle := contracts.Requires(rel); !strings.Contains(string(body), needle) {
			return fmt.Errorf("%s no longer contains %q — without it agents edit happily and never land anything",
				rel, needle)
		}
	}
	return nil
}

// mergePrompt asks for the one thing that genuinely needs judgement: reconciling
// a file the scaffold wrote with slopball's copy of the same path.
func mergePrompt(collided []string) string {
	var b strings.Builder
	b.WriteString(`You are slopball's setup role-agent, continuing from the project you just created.

You wrote your own versions of files that slopball also maintains. Both matter: yours carries what you know about the project, slopball's carries the protocol every agent in this session follows. Merge them — do not pick a side.

`)
	for _, rel := range collided {
		fmt.Fprintf(&b, "- %s — slopball's copy is at %s/%s, and the merged file MUST still contain %q\n",
			rel, incomingDir, rel, contracts.Requires(rel))
	}
	fmt.Fprintf(&b, `
For each file: keep your content, fold slopball's in, and write the result back to the original path.

Hard rules:
- Each file MUST still contain the text named beside it above. That is how work reaches the rest of the team; dropping it silently breaks the session.
- Delete the %s directory when you are done. It is scaffolding, not part of the project.
- Do not create, edit or delete anything under %s/, and NEVER run any git command — slopball makes the commit.
`, incomingDir, privateDir)
	return b.String()
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// pruneEmptyDirs removes now-empty directories from dir upwards, stopping at
// root. A generator counts directory entries, so an emptied `.cursor/` is still
// enough to make it refuse.
func pruneEmptyDirs(root, dir string) {
	root = filepath.Clean(root)
	for dir = filepath.Clean(dir); strings.HasPrefix(dir, root) && dir != root; dir = filepath.Dir(dir) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
	}
}

// scaffoldHandover runs the set-aside → scaffold → merge sequence and leaves the
// work tree ready to commit. Split out of Tick so the ordering — and especially
// the fact that verify runs before anything is staged — reads in one place.
func (s *Setup) scaffoldHandover(runCtx context.Context, work, prompt string, onActivity func(string)) error {
	held, err := setAside(work)
	if err != nil {
		return err
	}
	defer held.cleanup()

	if err := s.Agent(runCtx, work, prompt, onActivity); err != nil {
		return err
	}
	collided, err := held.restore(work)
	if err != nil {
		return err
	}
	if len(collided) > 0 {
		setupLog.Infof("%s wrote its own %s — asking it to merge slopball's copy in",
			s.harnessName(), strings.Join(collided, ", "))
		if err := s.Agent(runCtx, work, mergePrompt(collided), onActivity); err != nil {
			return err
		}
	}
	return held.verify(work)
}
