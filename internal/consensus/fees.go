package consensus

import (
	"math"
	"math/bits"
)

// FeeParams holds protocol-level fee constants.
// Initially hardcoded, later stored in a system singleton.
type FeeParams struct {
	GasPrice           uint64 // GasPrice is the price per unit of gas
	MinGas             uint64 // MinGas is the minimum gas per transaction (anti-spam)
	TransitFee         uint64 // TransitFee is the fixed fee per standard object in the ATX
	StorageFee         uint64 // StorageFee is the fixed fee per created object (flat 4 KB)
	DomainFee          uint64 // DomainFee is the fixed fee per registered domain
	AggregatorBPS      uint64 // AggregatorBPS is the aggregator share in basis points (2000 = 20%)
	BurnBPS            uint64 // BurnBPS is the burn share in basis points (3000 = 30%)
	EpochBPS           uint64 // EpochBPS is the epoch reward share in basis points (5000 = 50%)
	StorageRefundBPS   uint64 // StorageRefundBPS is the refund ratio on deletion in basis points (9500 = 95%)
}

// FeeSplit holds the breakdown of a fee into its three components.
type FeeSplit struct {
	Total      uint64 // Total is the full fee amount
	Aggregator uint64 // Aggregator is the aggregator share (20%)
	Burned     uint64 // Burned is the burned share (30%)
	Epoch      uint64 // Epoch is the epoch reward share (50%)
}

// DefaultFeeParams returns the default fee parameters.
// Values are placeholders until governance sets real ones.
func DefaultFeeParams() FeeParams {
	return FeeParams{
		GasPrice:         1,
		MinGas:           100,
		TransitFee:       10,
		StorageFee:       1000,
		DomainFee:        10000,
		AggregatorBPS:    2000,
		BurnBPS:          3000,
		EpochBPS:         5000,
		StorageRefundBPS: 9500,
	}
}

// bpsMax is the basis point denominator (100% = 10000).
const bpsMax = 10000

// safeMul returns a * b, capping at MaxUint64 on overflow.
// Prevents attackers from crafting large max_gas * gas_price that wraps to a small fee.
func safeMul(a, b uint64) uint64 {
	if a == 0 || b == 0 {
		return 0
	}

	hi, _ := bits.Mul64(a, b)
	if hi > 0 {
		return math.MaxUint64
	}

	return a * b
}

// safeAdd returns a + b, capping at MaxUint64 on overflow.
func safeAdd(a, b uint64) uint64 {
	sum := a + b
	if sum < a {
		return math.MaxUint64
	}

	return sum
}

// effectiveRep normalizes replication for fee formulas.
// Singleton (replication=0) means "replicated everywhere" → total_validators.
func effectiveRep(replication uint16, totalValidators int) int {
	if replication == 0 {
		return totalValidators
	}

	return int(replication)
}

// HolderFunc computes the set of holder pubkeys for an object.
type HolderFunc func(objectID [32]byte, replication int) []Hash

// ReplicationRatio computes the proportion of validators that execute the tx.
// Returns numerator and denominator to avoid floating-point arithmetic.
// If any mutable is singleton or tx creates objects/domains: ratio = 1/1.
func ReplicationRatio(
	mutableRefs []ObjectRef,
	createdObjectsCount int,
	maxCreateDomains int,
	computeHolders HolderFunc,
	totalValidators int,
) (num, denom int) {
	if totalValidators == 0 {
		return 0, 1
	}

	// Forces all validators: creating objects or domains
	if createdObjectsCount > 0 || maxCreateDomains > 0 {
		return 1, 1
	}

	// No mutable refs: ratio is 0 (read-only tx still pays transit)
	if len(mutableRefs) == 0 {
		return 0, 1
	}

	// Check for singletons: if any mutable is singleton, all validators execute
	for _, ref := range mutableRefs {
		if ref.Replication == 0 {
			return 1, 1
		}
	}

	// Compute union of holders across all mutable objects
	seen := make(map[Hash]struct{})

	for _, ref := range mutableRefs {
		holders := computeHolders(ref.ID, int(ref.Replication))
		for _, h := range holders {
			seen[h] = struct{}{}
		}
	}

	return len(seen), totalValidators
}

// ObjectRef holds minimal info needed for fee calculation.
type ObjectRef struct {
	ID          [32]byte // ID is the object identifier
	Replication uint16   // Replication is the object's replication factor
}

// CalculateFee computes the total fee for a transaction from its header fields.
// All arithmetic uses uint64 with careful ordering to avoid overflow and precision loss.
func CalculateFee(
	maxGas uint64,
	repNum, repDenom int,
	standardObjectCount int,
	createdObjectsReplication []uint16,
	maxCreateDomains int,
	totalValidators int,
	params FeeParams,
) uint64 {
	var total uint64

	// Compute fee: max_gas * gas_price * replication_ratio
	// Uses safeMul to prevent overflow (attacker could craft large max_gas * gas_price → wrap to 0)
	if repDenom > 0 && repNum > 0 {
		compute := safeMul(maxGas, params.GasPrice)
		compute = safeMul(compute, uint64(repNum)) / uint64(repDenom)
		total = safeAdd(total, compute)
	}

	// Transit fee: nb_standard_objects * transit_fee
	total = safeAdd(total, safeMul(uint64(standardObjectCount), params.TransitFee))

	// Storage fee: sum(effective_rep(replication_i) / total_validators) * storage_fee
	if totalValidators > 0 {
		for _, rep := range createdObjectsReplication {
			effRep := effectiveRep(rep, totalValidators)
			storage := safeMul(uint64(effRep), params.StorageFee) / uint64(totalValidators)
			total = safeAdd(total, storage)
		}
	}

	// Domain fee: max_create_domains * domain_fee
	total = safeAdd(total, safeMul(uint64(maxCreateDomains), params.DomainFee))

	return total
}

// SplitFee breaks a total fee into its three components.
// Uses integer division: aggregator + burned + epoch <= total.
// Any remainder (from rounding) is added to epoch.
func SplitFee(total uint64, params FeeParams) FeeSplit {
	aggregator := total * params.AggregatorBPS / bpsMax
	burned := total * params.BurnBPS / bpsMax
	epoch := total - aggregator - burned

	return FeeSplit{
		Total:      total,
		Aggregator: aggregator,
		Burned:     burned,
		Epoch:      epoch,
	}
}

// StorageDeposit computes the storage deposit for a newly created object.
// deposit = storage_fee * effective_rep(replication) / total_validators.
func StorageDeposit(replication uint16, totalValidators int, storageFee uint64) uint64 {
	if totalValidators == 0 {
		return 0
	}

	effRep := effectiveRep(replication, totalValidators)

	return uint64(effRep) * storageFee / uint64(totalValidators)
}

// StorageRefund computes the refund amount when an object is deleted.
// Returns refund (credited to owner) and burned (destroyed).
func StorageRefund(objectFees uint64, params FeeParams) (refund, burned uint64) {
	refund = objectFees * params.StorageRefundBPS / bpsMax
	burned = objectFees - refund

	return refund, burned
}

// CountStandardObjects counts non-singleton objects in a ref list.
// Standard objects (replication > 0) are in the ATX body and incur transit fees.
func CountStandardObjects(refs []ObjectRef) int {
	count := 0

	for _, ref := range refs {
		if ref.Replication > 0 {
			count++
		}
	}

	return count
}
