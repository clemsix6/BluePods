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
