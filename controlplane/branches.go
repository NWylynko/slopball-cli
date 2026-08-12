package controlplane

// TruncateBranchesAhead caps a branches-ahead list for publication (ticket 19).
// omitted is how many names did not fit past MaxBranchesAhead.
func TruncateBranchesAhead(names []string) (out []string, omitted int) {
	if len(names) > MaxBranchesAhead {
		omitted = len(names) - MaxBranchesAhead
		names = names[:MaxBranchesAhead]
	}
	out = make([]string, len(names))
	for i, n := range names {
		if len(n) > MaxBranchNameLen {
			n = n[:MaxBranchNameLen]
		}
		out[i] = n
	}
	return out, omitted
}
