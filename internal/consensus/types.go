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

// CommittedTx represents a finalized transaction from the DAG.
type CommittedTx struct {
	Hash     Hash   // Hash is the transaction hash
	Success  bool   // Success is true if executed, false if version conflict
	Function string // Function is the function name (e.g., "register_validator")
	Sender   Hash   // Sender is the sender pubkey
}

// Broadcaster sends vertices to the network.
type Broadcaster interface {
	// Gossip sends data to a subset of peers who will relay it.
	Gossip(data []byte, fanout int) error
}
