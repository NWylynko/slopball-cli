package git

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ulikunitz/xz"
)

// ExtractArchive unpacks a git-minimal-style tar.xz into dest. The archive's
// single top-level directory is stripped so dest ends up with cmd/, bin/,
// libexec/, share/ directly underneath. Exported for the extractor's own tests
// (the monorepo holds them); the only production caller is the embedded
// bundled-git archive.
func ExtractArchive(archive []byte, dest string) error {
	marker := filepath.Join(dest, ".slopball-git-ok")
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), "git-extract-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	xr, err := xz.NewReader(newBytesReader(archive))
	if err != nil {
		return fmt.Errorf("xz: %w", err)
	}
	if err := untarStripRoot(xr, tmp); err != nil {
		return err
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	for _, e := range entries {
		src := filepath.Join(tmp, e.Name())
		dst := filepath.Join(dest, e.Name())
		_ = os.RemoveAll(dst)
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	return os.WriteFile(marker, []byte(Version+"\n"), 0o644)
}

func untarStripRoot(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
	var root string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if name == "." || name == ".." {
			continue
		}
		parts := strings.SplitN(name, string(os.PathSeparator), 2)
		if root == "" {
			root = parts[0]
		}
		rel := name
		if strings.HasPrefix(name, root+string(os.PathSeparator)) {
			rel = strings.TrimPrefix(name, root+string(os.PathSeparator))
		} else if name == root {
			continue
		}
		if rel == "" || rel == "." {
			continue
		}
		target := filepath.Join(dest, rel)
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) && target != filepath.Clean(dest) {
			return fmt.Errorf("tar path escapes dest: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)|0o200)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			// Ensure executables stay executable after the |0o200 write bit.
			if err := os.Chmod(target, os.FileMode(hdr.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// The link's TARGET has to stay inside dest too — the path check
			// above covers where the link lives, not where it points.
			// Absolute targets and relative ones that climb past dest are both
			// refused; git-minimal's own links (`../../bin/git`) resolve inside.
			resolved := hdr.Linkname
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(target), resolved)
			}
			resolved = filepath.Clean(resolved)
			if !strings.HasPrefix(resolved, filepath.Clean(dest)+string(os.PathSeparator)) {
				return fmt.Errorf("tar symlink escapes dest: %s -> %s", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			// skip other types
		}
	}
}

type bytesReader struct {
	b []byte
	i int
}

func newBytesReader(b []byte) *bytesReader { return &bytesReader{b: b} }

func (r *bytesReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}
