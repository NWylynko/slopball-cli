package scripts_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// `slopball update` fetches scripts/update.sh from the site and pipes it into
// sh, so this script is the whole update mechanism — the binary that runs it is
// the binary it replaces. Two properties are worth a test and nothing else is:
// it replaces the slopball you ALREADY HAVE (not a second one under
// ~/.local/bin, which would leave `slopball --version` reporting the old one
// forever), and it reuses install.sh rather than carrying a second copy of the
// download logic that can drift from the release names.
//
// The site is a real HTTP server here, serving the real scripts/install.sh, so
// what the test drives is the actual two-hop shape a user gets:
//
//	curl <site>/update.sh | sh   →   curl <site>/install.sh | sh
//
// with a fake `gh` and `uname` underneath, the same stubs installer_test.go
// uses. Nothing here reaches GitHub or the network.

// updateEnv is one sandboxed run of scripts/update.sh against a fake site.
type updateEnv struct {
	t *testing.T
	// dir is the sandbox root: dir/stub holds the fake uname+gh, dir/bin is
	// where the "already installed" slopball lives.
	dir  string
	site *httptest.Server
}

// newUpdateEnv lays down an already-installed slopball whose bytes name the
// version it is, so a test can tell a replaced binary from an untouched one.
func newUpdateEnv(t *testing.T, installed string) *updateEnv {
	t.Helper()
	env := &updateEnv{t: t, dir: t.TempDir()}
	for _, sub := range []string{"stub", "bin"} {
		if err := os.MkdirAll(filepath.Join(env.dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(t, filepath.Join(env.dir, "stub", "uname"), "#!/bin/sh\n"+
		"case \"$1\" in\n"+
		"  -s) echo Linux ;;\n"+
		"  -m) echo x86_64 ;;\n"+
		"  *) echo Linux ;;\n"+
		"esac\n")
	writeExec(t, filepath.Join(env.dir, "stub", "gh"), "#!/bin/sh\n"+
		"pattern=; dir=.\n"+
		"while [ $# -gt 0 ]; do\n"+
		"  case \"$1\" in\n"+
		"    --pattern) pattern=$2; shift 2 ;;\n"+
		"    --dir|-D) dir=$2; shift 2 ;;\n"+
		"    *) shift ;;\n"+
		"  esac\n"+
		"done\n"+
		"[ -n \"$pattern\" ] || exit 1\n"+
		"printf 'the new %s\\n' \"$pattern\" > \"$dir/$pattern\"\n")
	if installed != "" {
		writeExec(t, filepath.Join(env.dir, "bin", "slopball"), installed)
	}

	// The site: the real install.sh, served the way the worker serves it.
	root := repoRoot(t)
	env.site = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		body, err := os.ReadFile(filepath.Join(root, "scripts", name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/x-shellscript")
		_, _ = w.Write(body)
	}))
	t.Cleanup(env.site.Close)
	return env
}

// run executes scripts/update.sh with the sandbox on PATH. extraEnv is how a
// test drives the `slopball update` path, which passes the running binary's own
// directory rather than letting the script resolve one.
func (e *updateEnv) run(extraEnv ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command("sh", filepath.Join(repoRoot(e.t), "scripts", "update.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(e.dir, "stub")+string(os.PathListSeparator)+
			filepath.Join(e.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SLOPBALL_SITE="+e.site.URL,
		// So a script that ignored the resolved location and fell back to the
		// default would install somewhere this test can see rather than into
		// the developer's real ~/.local/bin.
		"HOME="+filepath.Join(e.dir, "home"),
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// installedBytes is what sits at the sandbox's slopball right now.
func (e *updateEnv) installedBytes() string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, "bin", "slopball"))
	if err != nil {
		e.t.Fatalf("nothing is installed at the sandbox's bin/slopball: %v", err)
	}
	return string(b)
}

// TestUpdateShReplacesTheSlopballAlreadyOnPath is the whole point of a separate
// updater: install.sh installs to ~/.local/bin, and a machine whose slopball
// lives anywhere else would end up with two of them and keep running the old
// one. The update has to land on the binary that is actually being used.
func TestUpdateShReplacesTheSlopballAlreadyOnPath(t *testing.T) {
	requireSh(t)
	requireCurl(t)
	env := newUpdateEnv(t, "#!/bin/sh\necho the old one\n")
	out, err := env.run()
	if err != nil {
		t.Fatalf("update.sh failed: %v\n%s", err, out)
	}
	if got := env.installedBytes(); !strings.Contains(got, "the new slopball-linux-amd64") {
		t.Fatalf("the slopball on PATH was not replaced — it is still %q\n%s", got, out)
	}
	// And nothing was installed beside it under the install.sh default.
	if _, err := os.Stat(filepath.Join(env.dir, "home", ".local", "bin", "slopball")); err == nil {
		t.Fatalf("update.sh installed a SECOND slopball under ~/.local/bin instead of replacing the one on PATH\n%s", out)
	}
}

// TestUpdateShPrefersTheDirectoryItIsGiven: `slopball update` resolves its own
// executable and hands that directory over, because a binary invoked by
// absolute path is not on PATH at all — and updating "the slopball on PATH"
// would then replace a different one, or none.
func TestUpdateShPrefersTheDirectoryItIsGiven(t *testing.T) {
	requireSh(t)
	requireCurl(t)
	env := newUpdateEnv(t, "#!/bin/sh\necho the old one\n")
	elsewhere := filepath.Join(env.dir, "opt")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := env.run("SLOPBALL_INSTALL_DIR=" + elsewhere)
	if err != nil {
		t.Fatalf("update.sh failed: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(elsewhere, "slopball"))
	if err != nil {
		t.Fatalf("update.sh ignored the directory it was given: %v\n%s", err, out)
	}
	if !strings.Contains(string(body), "the new slopball-linux-amd64") {
		t.Fatalf("the given directory got %q", body)
	}
	if got := env.installedBytes(); strings.Contains(got, "the new") {
		t.Fatalf("update.sh replaced the slopball on PATH as well as the one it was told about")
	}
}

// TestUpdateShRefusesWhenThereIsNothingToUpdate: an update that silently
// performs a first install would put slopball somewhere the user never chose,
// and report success for a machine that still has no slopball on PATH.
func TestUpdateShRefusesWhenThereIsNothingToUpdate(t *testing.T) {
	requireSh(t)
	requireCurl(t)
	env := newUpdateEnv(t, "")
	out, err := env.run()
	if err == nil {
		t.Fatalf("update.sh updated a machine with no slopball installed:\n%s", out)
	}
	if !strings.Contains(out, "install.sh") {
		t.Fatalf("the refusal must point at the installer instead:\n%s", out)
	}
}

// TestUpdateShFetchesTheInstallerRatherThanCopyingIt closes the loop the
// installer tests opened: three files agree on the asset name, and update.sh
// must not become a fourth that can disagree. It has no `uname` mapping of its
// own — the proof is that the script's text never names an asset or a platform.
func TestUpdateShFetchesTheInstallerRatherThanCopyingIt(t *testing.T) {
	body := readRepoFile(t, "scripts/update.sh")
	for _, forbidden := range []string{"darwin", "aarch64", "x86_64", "slopball-linux", "releases/latest"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("update.sh names %q — the platform mapping and the release lookup live in install.sh, and a second copy is what installer_test.go exists to prevent", forbidden)
		}
	}
	if !strings.Contains(body, "install.sh") {
		t.Fatalf("update.sh must run install.sh:\n%s", body)
	}
}

func requireCurl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl on this machine")
	}
}
