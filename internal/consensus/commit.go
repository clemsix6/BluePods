package consensus

import (
	"bytes"
	"fmt"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/genesis"
	"BluePods/internal/logger"
	"BluePods/internal/types"
)

const (
	// commitCheckInterval is how often to check for new commits.
	commitCheckInterval = 50 * time.Millisecond
)

// commitLoop runs in background and detects committed vertices.
func (d *DAG) commitLoop() {
	defer d.wg.Done()

	ticker := time.NewTicker(commitCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-d.stop:
			return
		case <-ticker.C:
			d.checkCommits()
		}
	}
}

// checkCommits looks for newly committed vertices.
// Processes rounds sequentially: stops at the first uncommitted round
// to avoid skipping rounds and losing them permanently.
func (d *DAG) checkCommits() {
	d.commitMu.Lock()
	defer d.commitMu.Unlock()

	currentRound := d.round.Load()
	if currentRound < 2 {
		return
	}

	for round := d.lastCommitted; round <= currentRound-2; round++ {
		if !d.isRoundCommitted(round) {
			break
		}

		logger.Debug("committing round", "round", round)
		d.commitRound(round)
		d.lastCommitted = round + 1

		// Check for epoch boundary after committing the round
		if d.isEpochBoundary(round) {
			d.transitionEpoch(round)
		}
	}
}

// isRoundCommitted checks if a round has reached commit (referenced by N+2 quorum).
// Only vertices from known validators are counted toward quorum.
// When a round commits with full BFT quorum (not relaxed), marks fullQuorumAchieved.
func (d *DAG) isRoundCommitted(round uint64) bool {
	round2Hashes := d.store.getByRound(round + 2)

	// Determine required quorum for commit
	fullQuorum := d.validators.QuorumSize()
	requiredQuorum := fullQuorum

	// During init (before minValidators), use quorum=1 to observe bootstrap's chain.
	// This allows all nodes to see registrations and reach minValidators together.
	if d.minValidators > 0 && d.validators.Len() < d.minValidators {
		requiredQuorum = 1
	}

	// During transition + convergence, use quorum=1 for commit.
	// Matches the isRoundInTransitionOrBuffer logic.
	relaxed := d.isRoundInTransitionOrBuffer(round + 2)
	if relaxed {
		requiredQuorum = 1
	}

	if len(round2Hashes) < requiredQuorum {
		return false
	}

	// Count unique producers that are valid validators
	producers := make(map[Hash]bool)
	for _, h := range round2Hashes {
		v := d.store.get(h)
		if v == nil {
			continue
		}

		producer := extractProducer(v)

		// Security: only count vertices from known validators
		if !d.validators.Contains(producer) {
			continue
		}

		producers[producer] = true
	}

	committed := len(producers) >= requiredQuorum

	// Mark full quorum achieved when a round commits with strict BFT quorum.
	// This is the commit-side signal that the network has truly converged.
	if committed && !relaxed && len(producers) >= fullQuorum {
		d.markFullQuorumAchieved()
	}

	return committed
}

// commitRound processes all transactions from a committed round.
// Tracks per-validator round production and accumulates epoch fees.
//
// Before applying, it verifies the round's BLS quorum proofs in one parallel
// gate: collectRoundProofs gathers every proof-bearing ATX in committed order,
// the batch verifier checks their signatures across cores, and the resulting
// per-ATX verdicts feed the sequential apply below. The verdict for each ATX is
// the same value the inline sequential verifier would have produced, so the set
// of accepted ATXs and their apply order are unchanged.
func (d *DAG) commitRound(round uint64) {
	hashes := d.store.getByRound(round)
	d.epochTotalRounds++

	verdicts := d.verifyRoundProofs(round, hashes)

	for _, h := range hashes {
		v := d.store.get(h)
		if v == nil {
			continue
		}

		// Track round production per validator
		producer := extractProducer(v)
		d.epochRoundsProduced[producer]++

		fees := d.processTransactions(v, round, verdicts)

		// Accumulate epoch fees from vertex fee summary
		d.epochFees += fees.Epoch

		if fees.Total > 0 {
			logger.Debug("vertex fees",
				"round", round,
				"total", fees.Total,
				"burned", fees.Burned,
				"epoch", fees.Epoch,
			)
		}
	}
}

// proofVerdicts carries the parallel proof-verification result of a round to the
// sequential apply loop. The two loops walk the round's vertices and transactions
// in the same order, so consuming one verdict per ATX with next() keeps the
// verdict aligned to its ATX. A nil verdicts pointer means proof verification is
// disabled, and next() always reports "no error".
type proofVerdicts struct {
	errs   []error // errs holds one entry per proof-bearing ATX, in committed order
	cursor int     // cursor is the index of the next proof-bearing ATX to consume
}

// next reports the verdict for the next proof-bearing ATX in committed order.
// hasProofs must mirror the apply loop's "this ATX carries proofs" test so the
// cursor advances in lockstep with the pre-pass. ATXs without proofs are not
// represented in errs and never advance the cursor, matching the pre-pass.
func (p *proofVerdicts) next(hasProofs bool) error {
	if p == nil || !hasProofs {
		return nil
	}

	if p.cursor >= len(p.errs) {
		return nil
	}

	err := p.errs[p.cursor]
	p.cursor++

	return err
}

// verifyRoundProofs runs the parallel proof gate for a round and returns the
// per-ATX verdicts the apply loop consumes. It returns nil when proof
// verification is disabled, so executeTx falls back to its no-verification path.
func (d *DAG) verifyRoundProofs(round uint64, hashes []Hash) *proofVerdicts {
	if d.verifyATXProofsBatch == nil {
		return nil
	}

	atxs := d.collectRoundProofs(hashes)
	if len(atxs) == 0 {
		return &proofVerdicts{}
	}

	return &proofVerdicts{errs: d.verifyATXProofsBatch(atxs, round)}
}

// collectRoundProofs walks the round's vertices and transactions in committed
// order and returns one fresh AttestedTransaction per proof-bearing ATX. It
// mirrors the apply loop's iteration exactly (same vertex order, same
// Transactions index, same skip conditions) so the returned slice lines up
// one-to-one with the apply loop's proof-bearing ATXs.
func (d *DAG) collectRoundProofs(hashes []Hash) []*types.AttestedTransaction {
	var atxs []*types.AttestedTransaction

	for _, h := range hashes {
		v := d.store.get(h)
		if v == nil {
			continue
		}

		for i := 0; i < v.TransactionsLength(); i++ {
			atx := new(types.AttestedTransaction)
			if !v.Transactions(atx, i) {
				continue
			}

			// Mirror executeTx exactly: an ATX with a nil transaction returns
			// before the proof check, and an ATX with no proofs skips it, so
			// neither is part of the batch. Keeping these predicates identical
			// keeps the verdict cursor aligned with the apply loop.
			if atx.Transaction(nil) == nil {
				continue
			}

			if atx.ProofsLength() == 0 {
				continue
			}

			atxs = append(atxs, atx)
		}
	}

	return atxs
}

// processTransactions handles committed transactions from a vertex.
// Returns the accumulated fee summary for the vertex.
func (d *DAG) processTransactions(v *types.Vertex, commitRound uint64, verdicts *proofVerdicts) FeeSplit {
	var atx types.AttestedTransaction
	var vertexFees FeeSplit

	producer := extractProducer(v)

	for i := 0; i < v.TransactionsLength(); i++ {
		if !v.Transactions(&atx, i) {
			continue
		}

		txFees := d.executeTx(&atx, commitRound, producer, verdicts)
		vertexFees.Total += txFees.Total
		vertexFees.Burned += txFees.Burned
		vertexFees.Epoch += txFees.Epoch
	}

	return vertexFees
}

// executeTx checks version conflicts, deducts fees, and executes a transaction.
// Returns the fee split for this transaction (zero if fees disabled).
//
// verdicts carries the round's parallel proof-verification result; it is nil
// only when executeTx is driven outside the round commit loop (such as direct
// unit tests), in which case proofs are verified inline.
func (d *DAG) executeTx(atx *types.AttestedTransaction, commitRound uint64, producer Hash, verdicts *proofVerdicts) FeeSplit {
	tx := atx.Transaction(nil)
	if tx == nil {
		logger.Warn("tx is nil, skipping")
		return FeeSplit{}
	}

	funcName := string(tx.FunctionName())
	logger.Debug("processing tx", "func", funcName)

	// Verify BLS quorum proofs first (skip genesis/singletons/creates_objects with
	// no proofs). This runs before the commit-once guard on purpose: a proof or
	// epoch failure depends on the attested-transaction wrapper (its proofs and
	// attestation epoch), not on the inner transaction, so a client that recollects
	// against the current epoch and resubmits sends the same inner transaction hash
	// with fresh proofs. Marking that hash on a proof failure would block the
	// legitimate recollect-after-grace resubmission, so a proof failure returns
	// without recording the hash.
	if err := d.proofVerdict(atx, commitRound, verdicts); err != nil {
		logger.Warn("ATX proof verification failed", "func", funcName, "error", err)
		d.emitTransaction(tx, false)
		return FeeSplit{}
	}

	// Commit-time authenticity: re-verify the inner transaction's sender signature
	// and hash. This runs deterministically on every node, after the proof verdict
	// is consumed (so the batch-proof cursor stays aligned) and before the
	// commit-once guard (so a forged tx cannot poison the tracker with a chosen
	// hash to censor a legitimate one). A gossiped transaction can reach commit
	// without passing local ingress validation, so authenticity must be enforced
	// here, where every node agrees.
	if d.verifyTxAuth != nil {
		if err := d.verifyTxAuth(tx); err != nil {
			logger.Warn("tx authenticity verification failed", "func", funcName, "error", err)
			d.emitTransaction(tx, false)
			return FeeSplit{}
		}
	}

	// Commit-once guard: a transaction can reach the commit path more than once
	// when several producers include the same gossiped transaction in their
	// vertices. The first occurrence proceeds; later occurrences are skipped so
	// object creation, fees, and validator changes are never applied twice. The
	// hash covers the mutable refs (with versions), so a legitimate retry against a
	// newer version has a different hash and is not blocked.
	if txHash, ok := txCommitHash(tx); ok {
		if d.tracker.wasCommitted(txHash) {
			logger.Debug("duplicate tx skipped", "func", funcName)
			return FeeSplit{}
		}

		d.tracker.markCommitted(txHash)
	}

	// Check and update versions atomically
	if !d.tracker.checkAndUpdate(tx) {
		logger.Debug("version conflict", "func", funcName)
		d.emitTransaction(tx, false) // conflict
		return FeeSplit{}
	}

	// Protocol-level fee deduction (before execution)
	feeSplit, proceed := d.deductFees(tx, atx, producer)

	// If fee deduction rejected tx (insufficient funds, invalid gas_coin, min_gas violation)
	if !proceed {
		logger.Debug("fee deduction rejected", "func", funcName)
		d.emitTransaction(tx, false)
		return feeSplit
	}

	// Ownership check: sender must own all mutable_ref objects
	if !d.validateMutableRefOwnership(tx) {
		logger.Warn("mutable_ref ownership rejected", "func", funcName)
		d.emitTransaction(tx, false)
		return feeSplit
	}

	// Handle system transactions
	d.handleRegisterValidator(tx, commitRound)
	d.handleDeregisterValidator(tx, commitRound)

	// Execution sharding: skip execution if not a holder of any mutable object.
	// created_objects_replication/max_create_domains > 0 → ALL validators execute (holder unknown until after execution).
	// Otherwise → only holders of mutable objects execute.
	if tx.CreatedObjectsReplicationLength() == 0 && tx.MaxCreateDomains() == 0 && !d.shouldExecute(atx, tx) {
		logger.Debug("skipping execution (not holder)", "func", funcName)
		d.emitTransaction(tx, true)
		return feeSplit
	}

	// Execute via state (objects are in AttestedTransaction)
	var success bool
	if d.executor != nil {
		// Re-serialize AttestedTransaction as standalone buffer
		atxBytes := serializeAttestedTx(atx)
		err := d.executor.Execute(atxBytes)
		success = err == nil
		if err != nil {
			logger.Error("executor error", "func", funcName, "error", err)
		}
	} else {
		success = true // no executor = skip execution
	}

	logger.Debug("tx completed", "func", funcName, "success", success)
	d.emitTransaction(tx, success)

	return feeSplit
}

// proofVerdict returns the proof-verification verdict for one ATX. An ATX with no
// proofs is always accepted (genesis/singletons), exactly as before. When the
// round was verified in parallel, the verdict is read from verdicts in committed
// order so it lines up with this ATX. When verdicts is nil (direct unit-test
// calls outside the round loop), it falls back to the inline single verifier so
// the verdict is still identical to the sequential implementation.
func (d *DAG) proofVerdict(atx *types.AttestedTransaction, commitRound uint64, verdicts *proofVerdicts) error {
	hasProofs := atx.ProofsLength() > 0

	if verdicts != nil {
		return verdicts.next(hasProofs)
	}

	if d.verifyATXProofs != nil && hasProofs {
		return d.verifyATXProofs(atx, commitRound)
	}

	return nil
}

// deductFees performs protocol-level fee deduction from gas_coin.
// Returns the fee split and whether the tx should proceed.
// proceed=true means fees were successfully handled (or fees are disabled).
// proceed=false means tx must be rejected (missing/invalid gas_coin, min_gas,
// insufficient funds).
func (d *DAG) deductFees(tx *types.Transaction, atx *types.AttestedTransaction, producer Hash) (FeeSplit, bool) {
	if d.feeParams == nil || d.coinStore == nil {
		return FeeSplit{}, true
	}

	// No gas coin: reject, unless this is a validator-set-management action.
	// Genesis is seeded state and protocol actions (issuance, reward crediting,
	// slashing) are not transactions, so every user transaction must reference a
	// funded gas coin. Validator (de)registration is a network-formation action,
	// not a value transaction: a joining validator has no coin yet, and its
	// authenticity is already enforced at commit, so it is exempt here.
	gasCoinBytes := tx.GasCoinBytes()
	if len(gasCoinBytes) != 32 {
		if d.isRegisterValidatorTx(tx) || d.isDeregisterValidatorTx(tx) {
			return FeeSplit{}, true
		}

		return FeeSplit{}, false
	}

	var gasCoinID [32]byte
	copy(gasCoinID[:], gasCoinBytes)

	// min_gas anti-spam check
	if tx.MaxGas() < d.feeParams.MinGas {
		logger.Warn("max_gas below minimum", "max_gas", tx.MaxGas(), "min_gas", d.feeParams.MinGas)
		return FeeSplit{}, false
	}

	// Validate gas_coin ownership: must belong to sender
	if err := d.validateGasCoin(tx, gasCoinID); err != nil {
		logger.Warn("gas coin validation failed", "error", err)
		return FeeSplit{}, false
	}

	// Calculate fee from tx header
	fee := d.calculateTxFee(tx, atx)
	if fee == 0 {
		return FeeSplit{}, true
	}

	// Deduct fee from gas_coin
	deducted, fullyCovered, err := deductCoinFee(d.coinStore, gasCoinID, fee)
	if err != nil {
		logger.Warn("fee deduction failed", "error", err)
		return FeeSplit{}, false
	}

	// Split the actually deducted amount
	split := SplitFee(deducted, *d.feeParams)

	// If insufficient funds: fees partially deducted, tx rejected
	if !fullyCovered {
		split.Total = fee
		return split, false
	}

	return split, true
}

// validateGasCoin checks that the gas_coin exists, is a singleton, and belongs to sender.
func (d *DAG) validateGasCoin(tx *types.Transaction, gasCoinID [32]byte) error {
	data := d.coinStore.GetObject(gasCoinID)
	if data == nil {
		return fmt.Errorf("gas coin not found: %x", gasCoinID[:8])
	}

	// Must be a singleton (replication=0)
	if rep := readCoinReplication(data); rep != 0 {
		return fmt.Errorf("gas coin is not a singleton: replication=%d", rep)
	}

	// Owner must match sender
	owner, err := readCoinOwner(data)
	if err != nil {
		return err
	}

	senderBytes := tx.SenderBytes()
	if len(senderBytes) != 32 {
		return fmt.Errorf("invalid sender length: %d", len(senderBytes))
	}

	var sender [32]byte
	copy(sender[:], senderBytes)

	if owner != sender {
		return fmt.Errorf("gas coin owner mismatch: owner=%x sender=%x", owner[:8], sender[:8])
	}

	return nil
}

// validateMutableRefOwnership checks that all mutable refs are owned by the sender.
// Returns false if any mutable ref is not owned by the sender.
func (d *DAG) validateMutableRefOwnership(tx *types.Transaction) bool {
	count := tx.MutableRefsLength()
	if count == 0 || d.coinStore == nil {
		return true
	}

	senderBytes := tx.SenderBytes()
	if len(senderBytes) != 32 {
		return false
	}

	var sender Hash
	copy(sender[:], senderBytes)

	var ref types.ObjectRef

	for i := 0; i < count; i++ {
		tx.MutableRefs(&ref, i)

		idBytes := ref.IdBytes()
		if len(idBytes) != 32 {
			// Domain refs have no ID — skip ownership check
			if len(ref.Domain()) > 0 {
				continue
			}
			return false
		}

		var objectID Hash
		copy(objectID[:], idBytes)

		data := d.coinStore.GetObject(objectID)
		if data == nil {
			logger.Warn("mutable ref not found", "id_prefix", objectID[:4])
			return false
		}

		owner, err := readCoinOwner(data)
		if err != nil {
			logger.Warn("mutable ref invalid owner", "error", err)
			return false
		}

		if owner != sender {
			logger.Warn("mutable ref ownership mismatch",
				"id_prefix", objectID[:4],
				"owner_prefix", owner[:4],
				"sender_prefix", sender[:4],
			)
			return false
		}
	}

	return true
}

// calculateTxFee computes the total fee from transaction header fields.
func (d *DAG) calculateTxFee(tx *types.Transaction, atx *types.AttestedTransaction) uint64 {
	// Build mutable refs for replication ratio
	mutableRefs := extractMutableObjectRefs(tx, atx)

	// Count standard objects (non-singletons in read + mutable refs)
	readRefs := extractReadObjectRefs(tx, atx)
	allRefs := append(mutableRefs, readRefs...)
	standardCount := CountStandardObjects(allRefs)

	// Build created objects replication slice
	createdReps := extractCreatedObjectsReplication(tx)

	// Calculate replication ratio
	totalValidators := d.validators.Len()
	repNum, repDenom := ReplicationRatio(
		mutableRefs,
		len(createdReps),
		int(tx.MaxCreateDomains()),
		d.computeHolders,
		totalValidators,
	)

	return CalculateFee(
		tx.MaxGas(),
		repNum, repDenom,
		standardCount,
		createdReps,
		int(tx.MaxCreateDomains()),
		totalValidators,
		*d.feeParams,
	)
}

// extractMutableObjectRefs builds ObjectRef slice from tx mutable refs + ATX replication map.
func extractMutableObjectRefs(tx *types.Transaction, atx *types.AttestedTransaction) []ObjectRef {
	count := tx.MutableRefsLength()
	if count == 0 {
		return nil
	}

	repMap := buildReplicationMap(atx)
	refs := make([]ObjectRef, 0, count)
	var ref types.ObjectRef

	for i := 0; i < count; i++ {
		if !tx.MutableRefs(&ref, i) {
			continue
		}

		idBytes := ref.IdBytes()
		if len(idBytes) != 32 {
			continue
		}

		var id [32]byte
		copy(id[:], idBytes)

		replication := uint16(0)
		if rep, found := repMap[id]; found {
			replication = rep
		}

		refs = append(refs, ObjectRef{ID: id, Replication: replication})
	}

	return refs
}

// extractReadObjectRefs builds ObjectRef slice from tx read refs + ATX replication map.
func extractReadObjectRefs(tx *types.Transaction, atx *types.AttestedTransaction) []ObjectRef {
	count := tx.ReadRefsLength()
	if count == 0 {
		return nil
	}

	repMap := buildReplicationMap(atx)
	refs := make([]ObjectRef, 0, count)
	var ref types.ObjectRef

	for i := 0; i < count; i++ {
		if !tx.ReadRefs(&ref, i) {
			continue
		}

		idBytes := ref.IdBytes()
		if len(idBytes) != 32 {
			continue
		}

		var id [32]byte
		copy(id[:], idBytes)

		replication := uint16(0)
		if rep, found := repMap[id]; found {
			replication = rep
		}

		refs = append(refs, ObjectRef{ID: id, Replication: replication})
	}

	return refs
}

// extractCreatedObjectsReplication reads the created_objects_replication vector from tx.
func extractCreatedObjectsReplication(tx *types.Transaction) []uint16 {
	count := tx.CreatedObjectsReplicationLength()
	if count == 0 {
		return nil
	}

	reps := make([]uint16, count)
	for i := 0; i < count; i++ {
		reps[i] = tx.CreatedObjectsReplication(i)
	}

	return reps
}

// shouldExecute returns true if this node should execute the transaction.
// A node executes if it is a holder of at least one object in MutableRefs.
// Singletons (replication=0, not in ATX objects) are held by all validators.
func (d *DAG) shouldExecute(atx *types.AttestedTransaction, tx *types.Transaction) bool {
	if d.isHolder == nil {
		return true
	}

	replicationMap := buildReplicationMap(atx)

	var ref types.ObjectRef
	for i := 0; i < tx.MutableRefsLength(); i++ {
		if !tx.MutableRefs(&ref, i) {
			continue
		}

		idBytes := ref.IdBytes()
		if len(idBytes) != 32 {
			continue
		}

		var objectID [32]byte
		copy(objectID[:], idBytes)

		replication, found := replicationMap[objectID]
		if !found {
			replication = 0
		}

		if d.isHolder(objectID, replication) {
			return true
		}
	}

	return tx.MutableRefsLength() == 0
}

// buildReplicationMap extracts objectID → replication from ATX objects vector.
func buildReplicationMap(atx *types.AttestedTransaction) map[[32]byte]uint16 {
	count := atx.ObjectsLength()
	if count == 0 {
		return nil
	}

	m := make(map[[32]byte]uint16, count)
	var obj types.Object

	for i := 0; i < count; i++ {
		if !atx.Objects(&obj, i) {
			continue
		}

		idBytes := obj.IdBytes()
		if len(idBytes) != 32 {
			continue
		}

		var id [32]byte
		copy(id[:], idBytes)
		m[id] = obj.Replication()
	}

	return m
}

// handleRegisterValidator checks if TX is register_validator and adds the validator.
// The validator pubkey is taken from tx.Sender (matching the Rust pod behavior).
// Network addresses are parsed from tx.Args.
func (d *DAG) handleRegisterValidator(tx *types.Transaction, commitRound uint64) {
	if !d.isRegisterValidatorTx(tx) {
		return
	}

	sender := tx.SenderBytes()
	if len(sender) != 32 {
		return
	}

	var pubkey Hash
	copy(pubkey[:], sender)

	// Parse network address and BLS pubkey from transaction args
	quicAddr, blsPubkeyBytes := genesis.DecodeRegisterValidatorArgs(tx.ArgsBytes())

	var blsPubkey [48]byte
	if len(blsPubkeyBytes) == 48 {
		copy(blsPubkey[:], blsPubkeyBytes)
	}

	isNew := d.validators.Add(pubkey, quicAddr, blsPubkey)

	// Track mid-epoch additions for churn limiting
	if isNew && d.epochLength > 0 {
		d.epochAdditions = append(d.epochAdditions, pubkey)
	}

	// Retry pending vertices — some may be from this newly registered producer.
	// Run async to avoid blocking the commit path.
	if isNew {
		go d.processPendingVertices()
	}

	// Enter transition immediately when minValidators is reached.
	// This prevents producing vertices with cross-references before transition
	// parent filtering kicks in.
	if d.minValidators > 0 && d.validators.Len() >= d.minValidators {
		d.enterTransition(commitRound)
	}
}

// handleDeregisterValidator checks if TX is deregister_validator and marks for removal.
// The validator stays active until the next epoch boundary.
func (d *DAG) handleDeregisterValidator(tx *types.Transaction, commitRound uint64) {
	if !d.isDeregisterValidatorTx(tx) {
		return
	}

	sender := tx.SenderBytes()
	if len(sender) != 32 {
		return
	}

	var pubkey Hash
	copy(pubkey[:], sender)

	d.pendingRemovals[pubkey] = true

	logger.Info("validator deregistration pending",
		"pubkey_prefix", pubkey[:4],
		"commitRound", commitRound,
	)
}

// isDeregisterValidatorTx checks if a transaction calls deregister_validator on system pod.
func (d *DAG) isDeregisterValidatorTx(tx *types.Transaction) bool {
	podBytes := tx.PodBytes()
	if len(podBytes) != 32 {
		return false
	}

	if !bytes.Equal(podBytes, d.systemPod[:]) {
		return false
	}

	funcName := string(tx.FunctionName())
	return funcName == deregisterValidatorFunc
}

// isRegisterValidatorTx checks if a transaction calls register_validator on system pod.
func (d *DAG) isRegisterValidatorTx(tx *types.Transaction) bool {
	podBytes := tx.PodBytes()
	if len(podBytes) != 32 {
		return false
	}

	if !bytes.Equal(podBytes, d.systemPod[:]) {
		return false
	}

	funcName := string(tx.FunctionName())
	return funcName == registerValidatorFunc
}

// txCommitHash returns a transaction's 32-byte hash, or ok=false when the hash
// field is malformed (such a transaction is processed without the commit-once
// guard rather than being dropped on a bad hash length).
func txCommitHash(tx *types.Transaction) (Hash, bool) {
	hashBytes := tx.HashBytes()
	if len(hashBytes) != 32 {
		return Hash{}, false
	}

	var hash Hash
	copy(hash[:], hashBytes)

	return hash, true
}

// emitTransaction sends a committed transaction to the output channel.
func (d *DAG) emitTransaction(tx *types.Transaction, success bool) {
	var txHash Hash
	if hashBytes := tx.HashBytes(); len(hashBytes) == 32 {
		copy(txHash[:], hashBytes)
	}

	var sender Hash
	if senderBytes := tx.SenderBytes(); len(senderBytes) == 32 {
		copy(sender[:], senderBytes)
	}

	committed := CommittedTx{
		Hash:     txHash,
		Success:  success,
		Function: string(tx.FunctionName()),
		Sender:   sender,
	}

	select {
	case d.committed <- committed:
	case <-d.stop:
	}
}

// serializeAttestedTx re-serializes an AttestedTransaction as a standalone buffer.
// This is needed because atx.Table().Bytes returns the parent Vertex buffer.
func serializeAttestedTx(atx *types.AttestedTransaction) []byte {
	builder := flatbuffers.NewBuilder(1024)

	// Rebuild Transaction
	tx := atx.Transaction(nil)
	txOffset := serializeTx(builder, tx)

	// Rebuild Objects vector
	objOffsets := make([]flatbuffers.UOffsetT, atx.ObjectsLength())
	for i := 0; i < atx.ObjectsLength(); i++ {
		var obj types.Object
		atx.Objects(&obj, i)
		objOffsets[i] = serializeObject(builder, &obj)
	}

	types.AttestedTransactionStartObjectsVector(builder, len(objOffsets))
	for i := len(objOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(objOffsets[i])
	}
	objectsVec := builder.EndVector(len(objOffsets))

	// Rebuild Proofs vector
	proofOffsets := make([]flatbuffers.UOffsetT, atx.ProofsLength())
	for i := 0; i < atx.ProofsLength(); i++ {
		var proof types.QuorumProof
		atx.Proofs(&proof, i)
		proofOffsets[i] = serializeQuorumProof(builder, &proof)
	}

	types.AttestedTransactionStartProofsVector(builder, len(proofOffsets))
	for i := len(proofOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(proofOffsets[i])
	}
	proofsVec := builder.EndVector(len(proofOffsets))

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOffset)
	types.AttestedTransactionAddObjects(builder, objectsVec)
	types.AttestedTransactionAddProofs(builder, proofsVec)
	types.AttestedTransactionAddAttestationEpoch(builder, atx.AttestationEpoch())
	atxOffset := types.AttestedTransactionEnd(builder)

	builder.Finish(atxOffset)

	return builder.FinishedBytes()
}

// serializeTx rebuilds a Transaction in the builder.
func serializeTx(builder *flatbuffers.Builder, tx *types.Transaction) flatbuffers.UOffsetT {
	if tx == nil {
		types.TransactionStart(builder)
		return types.TransactionEnd(builder)
	}

	return genesis.RebuildTxInBuilder(builder, tx)
}

// serializeObject rebuilds an Object in the builder.
func serializeObject(builder *flatbuffers.Builder, obj *types.Object) flatbuffers.UOffsetT {
	idVec := builder.CreateByteVector(obj.IdBytes())
	ownerVec := builder.CreateByteVector(obj.OwnerBytes())
	contentVec := builder.CreateByteVector(obj.ContentBytes())

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, obj.Version())
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, obj.Replication())
	types.ObjectAddContent(builder, contentVec)
	types.ObjectAddFees(builder, obj.Fees())

	return types.ObjectEnd(builder)
}

// serializeQuorumProof rebuilds a QuorumProof in the builder.
func serializeQuorumProof(builder *flatbuffers.Builder, proof *types.QuorumProof) flatbuffers.UOffsetT {
	objIdVec := builder.CreateByteVector(proof.ObjectIdBytes())
	blsSigVec := builder.CreateByteVector(proof.BlsSignatureBytes())
	bitmapVec := builder.CreateByteVector(proof.SignerBitmapBytes())

	types.QuorumProofStart(builder)
	types.QuorumProofAddObjectId(builder, objIdVec)
	types.QuorumProofAddBlsSignature(builder, blsSigVec)
	types.QuorumProofAddSignerBitmap(builder, bitmapVec)

	return types.QuorumProofEnd(builder)
}
