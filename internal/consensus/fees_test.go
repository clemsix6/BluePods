package consensus

import (
	"math"
	"testing"
)

func TestEffectiveRep(t *testing.T) {
	// Singleton (rep=0) → total_validators
	if got := effectiveRep(0, 100); got != 100 {
		t.Errorf("singleton: got %d, want 100", got)
	}

	// Standard object
	if got := effectiveRep(10, 100); got != 10 {
		t.Errorf("standard: got %d, want 10", got)
	}

	// Rep equals total_validators
	if got := effectiveRep(50, 50); got != 50 {
		t.Errorf("equal: got %d, want 50", got)
	}

	// Singleton with 1 validator
	if got := effectiveRep(0, 1); got != 1 {
		t.Errorf("singleton-1: got %d, want 1", got)
	}

	// Singleton with 0 validators (edge case)
	if got := effectiveRep(0, 0); got != 0 {
		t.Errorf("singleton-0: got %d, want 0", got)
	}
}

func TestReplicationRatio_CreatesObjects(t *testing.T) {
	// Creating objects forces ratio = 1/1
	num, denom := ReplicationRatio(nil, 1, 0, nil, 100)
	if num != 1 || denom != 1 {
		t.Errorf("creates objects: got %d/%d, want 1/1", num, denom)
	}
}

func TestReplicationRatio_CreatesDomains(t *testing.T) {
	// Creating domains forces ratio = 1/1
	num, denom := ReplicationRatio(nil, 0, 1, nil, 100)
	if num != 1 || denom != 1 {
		t.Errorf("creates domains: got %d/%d, want 1/1", num, denom)
	}
}

func TestReplicationRatio_Singleton(t *testing.T) {
	// Mutable singleton → ratio = 1/1
	refs := []ObjectRef{{Replication: 0}}
	num, denom := ReplicationRatio(refs, 0, 0, nil, 100)
	if num != 1 || denom != 1 {
		t.Errorf("singleton: got %d/%d, want 1/1", num, denom)
	}
}

func TestReplicationRatio_StandardObjects(t *testing.T) {
	// 2 objects with replication=10, each has 10 unique holders among 100 validators
	refs := []ObjectRef{
		{ID: [32]byte{1}, Replication: 10},
		{ID: [32]byte{2}, Replication: 10},
	}

	// Mock: each object has 10 unique holders (no overlap)
	computeHolders := func(id [32]byte, rep int) []Hash {
		var holders []Hash
		base := byte(0)
		if id[0] == 2 {
			base = 10
		}

		for i := 0; i < rep; i++ {
			var h Hash
			h[0] = base + byte(i)
			holders = append(holders, h)
		}

		return holders
	}

	num, denom := ReplicationRatio(refs, 0, 0, computeHolders, 100)
	// 20 unique holders out of 100
	if num != 20 || denom != 100 {
		t.Errorf("standard: got %d/%d, want 20/100", num, denom)
	}
}

func TestReplicationRatio_OverlappingHolders(t *testing.T) {
	refs := []ObjectRef{
		{ID: [32]byte{1}, Replication: 10},
		{ID: [32]byte{2}, Replication: 10},
	}

	// 5 holders overlap between the two objects
	computeHolders := func(id [32]byte, rep int) []Hash {
		var holders []Hash
		base := byte(0)
		if id[0] == 2 {
			base = 5 // overlaps with first 5 of object 1
		}

		for i := 0; i < rep; i++ {
			var h Hash
			h[0] = base + byte(i)
			holders = append(holders, h)
		}

		return holders
	}

	num, denom := ReplicationRatio(refs, 0, 0, computeHolders, 100)
	// 15 unique holders (10 + 10 - 5 overlap)
	if num != 15 || denom != 100 {
		t.Errorf("overlap: got %d/%d, want 15/100", num, denom)
	}
}

func TestReplicationRatio_NoMutableRefs(t *testing.T) {
	// Read-only tx: ratio = 0/1
	num, denom := ReplicationRatio(nil, 0, 0, nil, 100)
	if num != 0 || denom != 1 {
		t.Errorf("no mutable: got %d/%d, want 0/1", num, denom)
	}
}

func TestReplicationRatio_ZeroValidators(t *testing.T) {
	num, denom := ReplicationRatio(nil, 0, 0, nil, 0)
	if num != 0 || denom != 1 {
		t.Errorf("zero validators: got %d/%d, want 0/1", num, denom)
	}
}

func TestReplicationRatio_MixedSingletonStandard(t *testing.T) {
	// Mixed: one singleton + one standard → ratio = 1/1 (singleton dominates)
	refs := []ObjectRef{
		{Replication: 10},
		{Replication: 0},
	}

	num, denom := ReplicationRatio(refs, 0, 0, nil, 100)
	if num != 1 || denom != 1 {
		t.Errorf("mixed: got %d/%d, want 1/1", num, denom)
	}
}

func TestCalculateFee_FullFormula(t *testing.T) {
	params := FeeParams{
		GasPrice:   2,
		TransitFee: 10,
		StorageFee: 1000,
		DomainFee:  10000,
	}

	// max_gas=500, ratio=20/100, 3 standard objects, 2 created objects (rep=10,rep=0), 1 domain
	// compute = 500 * 2 * 20 / 100 = 200
	// transit = 3 * 10 = 30
	// storage[0] = 10 * 1000 / 100 = 100 (rep=10, 100 validators)
	// storage[1] = 100 * 1000 / 100 = 1000 (rep=0 → effective=100)
	// domain = 1 * 10000 = 10000
	// total = 200 + 30 + 100 + 1000 + 10000 = 11330
	fee := CalculateFee(500, 20, 100, 3, []uint16{10, 0}, 1, 100, params)
	if fee != 11330 {
		t.Errorf("full formula: got %d, want 11330", fee)
	}
}

func TestCalculateFee_ZeroMaxGas(t *testing.T) {
	params := FeeParams{
		GasPrice:   2,
		TransitFee: 10,
		StorageFee: 1000,
		DomainFee:  10000,
	}

	// No gas, but still transit + storage + domain
	// transit = 2 * 10 = 20
	// storage = 10 * 1000 / 50 = 200
	// domain = 1 * 10000 = 10000
	fee := CalculateFee(0, 1, 1, 2, []uint16{10}, 1, 50, params)
	if fee != 10220 {
		t.Errorf("zero gas: got %d, want 10220", fee)
	}
}

func TestCalculateFee_NoStandardObjects(t *testing.T) {
	params := FeeParams{
		GasPrice:   1,
		TransitFee: 10,
	}

	// Only compute, no transit
	fee := CalculateFee(100, 1, 1, 0, nil, 0, 100, params)
	if fee != 100 {
		t.Errorf("no standard: got %d, want 100", fee)
	}
}

func TestCalculateFee_NoCreatedObjects(t *testing.T) {
	params := FeeParams{
		GasPrice:   1,
		TransitFee: 10,
		StorageFee: 1000,
	}

	// compute + transit, no storage
	fee := CalculateFee(100, 1, 1, 5, nil, 0, 100, params)
	if fee != 150 {
		t.Errorf("no created: got %d, want 150", fee)
	}
}

func TestCalculateFee_NoDomains(t *testing.T) {
	params := FeeParams{
		GasPrice:  1,
		DomainFee: 10000,
	}

	fee := CalculateFee(100, 1, 1, 0, nil, 0, 100, params)
	if fee != 100 {
		t.Errorf("no domains: got %d, want 100", fee)
	}
}

func TestCalculateFee_ZeroValidators(t *testing.T) {
	params := FeeParams{
		GasPrice:   1,
		StorageFee: 1000,
	}

	// Zero validators → no storage fees, no panic
	fee := CalculateFee(100, 0, 1, 0, []uint16{10}, 0, 0, params)
	if fee != 0 {
		t.Errorf("zero validators: got %d, want 0", fee)
	}
}

func TestCalculateFee_ZeroRepDenom(t *testing.T) {
	params := FeeParams{GasPrice: 1}

	// Zero denom → no compute fee, no panic
	fee := CalculateFee(100, 1, 0, 0, nil, 0, 100, params)
	if fee != 0 {
		t.Errorf("zero denom: got %d, want 0", fee)
	}
}

func TestCalculateFee_LargeValues(t *testing.T) {
	params := FeeParams{GasPrice: 1}

	// Large max_gas that doesn't overflow: 10^18 * 1 * 1/1 = 10^18
	fee := CalculateFee(1_000_000_000_000_000_000, 1, 1, 0, nil, 0, 100, params)
	if fee != 1_000_000_000_000_000_000 {
		t.Errorf("large: got %d, want 1000000000000000000", fee)
	}
}

func TestSplitFee_DefaultRatios(t *testing.T) {
	params := FeeParams{
		AggregatorBPS: 2000,
		BurnBPS:       3000,
		EpochBPS:      5000,
	}

	split := SplitFee(10000, params)

	if split.Total != 10000 {
		t.Errorf("total: got %d, want 10000", split.Total)
	}
	if split.Aggregator != 2000 {
		t.Errorf("aggregator: got %d, want 2000", split.Aggregator)
	}
	if split.Burned != 3000 {
		t.Errorf("burned: got %d, want 3000", split.Burned)
	}
	if split.Epoch != 5000 {
		t.Errorf("epoch: got %d, want 5000", split.Epoch)
	}

	// Invariant: parts sum to total
	if split.Aggregator+split.Burned+split.Epoch != split.Total {
		t.Error("parts don't sum to total")
	}
}

func TestSplitFee_Rounding(t *testing.T) {
	params := FeeParams{
		AggregatorBPS: 2000,
		BurnBPS:       3000,
		EpochBPS:      5000,
	}

	// 999: aggregator=199, burned=299, epoch=999-199-299=501
	split := SplitFee(999, params)

	if split.Aggregator != 199 {
		t.Errorf("aggregator: got %d, want 199", split.Aggregator)
	}
	if split.Burned != 299 {
		t.Errorf("burned: got %d, want 299", split.Burned)
	}
	if split.Epoch != 501 {
		t.Errorf("epoch: got %d, want 501", split.Epoch)
	}

	// Invariant: parts always sum to total (remainder goes to epoch)
	if split.Aggregator+split.Burned+split.Epoch != split.Total {
		t.Error("parts don't sum to total after rounding")
	}
}

func TestSplitFee_Zero(t *testing.T) {
	params := DefaultFeeParams()
	split := SplitFee(0, params)

	if split.Total != 0 || split.Aggregator != 0 || split.Burned != 0 || split.Epoch != 0 {
		t.Error("zero split should be all zeros")
	}
}

func TestSplitFee_One(t *testing.T) {
	params := FeeParams{
		AggregatorBPS: 2000,
		BurnBPS:       3000,
		EpochBPS:      5000,
	}

	split := SplitFee(1, params)

	// 1*2000/10000=0, 1*3000/10000=0, epoch=1-0-0=1
	if split.Aggregator != 0 || split.Burned != 0 || split.Epoch != 1 {
		t.Errorf("split(1): agg=%d burn=%d epoch=%d", split.Aggregator, split.Burned, split.Epoch)
	}

	if split.Aggregator+split.Burned+split.Epoch != 1 {
		t.Error("parts don't sum to 1")
	}
}

func TestStorageDeposit(t *testing.T) {
	// Standard object: rep=10, 100 validators, fee=1000
	// deposit = 10 * 1000 / 100 = 100
	dep := StorageDeposit(10, 100, 1000)
	if dep != 100 {
		t.Errorf("standard: got %d, want 100", dep)
	}

	// Singleton: rep=0, effective=100, 100 validators
	// deposit = 100 * 1000 / 100 = 1000
	dep = StorageDeposit(0, 100, 1000)
	if dep != 1000 {
		t.Errorf("singleton: got %d, want 1000", dep)
	}

	// 1 validator: rep=10, deposit = 10 * 1000 / 1 = 10000
	dep = StorageDeposit(10, 1, 1000)
	if dep != 10000 {
		t.Errorf("1 validator: got %d, want 10000", dep)
	}

	// 0 validators: no panic, returns 0
	dep = StorageDeposit(10, 0, 1000)
	if dep != 0 {
		t.Errorf("0 validators: got %d, want 0", dep)
	}
}

func TestStorageRefund(t *testing.T) {
	params := FeeParams{StorageRefundBPS: 9500}

	// 1000 fees → 950 refund, 50 burned
	refund, burned := StorageRefund(1000, params)
	if refund != 950 || burned != 50 {
		t.Errorf("refund: got %d/%d, want 950/50", refund, burned)
	}

	// Sum invariant
	if refund+burned != 1000 {
		t.Error("refund + burned != original")
	}

	// Zero fees
	refund, burned = StorageRefund(0, params)
	if refund != 0 || burned != 0 {
		t.Errorf("zero: got %d/%d, want 0/0", refund, burned)
	}

	// Small amount: 1 fee → 0 refund, 1 burned
	refund, burned = StorageRefund(1, params)
	if refund != 0 || burned != 1 {
		t.Errorf("small: got %d/%d, want 0/1", refund, burned)
	}
}

func TestStorageRefund_LargeAmount(t *testing.T) {
	params := FeeParams{StorageRefundBPS: 9500}

	// Large but safe amount
	fees := uint64(math.MaxUint64 / bpsMax)
	refund, burned := StorageRefund(fees, params)

	if refund+burned != fees {
		t.Errorf("large: refund(%d) + burned(%d) != fees(%d)", refund, burned, fees)
	}
}

func TestCountStandardObjects(t *testing.T) {
	refs := []ObjectRef{
		{Replication: 10}, // standard
		{Replication: 0},  // singleton
		{Replication: 5},  // standard
		{Replication: 0},  // singleton
		{Replication: 1},  // standard
	}

	if got := CountStandardObjects(refs); got != 3 {
		t.Errorf("count: got %d, want 3", got)
	}

	// Empty
	if got := CountStandardObjects(nil); got != 0 {
		t.Errorf("empty: got %d, want 0", got)
	}

	// All singletons
	if got := CountStandardObjects([]ObjectRef{{Replication: 0}}); got != 0 {
		t.Errorf("all singletons: got %d, want 0", got)
	}
}

func TestSafeMul(t *testing.T) {
	// Normal multiplication
	if got := safeMul(10, 20); got != 200 {
		t.Errorf("normal: got %d, want 200", got)
	}

	// Zero
	if got := safeMul(0, 999); got != 0 {
		t.Errorf("zero a: got %d, want 0", got)
	}
	if got := safeMul(999, 0); got != 0 {
		t.Errorf("zero b: got %d, want 0", got)
	}

	// Overflow → MaxUint64
	if got := safeMul(math.MaxUint64, 2); got != math.MaxUint64 {
		t.Errorf("overflow: got %d, want MaxUint64", got)
	}

	// Large but not overflowing
	if got := safeMul(1<<32, 1<<31); got != 1<<63 {
		t.Errorf("large: got %d, want %d", got, uint64(1<<63))
	}

	// Just at overflow boundary
	if got := safeMul(1<<32, 1<<32); got != math.MaxUint64 {
		t.Errorf("boundary: got %d, want MaxUint64", got)
	}
}

func TestSafeAdd(t *testing.T) {
	// Normal addition
	if got := safeAdd(100, 200); got != 300 {
		t.Errorf("normal: got %d, want 300", got)
	}

	// Zero
	if got := safeAdd(0, 42); got != 42 {
		t.Errorf("zero: got %d, want 42", got)
	}

	// Overflow → MaxUint64
	if got := safeAdd(math.MaxUint64, 1); got != math.MaxUint64 {
		t.Errorf("overflow: got %d, want MaxUint64", got)
	}

	// Both MaxUint64
	if got := safeAdd(math.MaxUint64, math.MaxUint64); got != math.MaxUint64 {
		t.Errorf("double overflow: got %d, want MaxUint64", got)
	}

	// Near boundary (no overflow)
	if got := safeAdd(math.MaxUint64-1, 1); got != math.MaxUint64 {
		t.Errorf("near boundary: got %d, want MaxUint64", got)
	}
}

func TestCalculateFee_OverflowMaxGas(t *testing.T) {
	params := FeeParams{GasPrice: 2}

	// MaxUint64 * 2 would overflow without safeMul
	fee := CalculateFee(math.MaxUint64, 1, 1, 0, nil, 0, 100, params)

	// Should be capped at MaxUint64, not wrap to 0
	if fee == 0 {
		t.Fatal("fee should not be 0 (overflow wrapped)")
	}
	if fee != math.MaxUint64 {
		t.Errorf("expected MaxUint64, got %d", fee)
	}
}

func TestCalculateFee_OverflowStorageFee(t *testing.T) {
	params := FeeParams{StorageFee: math.MaxUint64}

	// Large storage fee with singleton (effRep=100 validators)
	fee := CalculateFee(0, 0, 1, 0, []uint16{0}, 0, 100, params)

	// effRep=100, storageFee=MaxUint64, 100 validators → MaxUint64*100/100 = MaxUint64
	// With safeMul, 100 * MaxUint64 → MaxUint64, then / 100 → still huge
	if fee == 0 {
		t.Fatal("fee should not be 0")
	}
}

func TestCalculateFee_AllComponentsOverflow(t *testing.T) {
	params := FeeParams{
		GasPrice:   math.MaxUint64,
		TransitFee: math.MaxUint64,
		StorageFee: math.MaxUint64,
		DomainFee:  math.MaxUint64,
	}

	// All components would overflow individually
	fee := CalculateFee(math.MaxUint64, 1, 1, 100, []uint16{0}, 100, 100, params)

	// Should be capped at MaxUint64, not some small wrapped value
	if fee != math.MaxUint64 {
		t.Errorf("expected MaxUint64 (saturated), got %d", fee)
	}
}

func TestDefaultFeeParams(t *testing.T) {
	p := DefaultFeeParams()

	// Ratios must sum to 100%
	if p.AggregatorBPS+p.BurnBPS+p.EpochBPS != bpsMax {
		t.Errorf("ratios sum to %d, want %d", p.AggregatorBPS+p.BurnBPS+p.EpochBPS, bpsMax)
	}

	// Refund ratio must be < 100%
	if p.StorageRefundBPS >= bpsMax {
		t.Errorf("refund ratio %d >= 100%%", p.StorageRefundBPS)
	}
}
