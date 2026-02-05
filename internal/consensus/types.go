package consensus

import "sync"

const (
	// quorumThreshold is the minimum percentage of validators required (67%).
	quorumThreshold = 67

	// channelBuffer is the buffer size for output channels.
	channelBuffer = 1024

	// registerValidatorFunc is the function name for validator registration.
	registerValidatorFunc = "register_validator"
)

// Hash is a 32-byte identifier for vertices and validators.
type Hash [32]byte

// ValidatorSet holds the active validators.
// It is safe for concurrent access.
type ValidatorSet struct {
	mu         sync.RWMutex
	validators []Hash // pubkeys of active validators
	index      map[Hash]int
}

// NewValidatorSet creates a validator set from a list of pubkeys.
func NewValidatorSet(pubkeys []Hash) *ValidatorSet {
	vs := &ValidatorSet{
		validators: make([]Hash, len(pubkeys)),
		index:      make(map[Hash]int, len(pubkeys)),
	}

	for i, pk := range pubkeys {
		vs.validators[i] = pk
		vs.index[pk] = i
	}

	return vs
}

// Add adds a validator to the set. Returns true if added, false if already exists.
func (vs *ValidatorSet) Add(pubkey Hash) bool {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	if _, exists := vs.index[pubkey]; exists {
		return false
	}

	vs.index[pubkey] = len(vs.validators)
	vs.validators = append(vs.validators, pubkey)

	return true
}

// Contains checks if a pubkey is in the validator set.
func (vs *ValidatorSet) Contains(pubkey Hash) bool {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	_, exists := vs.index[pubkey]
	return exists
}

// Len returns the number of validators.
func (vs *ValidatorSet) Len() int {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	return len(vs.validators)
}

// QuorumSize returns the minimum number of validators for quorum (67%).
func (vs *ValidatorSet) QuorumSize() int {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	return (len(vs.validators)*quorumThreshold + 99) / 100
}

// Validators returns a copy of all validator pubkeys.
func (vs *ValidatorSet) Validators() []Hash {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	result := make([]Hash, len(vs.validators))
	copy(result, vs.validators)

	return result
}

// Index returns the index of a validator in the set, or -1 if not found.
func (vs *ValidatorSet) Index(pubkey Hash) int {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	if idx, exists := vs.index[pubkey]; exists {
		return idx
	}

	return -1
}

// CommittedTx represents a finalized transaction from the DAG.
type CommittedTx struct {
	Hash    Hash // transaction hash
	Success bool // true if executed, false if version conflict
}

// Broadcaster sends vertices to the network.
type Broadcaster interface {
	// Gossip sends data to a subset of peers who will relay it.
	Gossip(data []byte, fanout int) error
}
