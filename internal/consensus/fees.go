package consensus

import (
	"math"
	"math/bits"

	"BluePods/internal/genesis"
)

// FeeParams holds protocol-level fee constants.
// Initially hardcoded, later stored in a system singleton.
type FeeParams struct {
	GasPrice           uint64 // GasPrice is the price per unit of gas
	MinGas             uint64 // MinGas is the minimum gas per transaction (anti-spam), and the flat compute a declared-operation transaction pays for running no metered code
	TransitFee         uint64 // TransitFee is the fixed fee per standard object in the ATX
	StorageFee         uint64 // StorageFee is the fixed fee per created object (flat 4 KB)
	RentalRatePerEpoch uint64 // RentalRatePerEpoch is what one epoch of a domain lease costs; a register or renew pays it times the term declared in the header
	MaxTermEpochs      uint64 // MaxTermEpochs caps how far past the current epoch a lease may run; an operation whose term would exceed it reverts rather than being clamped, because the rent charged is the rate times the DECLARED term
	GraceEpochs        uint64 // GraceEpochs is how many epochs past expiry a lease stays in the registry, reserving its owner's renewal right until the boundary sweep removes it
	ReparentFee        uint64 // ReparentFee is the flat fee a declared reparent pays for the tracker edge every node rewrites
	DeleteFee          uint64 // DeleteFee is the flat fee a declared delete pays for the tracker and index entries every node removes
	IndexEntryFee      uint64 // IndexEntryFee is the flat term an object's hierarchy-index entry adds to its creation deposit
	BurnBPS            uint64 // BurnBPS is the scarcity burn share in basis points (0 = no burn; against the stability goal)
	StorageRefundBPS   uint64 // StorageRefundBPS is the refund ratio on deletion in basis points (9500 = 95%)
}

// FeeSplit holds the breakdown of a consumed fee into its two components.
type FeeSplit struct {
	Total  uint64 // Total is the full consumed fee amount
	Burned uint64 // Burned is the scarcity burn share (vestigial, 0: the scarcity burn is removed)
	Epoch  uint64 // Epoch is the epoch reward share (100% of consumed fees)
}

// defaultMaxTermEpochs is the lease cap DefaultFeeParams carries, and the
// fallback a DAG with no fee system wired prices leases against. It is never 0:
// a zero cap reverts every lease, which a fee-less test or bootstrap node has no
// reason to do.
const defaultMaxTermEpochs uint64 = 256

// defaultGraceEpochs is the post-expiry grace window DefaultFeeParams
// carries, and the fallback the epoch boundary's sweep uses on a DAG with no
// fee system wired. Never 0: a zero window would sweep every lease the
// instant it expired, forfeiting the owner's exclusive renewal right the
// window exists to give.
const defaultGraceEpochs uint64 = 8

// DefaultFeeParams returns the default fee parameters.
// Values are placeholders until governance sets real ones.
func DefaultFeeParams() FeeParams {
	return FeeParams{
		GasPrice:           1,
		MinGas:             100,
		TransitFee:         10,
		StorageFee:         1000,
		RentalRatePerEpoch: 100,
		MaxTermEpochs:      defaultMaxTermEpochs,
		GraceEpochs:        defaultGraceEpochs,
		ReparentFee:        100,
		DeleteFee:          100,
		IndexEntryFee:      25,
		BurnBPS:            0,
		StorageRefundBPS:   9500,
	}
}

// bpsMax is the basis point denominator (100% = 10000).
const bpsMax = 10000

// milleMax is the per-mille denominator (100% = 1000), used for the auto-restake
// fraction of an epoch reward.
const milleMax = 1000

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
// If any mutable is singleton or tx creates objects: ratio = 1/1. Domain
// creation used to force the same (a pod-executed registration needed every
// node to observe it), but the pod domain write path is retired: domain
// registration is a declared operation, priced by declaredOpsFee instead of
// this ratio, so no term for it remains here.
func ReplicationRatio(
	mutableRefs []ObjectRef,
	createdObjectsCount int,
	computeHolders HolderFunc,
	totalValidators int,
) (num, denom int) {
	if totalValidators == 0 {
		return 0, 1
	}

	// Forces all validators: creating objects (holder unknown until after execution).
	if createdObjectsCount > 0 {
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
// Every term is derivable from the header alone, which is what lets each of the
// four fee sites — ingress, commit, summary production and summary validation —
// reach the same number for the same transaction without consulting state.
// opsOnly marks a transaction that declares operations and carries no pod call:
// it runs no metered code, so it pays a flat min_gas compute term instead of
// its declared max_gas, and its operations are priced individually below.
// All arithmetic uses uint64 with careful ordering to avoid overflow and precision loss.
func CalculateFee(
	maxGas uint64,
	repNum, repDenom int,
	standardObjectCount int,
	createdObjectsReplication []uint16,
	ops []genesis.DeclaredOp,
	opsOnly bool,
	totalValidators int,
	params FeeParams,
) uint64 {
	var total uint64

	// Compute fee: max_gas * gas_price * replication_ratio, or the flat min_gas
	// floor for a transaction whose only work is its declared operations.
	// Uses safeMul to prevent overflow (attacker could craft large max_gas * gas_price → wrap to 0)
	switch {
	case opsOnly:
		total = safeAdd(total, safeMul(params.MinGas, params.GasPrice))

	case repDenom > 0 && repNum > 0:
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

	// Declared-operation fees: flat per operation, rent for a lease. Domain
	// registration used to add a flat max_create_domains * domain_fee term here
	// (the pod path priced its declared intent to register); that path is
	// retired, and a domain lease is now priced individually below, by its
	// declared term, as one of these operation fees.
	total = safeAdd(total, declaredOpsFee(ops, params))

	return total
}

// declaredOpsFee prices a transaction's declared operations. They are charged
// whether or not they apply: the fee is fixed at ingress from the header, long
// before commit decides whether an operation is valid, exactly as a reverted
// pod call still pays the gas it declared. Making the charge conditional on the
// outcome would put the summary out of reach of the nodes that must recompute
// it, which is the whole reason the fee is header-derived.
func declaredOpsFee(ops []genesis.DeclaredOp, params FeeParams) uint64 {
	var total uint64

	for i := range ops {
		total = safeAdd(total, declaredOpFee(ops[i], params))
	}

	return total
}

// declaredOpFee prices one declared operation: a flat fee for the two that grow
// global state by a fixed amount, rate x the DECLARED term for the two that buy
// a lease. The rent is read from the header's term, never from the expiry the
// operation would produce, which is why a term past the cap reverts instead of
// being clamped. The remaining domain kinds rewrite or shrink a leaf that
// already exists and pay the transaction's min_gas floor only, as does an
// unknown kind, which commit rejects outright.
func declaredOpFee(op genesis.DeclaredOp, params FeeParams) uint64 {
	switch op.Kind {
	case reparentOp:
		return params.ReparentFee

	case deleteOp:
		return params.DeleteFee

	case domainRegisterOp, domainRenewOp:
		return safeMul(params.RentalRatePerEpoch, uint64(op.TermEpochs))

	default:
		return 0
	}
}

// SplitFee breaks a total fee into its two components.
// Uses integer division: burned + epoch <= total.
// Any remainder (from rounding) is added to epoch.
func SplitFee(total uint64, params FeeParams) FeeSplit {
	burned := total * params.BurnBPS / bpsMax
	epoch := total - burned

	return FeeSplit{
		Total:  total,
		Burned: burned,
		Epoch:  epoch,
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
