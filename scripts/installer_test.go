package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The GitHub Release is the only distribution channel there is (plan 47 step
// 10; homebrew was dropped, not deferred), so these tests own the contract
// between three files that must agree on one string: `make release` writes
// `dist/slopball-<os>-<arch>`, the workflow uploads exactly those files, and
// `scripts/install.sh` derives the same name from `uname`. A rename in any one
// of them breaks installing for everybody, and nothing else would notice.
//
// These live HERE, in the public client module, and that is deliberate (plan
// 49): "no tests are public" always meant "nothing needing cptest, a database
// or a credential". All three files this holds together are now in this repo,
// and a test that guards a contract from a repo that no longer contains either
// end of it is a tripwire nobody is standing on.
//
// The real suite is still the monorepo's. Nothing here proves the client works.

// repoRoot is this module's root — the directory holding the Makefile, the
// workflows and scripts/. These tests read all three, so they are anchored to
// it rather than to the working directory `go test` happens to pick.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestInstallShFetchesTheAssetForThisPlatform runs the installer for all four
// released platforms with a fake `uname` and a fake `gh`, which is the only way
// to prove the darwin half from linux. The fake gh records what was asked for,
// so the assertion is on the ASSET NAME rather than on the script's own echo.
func TestInstallShFetchesTheAssetForThisPlatform(t *testing.T) {
	requireSh(t)
	for _, tc := range []struct {
		unameS, unameM, want string
	}{
		{"Darwin", "arm64", "slopball-darwin-arm64"},
		{"Darwin", "x86_64", "slopball-darwin-amd64"},
		{"Linux", "aarch64", "slopball-linux-arm64"},
		{"Linux", "x86_64", "slopball-linux-amd64"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			env := newInstallEnv(t, tc.unameS, tc.unameM)
			out, err := env.run()
			if err != nil {
				t.Fatalf("install.sh failed: %v\n%s", err, out)
			}
			if got := env.asked(); got != tc.want {
				t.Fatalf("installer asked the release for %q, want %q\n%s", got, tc.want, out)
			}
			// Installed under the plain name, executable, and actually the bytes
			// that were downloaded rather than a placeholder.
			bin := filepath.Join(env.dir, "bin", "slopball")
			info, err := os.Stat(bin)
			if err != nil {
				t.Fatalf("nothing was installed: %v\n%s", err, out)
			}
			if info.Mode()&0o111 == 0 {
				t.Fatalf("installed binary is not executable: %v", info.Mode())
			}
			body, _ := os.ReadFile(bin)
			if !strings.Contains(string(body), tc.want) {
				t.Fatalf("installed the wrong asset: %q", body)
			}
		})
	}
}

// TestInstallShRefusesAPlatformThatHasNoAsset: four platforms are released, and
// a machine outside them must be told so rather than handed a 404 from curl or
// a file that will not exec.
func TestInstallShRefusesAPlatformThatHasNoAsset(t *testing.T) {
	requireSh(t)
	env := newInstallEnv(t, "Linux", "riscv64")
	out, err := env.run()
	if err == nil {
		t.Fatalf("install.sh accepted an unreleased architecture:\n%s", out)
	}
	if !strings.Contains(out, "riscv64") {
		t.Fatalf("the refusal must name the architecture it cannot serve:\n%s", out)
	}
}

// TestTheInstallerNamesTheFilesMakeReleaseWrites closes the loop: the names the
// installer asks for are the names the release matrix produces, taken from the
// Makefile rather than from a list copied into this test.
func TestTheInstallerNamesTheFilesMakeReleaseWrites(t *testing.T) {
	requireSh(t)
	// The matrix, from the Makefile rather than from a list copied in here —
	// adding a fifth platform there must make this test say something.
	built := map[string]bool{}
	for _, line := range strings.Split(readRepoFile(t, "Makefile"), "\n") {
		if !strings.HasPrefix(line, "PLATFORMS ") && !strings.HasPrefix(line, "PLATFORMS:=") && !strings.HasPrefix(line, "PLATFORMS :=") {
			continue
		}
		_, rhs, _ := strings.Cut(line, ":=")
		for _, p := range strings.Fields(rhs) {
			os, arch, _ := strings.Cut(p, "/")
			built["slopball-"+os+"-"+arch] = true
		}
	}
	if len(built) != 4 {
		t.Fatalf("expected four released platforms from the Makefile, got %v", built)
	}
	for _, tc := range []struct{ unameS, unameM string }{
		{"Darwin", "arm64"}, {"Darwin", "x86_64"}, {"Linux", "aarch64"}, {"Linux", "x86_64"},
	} {
		env := newInstallEnv(t, tc.unameS, tc.unameM)
		if out, err := env.run(); err != nil {
			t.Fatalf("install.sh failed: %v\n%s", err, out)
		}
		asked := env.asked()
		if !built[asked] {
			t.Fatalf("the installer asks for %q, which `make release` never writes (it writes %v)", asked, built)
		}
		delete(built, asked)
	}
	if len(built) != 0 {
		t.Fatalf("released platforms no uname maps to: %v", built)
	}
}

// TestTheReleaseWorkflowPublishesTheMatrixOnATag guards the three properties of
// the workflow that are not obvious from reading it, each of which fails
// silently: without full history `git describe` yields no tag and every binary
// reports 0.0.0-dev; without the command-line CONTROL_URL the release stamps
// whatever `.env` a runner happens to have (none, i.e. loopback); and without
// contents:write the upload 403s at the very end of a green build.
func TestTheReleaseWorkflowPublishesTheMatrixOnATag(t *testing.T) {
	wf := readRepoFile(t, ".github/workflows/release-cli.yml")
	for _, want := range []string{
		`- "v*"`,                      // the tag is the trigger
		"fetch-depth: 0",              // …and the tag is the version, which needs history
		"contents: write",             // the release is created by this workflow
		"CONTROL_URL=",                // the documented CI stamping path, beating any .env
		"dist/slopball-*",             // the assets are the files make release wrote
		"softprops/action-gh-release", // the release itself
	} {
		if !strings.Contains(wf, want) {
			t.Fatalf("release-cli.yml is missing %q:\n%s", want, wf)
		}
	}
	// A release must be cut for tags only — a push to main producing one would
	// mint a release per commit.
	if !strings.Contains(wf, "startsWith(github.ref, 'refs/tags/v')") {
		t.Fatalf("the release step must be gated on a tag ref:\n%s", wf)
	}
}

// TestTheBoxImageWorkflowFiresOnTheSameTag: the box image and the CLI that
// pulls it are one release. If they can drift, `pullImage`'s local-image
// fallback stays load-bearing forever, which is the thing tagging was meant to
// quiet.
func TestTheBoxImageWorkflowFiresOnTheSameTag(t *testing.T) {
	wf := readRepoFile(t, ".github/workflows/box-image.yml")
	if !strings.Contains(wf, `- "v*"`) {
		t.Fatalf("box-image.yml must fire on the same v* tag:\n%s", wf)
	}
}

// installEnv is one sandboxed run of scripts/install.sh: a PATH carrying a fake
// `uname` and a fake `gh`, and an install directory of its own.
type installEnv struct {
	t   *testing.T
	dir string
}

func newInstallEnv(t *testing.T, unameS, unameM string) *installEnv {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub")
	if err := os.MkdirAll(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeExec(t, filepath.Join(stub, "uname"), "#!/bin/sh\n"+
		"case \"$1\" in\n"+
		"  -s) echo "+unameS+" ;;\n"+
		"  -m) echo "+unameM+" ;;\n"+
		"  *) echo "+unameS+" ;;\n"+
		"esac\n")
	// The fake release: `gh release download … --pattern <asset> --dir <dir>`
	// writes a file whose contents name the asset, and records the pattern.
	writeExec(t, filepath.Join(stub, "gh"), "#!/bin/sh\n"+
		"pattern=; dir=.\n"+
		"while [ $# -gt 0 ]; do\n"+
		"  case \"$1\" in\n"+
		"    --pattern) pattern=$2; shift 2 ;;\n"+
		"    --dir|-D) dir=$2; shift 2 ;;\n"+
		"    *) shift ;;\n"+
		"  esac\n"+
		"done\n"+
		"[ -n \"$pattern\" ] || exit 1\n"+
		"printf '%s\\n' \"$pattern\" > "+filepath.Join(dir, "asked")+"\n"+
		"printf '%s\\n' \"$pattern\" > \"$dir/$pattern\"\n")
	return &installEnv{t: t, dir: dir}
}

func (e *installEnv) run() (string, error) {
	e.t.Helper()
	root := repoRoot(e.t)
	cmd := exec.Command("sh", filepath.Join(root, "scripts", "install.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(e.dir, "stub")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SLOPBALL_INSTALL_DIR="+filepath.Join(e.dir, "bin"),
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// asked is the asset name the installer requested from the release.
func (e *installEnv) asked() string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, "asked"))
	if err != nil {
		e.t.Fatalf("the installer never asked the release for anything: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func requireSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no POSIX sh on this machine")
	}
}
