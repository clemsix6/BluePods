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
func (d *DAG) isRoundCommitted(round uint64) bool {
	round2Hashes := d.store.getByRound(round + 2)

	// Determine required quorum for commit
	requiredQuorum := d.validators.QuorumSize()

	// During init (before minValidators), use quorum=1 to observe bootstrap's chain.
	// This allows all nodes to see registrations and reach minValidators together.
	if d.minValidators > 0 && d.validators.Len() < d.minValidators {
		requiredQuorum = 1
	}

	// During transition + convergence, use quorum=1 for commit.
	// Matches the isRoundInTransitionOrBuffer logic.
	if d.isRoundInTransitionOrBuffer(round + 2) {
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

	return len(producers) >= requiredQuorum
}

// commitRound processes all transactions from a committed round.
// Tracks per-validator round production and accumulates epoch fees.
func (d *DAG) commitRound(round uint64) {
	hashes := d.store.getByRound(round)
	d.epochTotalRounds++

	for _, h := range hashes {
		v := d.store.get(h)
		if v == nil {
			continue
		}

		// Track round production per validator
		producer := extractProducer(v)
		d.epochRoundsProduced[producer]++

		fees := d.processTransactions(v, round)

		// Accumulate epoch fees from vertex fee summary
		d.epochFees += fees.Epoch

		if fees.Total > 0 {
			logger.Debug("vertex fees",
				"round", round,
				"total", fees.Total,
				"aggregator", fees.Aggregator,
				"burned", fees.Burned,
				"epoch", fees.Epoch,
			)
		}
	}
}

// processTransactions handles committed transactions from a vertex.
// Returns the accumulated fee summary for the vertex.
func (d *DAG) processTransactions(v *types.Vertex, commitRound uint64) FeeSplit {
	var atx types.AttestedTransaction
	var vertexFees FeeSplit

	producer := extractProducer(v)

	for i := 0; i < v.TransactionsLength(); i++ {
		if !v.Transactions(&atx, i) {
			continue
		}

		txFees := d.executeTx(&atx, commitRound, producer)
		vertexFees.Total += txFees.Total
		vertexFees.Aggregator += txFees.Aggregator
		vertexFees.Burned += txFees.Burned
		vertexFees.Epoch += txFees.Epoch
	}

	return vertexFees
}

// executeTx checks version conflicts, deducts fees, and executes a transaction.
// Returns the fee split for this transaction (zero if fees disabled).
func (d *DAG) executeTx(atx *types.AttestedTransaction, commitRound uint64, producer Hash) FeeSplit {
	tx := atx.Transaction(nil)
	if tx == nil {
		logger.Warn("tx is nil, skipping")
		return FeeSplit{}
	}

	funcName := string(tx.FunctionName())
	logger.Debug("processing tx", "func", funcName)

	// Check and update versions atomically
	if !d.tracker.checkAndUpdate(tx) {
		logger.Debug("version conflict", "func", funcName)
		d.emitTransaction(tx, false) // conflict
		return FeeSplit{}
	}

	// Verify BLS quorum proofs (skip genesis/singletons/creates_objects with no proofs)
	if d.verifyATXProofs != nil && atx.ProofsLength() > 0 {
		if err := d.verifyATXProofs(atx); err != nil {
			logger.Warn("ATX proof verification failed", "func", funcName, "error", err)
			d.emitTransaction(tx, false)
			return FeeSplit{}
		}
	}

	// Protocol-level fee deduction (before execution)
	feeSplit, proceed := d.deductFees(tx, atx, producer)

	// If fee deduction rejected tx (insufficient funds, invalid gas_coin, min_gas violation)
	if !proceed {
		logger.Debug("fee deduction rejected", "func", funcName)
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

// deductFees performs protocol-level fee deduction from gas_coin.
// Returns the fee split and whether the tx should proceed.
// proceed=true means fees were successfully handled (or fees disabled/no gas_coin).
// proceed=false means tx must be rejected (invalid gas_coin, min_gas, insufficient funds).
func (d *DAG) deductFees(tx *types.Transaction, atx *types.AttestedTransaction, producer Hash) (FeeSplit, bool) {
	if d.feeParams == nil || d.coinStore == nil {
		return FeeSplit{}, true
	}

	// No gas_coin → genesis/bootstrap tx, skip fees
	gasCoinBytes := tx.GasCoinBytes()
	if len(gasCoinBytes) != 32 {
		return FeeSplit{}, true
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

	// Credit aggregator (vertex producer)
	d.creditAggregator(producer, split.Aggregator)

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

// creditAggregator credits the aggregator's coin with the aggregator share.
func (d *DAG) creditAggregator(producer Hash, amount uint64) {
	if amount == 0 || d.coinStore == nil {
		return
	}

	// Look up aggregator's coin. For now, the aggregator coin is identified
	// through ValidatorInfo. If not available, skip credit silently.
	info := d.validators.Get(producer)
	if info == nil {
		return
	}

	// TODO: aggregator coin lookup via ValidatorInfo once reward_coin is implemented
	// For now, aggregator credits are accumulated but not distributed to a specific coin.
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

	// Parse network addresses and BLS pubkey from transaction args
	httpAddr, quicAddr, blsPubkeyBytes := genesis.DecodeRegisterValidatorArgs(tx.ArgsBytes())

	var blsPubkey [48]byte
	if len(blsPubkeyBytes) == 48 {
		copy(blsPubkey[:], blsPubkeyBytes)
	}

	isNew := d.validators.Add(pubkey, httpAddr, quicAddr, blsPubkey)

	// Track mid-epoch additions for churn limiting
	if isNew && d.epochLength > 0 {
		d.epochAdditions = append(d.epochAdditions, pubkey)
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
