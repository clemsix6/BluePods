package consensus

import (
	"bytes"
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
func (d *DAG) commitRound(round uint64) {
	hashes := d.store.getByRound(round)

	for _, h := range hashes {
		v := d.store.get(h)
		if v == nil {
			continue
		}

		d.processTransactions(v, round)
	}
}

// processTransactions handles committed transactions from a vertex.
func (d *DAG) processTransactions(v *types.Vertex, commitRound uint64) {
	var atx types.AttestedTransaction

	for i := 0; i < v.TransactionsLength(); i++ {
		if !v.Transactions(&atx, i) {
			continue
		}

		d.executeTx(&atx, commitRound)
	}
}

// executeTx checks version conflicts and executes a transaction.
func (d *DAG) executeTx(atx *types.AttestedTransaction, commitRound uint64) {
	tx := atx.Transaction(nil)
	if tx == nil {
		logger.Warn("tx is nil, skipping")
		return
	}

	funcName := string(tx.FunctionName())
	logger.Debug("processing tx", "func", funcName)

	// Check and update versions atomically
	if !d.tracker.checkAndUpdate(tx) {
		logger.Debug("version conflict", "func", funcName)
		d.emitTransaction(tx, false) // conflict
		return
	}

	// Verify BLS quorum proofs (skip genesis/singletons/creates_objects with no proofs)
	if d.verifyATXProofs != nil && atx.ProofsLength() > 0 {
		if err := d.verifyATXProofs(atx); err != nil {
			logger.Warn("ATX proof verification failed", "func", funcName, "error", err)
			d.emitTransaction(tx, false)
			return
		}
	}

	// Handle system transactions
	d.handleRegisterValidator(tx, commitRound)
	d.handleDeregisterValidator(tx, commitRound)

	// Execution sharding: skip execution if not a holder of any mutable object.
	// creates_objects=true → ALL validators execute (holder unknown until after execution).
	// creates_objects=false → only holders of mutable objects execute.
	if !tx.CreatesObjects() && !d.shouldExecute(atx, tx) {
		logger.Debug("skipping execution (not holder)", "func", funcName)
		d.emitTransaction(tx, true)
		return
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
}

// shouldExecute returns true if this node should execute the transaction.
// A node executes if it is a holder of at least one object in MutableObjects.
// Singletons (replication=0, not in ATX objects) are held by all validators.
func (d *DAG) shouldExecute(atx *types.AttestedTransaction, tx *types.Transaction) bool {
	if d.isHolder == nil {
		return true // no sharding configured
	}

	// Build replication map from ATX objects vector
	replicationMap := buildReplicationMap(atx)

	// Check each mutable object
	data := tx.MutableObjectsBytes()
	for i := 0; i+40 <= len(data); i += 40 {
		var objectID [32]byte
		copy(objectID[:], data[i:i+32])

		replication, found := replicationMap[objectID]
		if !found {
			replication = 0 // not in ATX → singleton
		}

		if d.isHolder(objectID, replication) {
			return true
		}
	}

	return len(data) == 0 // no mutable objects → execute everywhere
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

	hashVec := builder.CreateByteVector(tx.HashBytes())
	readObjVec := builder.CreateByteVector(tx.ReadObjectsBytes())
	mutObjVec := builder.CreateByteVector(tx.MutableObjectsBytes())
	senderVec := builder.CreateByteVector(tx.SenderBytes())
	sigVec := builder.CreateByteVector(tx.SignatureBytes())
	podVec := builder.CreateByteVector(tx.PodBytes())
	funcNameOff := builder.CreateString(string(tx.FunctionName()))
	argsVec := builder.CreateByteVector(tx.ArgsBytes())

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddReadObjects(builder, readObjVec)
	types.TransactionAddMutableObjects(builder, mutObjVec)
	types.TransactionAddCreatesObjects(builder, tx.CreatesObjects())
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddSignature(builder, sigVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)

	return types.TransactionEnd(builder)
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
