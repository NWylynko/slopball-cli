package controlplane

import "github.com/nwylynko/slopball-cli/detect"

// CapFromProfile flattens a detect.Profile into the ranking subset published on members.
func CapFromProfile(p detect.Profile) *MemberCapability {
	s := p.DefaultStack()
	return &MemberCapability{
		Runtime: s.Runtime,
		Version: s.Version,
		PkgMgr:  s.PkgMgr,
		Score:   p.Score,
	}
}

// LocalGeneration reads the generation this machine last knew from cursors (0 if unknown).
func LocalGeneration(cursorsPath string, load func(string) struct{ Generation int }) int {
	if load == nil {
		return 0
	}
	return load(cursorsPath).Generation
}
