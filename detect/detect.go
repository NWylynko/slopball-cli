// Package detect probes a machine for stack + host-capability (plan 13).
// Detection only — never installs toolchains.
package detect

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Stack is a detected language runtime + package manager pair.
type Stack struct {
	Runtime string // node, python, go, rust, static, unknown
	Version string
	PkgMgr  string // pnpm, npm, yarn, uv, pip, go, cargo, none
}

// Profile is a machine's capability as a potential host.
type Profile struct {
	Hostname   string  `json:"hostname"`
	GOOS       string  `json:"goos"`
	GOARCH     string  `json:"goarch"`
	NumCPU     int     `json:"numCpu"`
	MemMB      int     `json:"memMb"` // best-effort; 0 if unknown
	OnBattery  bool    `json:"onBattery"`
	Stacks     []Stack `json:"stacks"`
	HasRuntime bool    `json:"hasRuntime"` // project runtime present
	Score      int     `json:"score"`      // filled by Rank
}

// Probe inspects the current machine.
func Probe() Profile {
	p := Profile{
		GOOS:   runtime.GOOS,
		GOARCH: runtime.GOARCH,
		NumCPU: runtime.NumCPU(),
		MemMB:  memMB(),
	}
	if h, err := os.Hostname(); err == nil {
		p.Hostname = h
	}
	p.OnBattery = onBattery()
	p.Stacks = detectStacks()
	p.HasRuntime = len(p.Stacks) > 0
	return p
}

// DefaultStack picks the best default for the host-start tap (§9.3).
func (p Profile) DefaultStack() Stack {
	prefer := []string{"node", "python", "go", "rust"}
	byRT := map[string]Stack{}
	for _, s := range p.Stacks {
		if _, ok := byRT[s.Runtime]; !ok {
			byRT[s.Runtime] = s
		}
	}
	for _, rt := range prefer {
		if s, ok := byRT[rt]; ok {
			return s
		}
	}
	if len(p.Stacks) > 0 {
		return p.Stacks[0]
	}
	return Stack{Runtime: "static", PkgMgr: "none"}
}

// Rank orders profiles best-host-first. Prefers AC power + existing runtime + RAM/CPU.
func Rank(profiles []Profile) []Profile {
	out := make([]Profile, len(profiles))
	copy(out, profiles)
	for i := range out {
		out[i].Score = score(out[i])
	}
	// simple insertion sort by score desc
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Score > out[j-1].Score; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func score(p Profile) int {
	s := p.NumCPU*10 + p.MemMB/256
	if p.HasRuntime {
		s += 100
	}
	if !p.OnBattery {
		s += 50
	}
	if p.MemMB >= 8192 {
		s += 20
	}
	return s
}

func detectStacks() []Stack {
	var out []Stack
	if v, ok := whichVersion("node", "--version"); ok {
		pm := "npm"
		if _, err := exec.LookPath("pnpm"); err == nil {
			pm = "pnpm"
		} else if _, err := exec.LookPath("yarn"); err == nil {
			pm = "yarn"
		}
		out = append(out, Stack{Runtime: "node", Version: v, PkgMgr: pm})
	}
	if v, ok := whichVersion("python3", "--version"); ok {
		pm := "pip"
		if _, err := exec.LookPath("uv"); err == nil {
			pm = "uv"
		}
		out = append(out, Stack{Runtime: "python", Version: strings.TrimPrefix(v, "Python "), PkgMgr: pm})
	}
	if v, ok := whichVersion("go", "version"); ok {
		out = append(out, Stack{Runtime: "go", Version: v, PkgMgr: "go"})
	}
	if v, ok := whichVersion("rustc", "--version"); ok {
		out = append(out, Stack{Runtime: "rust", Version: v, PkgMgr: "cargo"})
	}
	return out
}

func whichVersion(bin string, args ...string) (string, bool) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return "", false
	}
	_ = path
	cmd := exec.Command(bin, args...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", true // present but weird
	}
	return strings.TrimSpace(string(b)), true
}

func memMB() int {
	// Linux: MemTotal from /proc/meminfo
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.Atoi(fields[1])
				return kb / 1024
			}
		}
	}
	return 0
}

func onBattery() bool {
	// Linux sysfs; unknown → assume AC (don't punish).
	b, err := os.ReadFile("/sys/class/power_supply/BAT0/status")
	if err != nil {
		return false
	}
	s := strings.TrimSpace(string(b))
	return s == "Discharging"
}
