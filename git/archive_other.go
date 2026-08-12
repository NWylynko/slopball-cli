//go:build !(linux && (amd64 || arm64))

package git

// No pinned static git archive for this GOOS/GOARCH yet. Resolve() still works
// if we add a darwin
// asset later).
var embeddedArchive []byte
