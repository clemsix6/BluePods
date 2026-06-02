package consensus

import "BluePods/internal/validators"

const (
	// quorumThreshold is the minimum percentage of validators required (67%).
	quorumThreshold = 67

	// channelBuffer is the buffer size for output channels.
	channelBuffer = 1024

	// registerValidatorFunc is the function name for validator registration.
	registerValidatorFunc = "register_validator"

	// deregisterValidatorFunc is the function name for voluntary validator departure.
	deregisterValidatorFunc = "deregister_validator"

	// bondFunc is the function name for bonding self-stake.
	bondFunc = "bond"

	// unbondFunc is the function name for withdrawing self-stake.
	unbondFunc = "unbond"

	// delegateFunc is the function name for delegating stake to a validator.
	delegateFunc = "delegate"

	// undelegateFunc is the function name for withdrawing a delegation.
	undelegateFunc = "undelegate"
)

// Hash is a 32-byte identifier for vertices and validators.
// It aliases validators.Hash so consensus code stays unchanged.
type Hash = validators.Hash

// ValidatorInfo holds information about a validator.
// It aliases validators.ValidatorInfo.
type ValidatorInfo = validators.ValidatorInfo

// ValidatorSet holds the active validators with their network addresses.
// It aliases validators.ValidatorSet.
type ValidatorSet = validators.ValidatorSet

// NewValidatorSet creates a validator set from a list of pubkeys.
// It forwards to validators.NewValidatorSet.
func NewValidatorSet(pubkeys []validators.Hash) *validators.ValidatorSet {
	return validators.NewValidatorSet(pubkeys)
}

// FailReason explains why a committed transaction did not apply. FailNone means
// the transaction executed successfully.
type FailReason uint8

const (
	FailNone    FailReason = 0 // FailNone means the transaction executed successfully
	FailVersion FailReason = 1 // FailVersion is a version conflict on a referenced object
	FailFee     FailReason = 2 // FailFee is a missing, unowned, or underfunded gas coin, or below min_gas
	FailOwner   FailReason = 3 // FailOwner is a mutable-ref ownership mismatch
	FailAuth    FailReason = 4 // FailAuth is a failed proof, signature, or hash check
	FailRevert  FailReason = 5 // FailRevert is a pod execution error
	FailExpired FailReason = 6 // FailExpired is an expired or unbounded sponsored transaction
)

// String returns a short human-readable label for the reason.
func (r FailReason) String() string {
	switch r {
	case FailNone:
		return "none"
	case FailVersion:
		return "version"
	case FailFee:
		return "fee"
	case FailOwner:
		return "owner"
	case FailAuth:
		return "auth"
	case FailRevert:
		return "revert"
	case FailExpired:
		return "expired"
	default:
		return "unknown"
	}
}

// CommittedTx represents a finalized transaction from the DAG.
type CommittedTx struct {
	Hash     Hash       // Hash is the transaction hash
	Success  bool       // Success is true if executed, false if it failed
	Reason   FailReason // Reason explains a failure; FailNone on success
	Function string     // Function is the function name (e.g., "register_validator")
	Sender   Hash       // Sender is the sender pubkey
}

// Broadcaster sends vertices to the network.
type Broadcaster interface {
	// Gossip sends data to a subset of peers who will relay it.
	Gossip(data []byte, fanout int) error
}
