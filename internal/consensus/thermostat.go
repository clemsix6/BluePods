package consensus

// thermostatParams holds the adaptive issuance control loop's parameters, all in
// integer units so the loop is exact and deterministic. The rate is denominated
// in epoch events (no clock); the annual figures the parameters approximate hold
// only against an assumed epoch pace until the oracle supplies real time.
type thermostatParams struct {
	// TargetLowMille is the low edge of the staking-ratio dead band, in per-mille
	// of total supply (250 = 25%). Below it the rate is raised.
	TargetLowMille uint64

	// TargetHighMille is the high edge of the staking-ratio dead band, in per-mille
	// of total supply (350 = 35%). Above it the rate is lowered.
	TargetHighMille uint64

	// FloorRateMicro is the minimum per-epoch issuance rate in millionths (a low
	// perpetual security floor, not zero).
	FloorRateMicro uint64

	// CeilingRateMicro is the maximum per-epoch issuance rate in millionths
	// (rarely reached).
	CeilingRateMicro uint64

	// GenesisRateMicro is the per-epoch issuance rate seeded at genesis (the
	// bootstrap incentive), in millionths.
	GenesisRateMicro uint64

	// StepCapMicro is the largest single-epoch change of the rate in millionths
	// (the anti-lurch limit).
	StepCapMicro uint64

	// AutoRestakeMille is the fraction of each reward auto-restaked by default, in
	// per-mille (relieves the spent-reward treadmill). Consumed by Batch 7.
	AutoRestakeMille uint64
}

// defaultThermostatParams returns the starting thermostat parameters. The band is
// conservative and errs low (targeting too low rests at low inflation; targeting
// too high saturates at the ceiling and dilutes forever). The per-epoch rates
// approximate ~1% floor / ~20% ceiling / ~8-10% genesis annual against an assumed
// epoch pace; all are governed and recalibrated once the oracle supplies time.
func defaultThermostatParams() thermostatParams {
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

// stakingRatioMille returns the staking ratio bonded/supply in per-mille
// (bonded*1000/supply). It is read on PRE-mint supply so issuance cannot lower its
// own denominator. A zero supply yields 0 (no division by zero). Uses safeMul.
func stakingRatioMille(bonded, supply uint64) uint64 {
	if supply == 0 {
		return 0
	}

	return safeMul(bonded, 1000) / supply
}

// adjustRate steps the per-epoch issuance rate toward the target staking band.
// Inside the dead band [TargetLow, TargetHigh] the rate holds (preventing
// oscillation); below the band it rises, above it falls, each by at most StepCap;
// the result is clamped to [Floor, Ceiling]. All integer arithmetic.
func adjustRate(rate, ratioMille uint64, p thermostatParams) uint64 {
	if ratioMille >= p.TargetLowMille && ratioMille <= p.TargetHighMille {
		return clampRate(rate, p)
	}

	if ratioMille < p.TargetLowMille {
		rate = safeAdd(rate, p.StepCapMicro)
	} else {
		rate = subFloor(rate, p.StepCapMicro)
	}

	return clampRate(rate, p)
}

// clampRate confines a rate to the thermostat's [Floor, Ceiling] bounds.
func clampRate(rate uint64, p thermostatParams) uint64 {
	if rate < p.FloorRateMicro {
		return p.FloorRateMicro
	}

	if rate > p.CeilingRateMicro {
		return p.CeilingRateMicro
	}

	return rate
}

// subFloor returns a - b, flooring at 0 instead of underflowing.
func subFloor(a, b uint64) uint64 {
	if b > a {
		return 0
	}

	return a - b
}
