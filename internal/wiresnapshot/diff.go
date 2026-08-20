package wiresnapshot

import (
	"fmt"
	"strings"
)

// maxDiffLines caps the drift report. A whole-file rewrite is a real case (a
// generator change), and a 400-line failure buries the instructions that are
// the actual point of the message.
const maxDiffLines = 60

// diffContext is how many unchanged lines ride along either side of a change.
// Not decoration: the snapshot lists a struct's fields one per line, so a bare
// `+ Version string` names a field and not the TYPE it was added to, and the
// type is the half that decides whether an old client cares.
const diffContext = 3

// WireSurfaceDiff renders committed → current as a unified-ish line diff, with
// `-` for what the committed snapshot says and `+` for what this tree says.
// Written here rather than shelled out to `diff` because the tripwire's failure
// text has to be identical on every machine that runs the suite.
func WireSurfaceDiff(committed, current string) string {
	old := strings.Split(strings.TrimRight(committed, "\n"), "\n")
	new := strings.Split(strings.TrimRight(current, "\n"), "\n")
	common := longestCommonLines(old, new)

	// The full edit script first; the context window is then cut from it.
	var script []string
	changed := map[int]bool{}
	mark := func(prefix, line string) {
		if prefix != "  " {
			changed[len(script)] = true
		}
		script = append(script, prefix+line)
	}
	i, j := 0, 0
	for _, line := range common {
		for i < len(old) && old[i] != line {
			mark("- ", old[i])
			i++
		}
		for j < len(new) && new[j] != line {
			mark("+ ", new[j])
			j++
		}
		mark("  ", line)
		i++
		j++
	}
	for ; i < len(old); i++ {
		mark("- ", old[i])
	}
	for ; j < len(new); j++ {
		mark("+ ", new[j])
	}

	keep := map[int]bool{}
	for idx := range changed {
		for k := idx - diffContext; k <= idx+diffContext; k++ {
			if k >= 0 && k < len(script) {
				keep[k] = true
			}
		}
	}
	// Each hunk is announced with the type (or wire section) it sits in, for
	// the same reason: three lines of context name the neighbours, not the
	// owner, and the owner is what an old client decodes.
	var out []string
	gap := true
	anchor := ""
	for idx, line := range script {
		content := line[2:]
		isAnchor := strings.HasPrefix(content, "type ") || strings.HasPrefix(content, "## ")
		if isAnchor {
			anchor = content
		}
		if !keep[idx] {
			gap = true
			continue
		}
		if gap && !isAnchor && anchor != "" {
			out = append(out, "  … in "+anchor)
		} else if gap && len(out) > 0 {
			out = append(out, "  …")
		}
		gap = false
		out = append(out, line)
	}

	if len(out) > maxDiffLines {
		hidden := len(out) - maxDiffLines
		out = append(out[:maxDiffLines], fmt.Sprintf(
			"  … and %d more changed lines. For the whole picture: run `%s`, then `git diff %s` — but classify what you MEANT to change, not what the diff shows.",
			hidden, RegenerateCommand, SnapshotPath))
	}
	return strings.Join(out, "\n")
}

// longestCommonLines is the LCS of two line slices. The snapshot is a few
// hundred lines, so the quadratic table costs nothing and buys a diff that
// reads like a diff instead of a delete-everything/add-everything pair.
func longestCommonLines(a, b []string) []string {
	table := make([][]int, len(a)+1)
	for i := range table {
		table[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				table[i][j] = table[i+1][j+1] + 1
				continue
			}
			table[i][j] = max(table[i+1][j], table[i][j+1])
		}
	}
	var out []string
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			i++
		default:
			j++
		}
	}
	return out
}
