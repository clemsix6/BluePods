package consensus

import (
	"bytes"
	"fmt"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// commitLoop runs in background and detects committed vertices.
func (d *DAG) commitLoop() {
	defer d.wg.Done()

	for {
		select {
		case <-d.stop:
			return
		default:
			d.checkCommits()
		}
	}
}

// checkCommits looks for newly committed vertices.
func (d *DAG) checkCommits() {
	d.commitMu.Lock()
	defer d.commitMu.Unlock()

	currentRound := d.round.Load()
	if currentRound < 2 {
		return
	}

	for round := d.lastCommitted; round <= currentRound-2; round++ {
		if d.committedRound[round] {
			continue
		}

		if d.isRoundCommitted(round) {
			fmt.Printf("[DAG] committing round=%d\n", round)
			d.commitRound(round)
			d.committedRound[round] = true
			d.lastCommitted = round + 1
		}
	}
}

// isRoundCommitted checks if a round has reached commit (referenced by N+2 quorum).
func (d *DAG) isRoundCommitted(round uint64) bool {
	round2Hashes := d.store.getByRound(round + 2)

	if len(round2Hashes) < d.validators.QuorumSize() {
		return false
	}

	producers := make(map[Hash]bool)
	for _, h := range round2Hashes {
		v := d.store.get(h)
		if v == nil {
			continue
		}
		producer := extractProducer(v)
		producers[producer] = true
	}

	return len(producers) >= d.validators.QuorumSize()
}

// commitRound processes all transactions from a committed round.
func (d *DAG) commitRound(round uint64) {
	hashes := d.store.getByRound(round)

	for _, h := range hashes {
		v := d.store.get(h)
		if v == nil {
			continue
		}

		d.processTransactions(v)
	}
}

// processTransactions handles committed transactions from a vertex.
func (d *DAG) processTransactions(v *types.Vertex) {
	var atx types.AttestedTransaction

	for i := 0; i < v.TransactionsLength(); i++ {
		if !v.Transactions(&atx, i) {
			continue
		}

		d.executeTx(&atx)
	}
}

// executeTx checks version conflicts and executes a transaction.
func (d *DAG) executeTx(atx *types.AttestedTransaction) {
	tx := atx.Transaction(nil)
	if tx == nil {
		fmt.Printf("[executeTx] tx is nil, skipping\n")
		return
	}

	funcName := string(tx.FunctionName())
	fmt.Printf("[executeTx] processing tx func=%s\n", funcName)

	// Check and update versions atomically
	if !d.versions.checkAndUpdate(tx) {
		fmt.Printf("[executeTx] version conflict for func=%s\n", funcName)
		d.emitTransaction(tx, false) // conflict
		return
	}

	// Handle system transactions (register_validator)
	d.handleRegisterValidator(tx)

	// Execute via state (objects are in AttestedTransaction)
	var success bool
	if d.executor != nil {
		// Re-serialize AttestedTransaction as standalone buffer
		atxBytes := serializeAttestedTx(atx)
		err := d.executor.Execute(atxBytes)
		success = err == nil
		if err != nil {
			fmt.Printf("[executeTx] executor error for func=%s: %v\n", funcName, err)
		}
	} else {
		success = true // no executor = skip execution
	}

	fmt.Printf("[executeTx] completed func=%s success=%v\n", funcName, success)
	d.emitTransaction(tx, success)
}

// handleRegisterValidator checks if TX is register_validator and adds the validator.
// The validator pubkey is taken from tx.Sender (matching the Rust pod behavior).
func (d *DAG) handleRegisterValidator(tx *types.Transaction) {
	if !d.isRegisterValidatorTx(tx) {
		return
	}

	sender := tx.SenderBytes()
	if len(sender) != 32 {
		return
	}

	var pubkey Hash
	copy(pubkey[:], sender)

	d.validators.Add(pubkey)
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

	select {
	case d.committed <- CommittedTx{Hash: txHash, Success: success}:
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
