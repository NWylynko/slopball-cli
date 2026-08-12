package controlplane

import (
	"fmt"
	"strconv"
	"strings"
)

// Budget ceiling (abuse-surface ticket 24 / plan 45b).
//
// One configured number — concurrent managed boxes this control plane is
// willing to run at once — so the quotas below derive from it rather than
// being invented independently. Required at boot (ADR 0006): unset is a
// refusal, not today's soft defaults.

const (
	// BudgetUnit is the human unit logged next to the resolved ceiling.
	BudgetUnit = "concurrent boxes"

	// BudgetEnv is the configuration key. Empty / unset is a boot refusal.
	BudgetEnv = "SLOPBALL_BUDGET_CEILING"

	// Historical defaults when a ceiling was unset — kept so ResolveBudget's
	// unconfigured path and comments still name the same numbers. The shipped
	// binary never takes that path (ticket 05).
	BudgetDefaultBoxConcurrent   = 8
	BudgetDefaultHeldConnections = 256

	// HeldConnectionsPerBoxSlot is how HeldConnectionGlobalMax is derived
	// from the ceiling: held = ceiling × this.
	HeldConnectionsPerBoxSlot = BudgetDefaultHeldConnections / BudgetDefaultBoxConcurrent // 32
)

// Budget is the resolved ceiling and the bounds derived from it.
type Budget struct {
	// Ceiling is the configured concurrent-box limit. Zero with Unset true
	// means "no ceiling configured" — not "zero boxes".
	Ceiling int
	// Unset is true when SLOPBALL_BUDGET_CEILING was absent or blank.
	Unset bool
	// BoxConcurrent is the global concurrent box provision ceiling.
	BoxConcurrent int
	// HeldConnections is the global held-connection ceiling (redeem + SSE).
	HeldConnections int
}

// ResolveBudget derives box and held-connection ceilings from a concurrent-box
// budget. ceiling < 0 is invalid; ceiling == 0 with configured false means unset
// (today's defaults), while configured true and ceiling 0 means zero box slots.
func ResolveBudget(ceiling int, configured bool) (Budget, error) {
	if configured && ceiling < 0 {
		return Budget{}, fmt.Errorf("%s must be >= 0 (got %d)", BudgetEnv, ceiling)
	}
	if !configured {
		return Budget{
			Unset:           true,
			BoxConcurrent:   BudgetDefaultBoxConcurrent,
			HeldConnections: BudgetDefaultHeldConnections,
		}, nil
	}
	return Budget{
		Ceiling:         ceiling,
		BoxConcurrent:   ceiling,
		HeldConnections: ceiling * HeldConnectionsPerBoxSlot,
	}, nil
}

// ParseBudgetEnv reads SLOPBALL_BUDGET_CEILING. Empty is a refusal — the
// control plane must state what it may spend (ticket 05).
func ParseBudgetEnv(v string) (Budget, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return Budget{}, fmt.Errorf("%s is unset — set it to max concurrent managed boxes", BudgetEnv)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return Budget{}, fmt.Errorf("%s: %w", BudgetEnv, err)
	}
	return ResolveBudget(n, true)
}

// LogLine is what the control plane prints at startup — ceiling, unit, and
// the derived bounds so an operator can see the consequences.
func (b Budget) LogLine() string {
	if b.Unset {
		return fmt.Sprintf("budget ceiling unset — today's defaults (%s): box concurrent %d, held connections %d",
			BudgetUnit, b.BoxConcurrent, b.HeldConnections)
	}
	return fmt.Sprintf("budget ceiling %d %s → box concurrent %d, held connections %d (×%d per box slot)",
		b.Ceiling, BudgetUnit, b.BoxConcurrent, b.HeldConnections, HeldConnectionsPerBoxSlot)
}
