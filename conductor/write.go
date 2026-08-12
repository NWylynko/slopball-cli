package conductor

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	sbGit "github.com/nwylynko/slopball-cli/git"
)

func writeFileOS(root, rel, body string) error {
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func readWorkFile(root, rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// conflictMarkerFiles lists tracked files in dir whose working-tree content
// carries git conflict markers. It anchors on the open/close markers
// (`<<<<<<< ` / `>>>>>>> `), which — unlike the `=======` separator — do not
// collide with markdown rules or ordinary source, so false positives are
// negligible. git grep exits non-zero when nothing matches; that error is
// expected and ignored (empty result).
func conflictMarkerFiles(ctx context.Context, dir string) []string {
	out, _ := sbGit.Output(ctx, dir, "grep", "-lI", "--no-color", "-e", "^<<<<<<< ", "-e", "^>>>>>>> ")
	seen := map[string]bool{}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !seen[line] {
			seen[line] = true
			files = append(files, line)
		}
	}
	return files
}

// stripConflictMarkers resolves conflict markers mechanically by keeping the
// incoming ("theirs") side — the block between `=======` and `>>>>>>> ` — and
// dropping our side. This mirrors the merger's documented incoming-change bias
// and is only the fallback when no harness is available to resolve properly.
func stripConflictMarkers(body string) string {
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	const (
		normal = iota
		ours
		theirs
	)
	state := normal
	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "<<<<<<< "):
			state = ours
		case state != normal && strings.HasPrefix(ln, "======="):
			state = theirs
		case state != normal && strings.HasPrefix(ln, ">>>>>>> "):
			state = normal
		case state == ours:
			// drop our side
		default:
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}
