package consensus

import "sync"

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
type Hash [32]byte

// ValidatorInfo holds information about a validator.
type ValidatorInfo struct {
	Pubkey    Hash     // Pubkey is the validator's Ed25519 public key
	HTTPAddr  string   // HTTPAddr is the HTTP API endpoint (may be empty)
	QUICAddr  string   // QUICAddr is the QUIC P2P endpoint (may be empty)
	BLSPubkey [48]byte // BLSPubkey is the validator's BLS public key for attestation signing
}

// ValidatorSet holds the active validators with their network addresses.
// It is safe for concurrent access.
type ValidatorSet struct {
	mu         sync.RWMutex
	validators []*ValidatorInfo     // validators in order of addition
	index      map[Hash]int         // pubkey -> index in validators slice
	onAdd      func(*ValidatorInfo) // onAdd callback when validator is added
}

// NewValidatorSet creates a validator set from a list of pubkeys.
// Validators added this way have no network addresses.
func NewValidatorSet(pubkeys []Hash) *ValidatorSet {
	vs := &ValidatorSet{
		validators: make([]*ValidatorInfo, len(pubkeys)),
		index:      make(map[Hash]int, len(pubkeys)),
	}

	for i, pk := range pubkeys {
		vs.validators[i] = &ValidatorInfo{Pubkey: pk}
		vs.index[pk] = i
	}

	return vs
}

// OnAdd sets a callback that is called when a new validator is added.
// The callback receives the validator info including network addresses.
func (vs *ValidatorSet) OnAdd(fn func(*ValidatorInfo)) {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	vs.onAdd = fn
}

// Add adds a validator with network addresses and BLS public key.
// Returns true if added or updated, false if already exists with same addresses.
// If the validator exists but had empty addresses, the addresses are updated.
// If the validator exists but had zero BLS key, the BLS key is updated.
// The callback (if set) is called outside the lock only for new validators.
func (vs *ValidatorSet) Add(pubkey Hash, httpAddr, quicAddr string, blsPubkey [48]byte) bool {
	vs.mu.Lock()

	if idx, exists := vs.index[pubkey]; exists {
		// Validator exists - check if we need to update fields
		existing := vs.validators[idx]
		if existing.HTTPAddr == "" && httpAddr != "" {
			existing.HTTPAddr = httpAddr
		}
		if existing.QUICAddr == "" && quicAddr != "" {
			existing.QUICAddr = quicAddr
		}
		if existing.BLSPubkey == [48]byte{} && blsPubkey != [48]byte{} {
			existing.BLSPubkey = blsPubkey
		}
		vs.mu.Unlock()
		return false // Not a new validator
	}

	info := &ValidatorInfo{
		Pubkey:    pubkey,
		HTTPAddr:  httpAddr,
		QUICAddr:  quicAddr,
		BLSPubkey: blsPubkey,
	}

	vs.index[pubkey] = len(vs.validators)
	vs.validators = append(vs.validators, info)

	callback := vs.onAdd
	vs.mu.Unlock()

	// Call callback outside lock to avoid deadlocks
	if callback != nil {
		callback(info)
	}

	return true
}

// Remove removes a validator from the set.
// Returns true if the validator was found and removed.
func (vs *ValidatorSet) Remove(pubkey Hash) bool {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	idx, exists := vs.index[pubkey]
	if !exists {
		return false
	}

	// Remove from slice by swapping with last element
	last := len(vs.validators) - 1
	if idx != last {
		vs.validators[idx] = vs.validators[last]
		vs.index[vs.validators[idx].Pubkey] = idx
	}

	vs.validators = vs.validators[:last]
	delete(vs.index, pubkey)

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

// QuorumSize returns the minimum number of validators for quorum.
// Requires all validators to participate, ensuring full synchronization.
func (vs *ValidatorSet) QuorumSize() int {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	n := len(vs.validators)
	if n < 1 {
		return 1
	}

	// BFT quorum: 2n/3 + 1 (tolerates up to n/3 failures)
	return (2*n)/3 + 1
}

// Validators returns a copy of all validator pubkeys.
func (vs *ValidatorSet) Validators() []Hash {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	result := make([]Hash, len(vs.validators))
	for i, v := range vs.validators {
		result[i] = v.Pubkey
	}

	return result
}

// Get returns validator info by pubkey, or nil if not found.
func (vs *ValidatorSet) Get(pubkey Hash) *ValidatorInfo {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	if idx, exists := vs.index[pubkey]; exists {
		info := vs.validators[idx]
		// Return a copy to avoid races
		return &ValidatorInfo{
			Pubkey:    info.Pubkey,
			HTTPAddr:  info.HTTPAddr,
			QUICAddr:  info.QUICAddr,
			BLSPubkey: info.BLSPubkey,
		}
	}

	return nil
}

// All returns a copy of all validator infos.
func (vs *ValidatorSet) All() []*ValidatorInfo {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	result := make([]*ValidatorInfo, len(vs.validators))
	for i, v := range vs.validators {
		result[i] = &ValidatorInfo{
			Pubkey:    v.Pubkey,
			HTTPAddr:  v.HTTPAddr,
			QUICAddr:  v.QUICAddr,
			BLSPubkey: v.BLSPubkey,
		}
	}

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
