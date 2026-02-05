package consensus

import (
	"crypto/ed25519"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"BluePods/internal/storage"
	"BluePods/internal/types"
)

const (
	// livenessTimeout is the fallback timeout to produce a vertex when idle.
	livenessTimeout = 500 * time.Millisecond

	// defaultGossipFanout is the number of peers to gossip vertices to.
	defaultGossipFanout = 40
)

// Executor executes committed transactions.
type Executor interface {
	Execute(tx []byte) error
}

// DAG manages the Mysticeti consensus.
type DAG struct {
	store       *store
	versions    *versionTracker
	validators  *ValidatorSet
	executor    Executor
	broadcaster Broadcaster
	systemPod   Hash
	epoch       uint64
	privKey     ed25519.PrivateKey
	pubKey      Hash

	// Round management
	round      atomic.Uint64
	roundMu    sync.Mutex
	pendingTxs [][]byte

	// Output channel for committed transactions
	committed chan CommittedTx

	// Commit tracking
	commitMu       sync.Mutex
	lastCommitted  uint64
	committedRound map[uint64]bool

	// Lifecycle
	stop chan struct{}
	wg   sync.WaitGroup
}

// Option configures the DAG during creation.
type Option func(*DAG)

// WithGenesisTxs sets transactions to include in the first vertex.
// These are injected into pendingTxs before the consensus loops start.
func WithGenesisTxs(txs [][]byte) Option {
	return func(d *DAG) {
		d.pendingTxs = append(d.pendingTxs, txs...)
	}
}

// New creates a DAG with the given parameters.
// Options are applied before starting the background goroutines.
func New(db *storage.Storage, validators *ValidatorSet, broadcaster Broadcaster, systemPod Hash, epoch uint64, privKey ed25519.PrivateKey, executor Executor, opts ...Option) *DAG {
	var pubKey Hash
	copy(pubKey[:], privKey.Public().(ed25519.PublicKey))

	d := &DAG{
		store:          newStore(db),
		versions:       newVersionTracker(),
		validators:     validators,
		executor:       executor,
		broadcaster:    broadcaster,
		systemPod:      systemPod,
		epoch:          epoch,
		privKey:        privKey,
		pubKey:         pubKey,
		committed:      make(chan CommittedTx, channelBuffer),
		committedRound: make(map[uint64]bool),
		stop:           make(chan struct{}),
	}

	for _, opt := range opts {
		opt(d)
	}

	d.wg.Add(2)
	go d.commitLoop()
	go d.livenessLoop()

	return d
}

// AddVertex validates and adds a vertex received from the network.
// Returns true if the vertex was new and added, false if duplicate or invalid.
func (d *DAG) AddVertex(data []byte) bool {
	vertex := types.GetRootAsVertex(data, 0)

	if err := d.validateVertex(vertex, data); err != nil {
		return false
	}

	hash := hashVertex(data)
	producer := extractProducer(vertex)
	round := vertex.Round()

	if !d.store.add(data, hash, round, producer) {
		return false // already exists
	}

	d.onVertexAdded(round)

	return true
}

// SubmitTx adds a transaction to be included in the next vertex.
func (d *DAG) SubmitTx(tx []byte) {
	d.roundMu.Lock()
	d.pendingTxs = append(d.pendingTxs, tx)
	d.roundMu.Unlock()

	d.tryProduceVertex()
}

// Committed returns a channel of finalized transactions.
func (d *DAG) Committed() <-chan CommittedTx {
	return d.committed
}

// Round returns the current round number.
func (d *DAG) Round() uint64 {
	return d.round.Load()
}

// Close stops the DAG and waits for goroutines to finish.
func (d *DAG) Close() {
	close(d.stop)
	d.wg.Wait()
	close(d.committed)
}

// onVertexAdded is called after a vertex is stored.
func (d *DAG) onVertexAdded(round uint64) {
	d.updateRound(round)
	d.tryProduceVertex()
}

// updateRound advances the local round if needed.
func (d *DAG) updateRound(round uint64) {
	for {
		current := d.round.Load()
		if round <= current {
			return
		}
		if d.round.CompareAndSwap(current, round) {
			return
		}
	}
}

// tryProduceVertex attempts to produce a vertex if conditions are met.
func (d *DAG) tryProduceVertex() {
	d.roundMu.Lock()
	defer d.roundMu.Unlock()

	round := d.round.Load()

	if !d.canProduceVertex(round) {
		return
	}

	parents := d.collectParents(round)
	txs := d.takePendingTxs()

	data := d.buildVertex(round, parents, txs)
	hash := hashVertex(data)
	d.store.add(data, hash, round, d.pubKey)

	fmt.Printf("[DAG] produced vertex round=%d txs=%d hash=%x\n", round, len(txs), hash[:8])

	d.sendVertex(data)
	d.round.Store(round + 1)
}

// canProduceVertex checks if we have quorum to produce.
func (d *DAG) canProduceVertex(round uint64) bool {
	if round == 0 {
		return true
	}

	return d.hasQuorumFromRound(round - 1)
}

// hasQuorumFromRound checks if we have enough vertices from a round.
func (d *DAG) hasQuorumFromRound(round uint64) bool {
	count := d.store.countByRound(round)
	return count >= d.validators.QuorumSize()
}

// collectParents gathers parent hashes from the previous round.
func (d *DAG) collectParents(round uint64) []Hash {
	if round == 0 {
		return nil
	}

	return d.store.getByRound(round - 1)
}

// takePendingTxs takes and clears pending transactions.
func (d *DAG) takePendingTxs() [][]byte {
	txs := d.pendingTxs
	d.pendingTxs = nil
	return txs
}

// sendVertex broadcasts a vertex to the network.
func (d *DAG) sendVertex(data []byte) {
	if d.broadcaster != nil {
		_ = d.broadcaster.Gossip(data, defaultGossipFanout)
	}
}

// livenessLoop ensures progress even when idle by periodically trying to produce.
func (d *DAG) livenessLoop() {
	defer d.wg.Done()

	ticker := time.NewTicker(livenessTimeout)
	defer ticker.Stop()

	for {
		select {
		case <-d.stop:
			return
		case <-ticker.C:
			d.tryProduceVertex()
		}
	}
}
