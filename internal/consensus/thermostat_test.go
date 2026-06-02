package consensus

import "testing"

// testThermostatParams returns small, exact parameters for deterministic math
// tests: a 250..350 per-mille dead band, a 2-millionths step cap, clamped to
// [2, 40] millionths.
func testThermostatParams() thermostatParams {
	return thermostatParams{
		TargetLowMille:   250,
		TargetHighMille:  350,
		FloorRateMicro:   2,
		CeilingRateMicro: 40,
		GenesisRateMicro: 18,
		StepCapMicro:     2,
		AutoRestakeMille: 200,
	}
}

// TestStakingRatioMille checks the staking ratio is bonded*1000/supply, floors at
// 0 when supply is 0, and never panics on overflow-prone inputs.
func TestStakingRatioMille(t *testing.T) {
	cases := []struct {
		name   string
		bonded uint64
		supply uint64
		want   uint64
	}{
		{"thirty percent", 300, 1000, 300},
		{"zero bonded", 0, 1000, 0},
		{"zero supply guards", 500, 0, 0},
		{"full", 1000, 1000, 1000},
		{"truncates down", 1, 3, 333},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stakingRatioMille(c.bonded, c.supply)
			if got != c.want {
				t.Errorf("stakingRatioMille(%d, %d) = %d, want %d", c.bonded, c.supply, got, c.want)
			}
		})
	}
}

// TestComputeIssuance checks issuanceFor returns rate*supply/1_000_000 and is 0
// when the rate is 0, computed against the pre-mint supply.
func TestComputeIssuance(t *testing.T) {
	cases := []struct {
		name   string
		rate   uint64
		supply uint64
		want   uint64
	}{
		{"zero rate mints nothing", 0, 1_000_000, 0},
		{"ten percent annual approx", 18, 1_000_000, 18},
		{"one millionth of a million", 1, 1_000_000, 1},
		{"large supply", 40, 1_000_000_000, 40_000},
		{"truncates down", 1, 1_500_000, 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := issuanceFor(c.rate, c.supply)
			if got != c.want {
				t.Errorf("issuanceFor(%d, %d) = %d, want %d", c.rate, c.supply, got, c.want)
			}
		})
	}
}

// TestAdjustRate checks the dead-band hold, the step cap, and the floor/ceiling
// clamps. Below the band the rate rises by at most stepCap (and is clamped to the
// ceiling); above the band it lowers (clamped to the floor); inside it holds.
func TestAdjustRate(t *testing.T) {
	p := testThermostatParams()

	// Inside the dead band (250..350): hold exactly.
	if got := adjustRate(18, 300, p); got != 18 {
		t.Errorf("inside band should hold: got %d, want 18", got)
	}

	// Below the band: raise by at most the step cap (2).
	if got := adjustRate(18, 100, p); got != 20 {
		t.Errorf("below band should rise by step cap: got %d, want 20", got)
	}

	// Below the band but near the ceiling: clamp to the ceiling (40), never above.
	if got := adjustRate(39, 100, p); got != 40 {
		t.Errorf("below band near ceiling should clamp: got %d, want 40", got)
	}

	// Above the band: lower by at most the step cap (2).
	if got := adjustRate(18, 500, p); got != 16 {
		t.Errorf("above band should lower by step cap: got %d, want 16", got)
	}

	// Above the band near the floor: clamp to the floor (2), never below.
	if got := adjustRate(3, 500, p); got != 2 {
		t.Errorf("above band near floor should clamp: got %d, want 2", got)
	}

	// At the exact low edge of the band: still inside, hold.
	if got := adjustRate(18, 250, p); got != 18 {
		t.Errorf("low band edge should hold: got %d, want 18", got)
	}

	// At the exact high edge of the band: still inside, hold.
	if got := adjustRate(18, 350, p); got != 18 {
		t.Errorf("high band edge should hold: got %d, want 18", got)
	}
}
