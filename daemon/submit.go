package daemon

import (
	"context"
	"errors"
	"fmt"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/network"
	"BluePods/internal/types"
)

// SubmitTransaction submits a signed transaction to the network. A transaction
// that touches only singletons (replication 0, including the gas coin) is
// submitted raw and wrapped into a trivial ATX by the receiving validator. A
// transaction that references replicated objects has its attestations collected,
// is assembled into an ATX (with the synced attestation epoch), and submitted.
// On a quorum-impossible result it resyncs the validator set and epoch once and
// retries with bounded randomized backoff before surfacing ErrQuorumImpossible.
func (d *Daemon) SubmitTransaction(ctx context.Context, rawTx []byte) ([]byte, error) {
	refs, err := replicatedRefs(d, ctx, rawTx)
	if err != nil {
		return nil, fmt.Errorf("inspect refs:\n%w", err)
	}

	if len(refs) == 0 {
		// Singleton-only: submit raw, the validator wraps it.
		return d.submitBody(rawTx)
	}

	return d.collectBuildSubmit(ctx, rawTx, refs)
}

// collectBuildSubmit collects attestations, builds the ATX, and submits it,
// resyncing and retrying once on a quorum-impossible result.
func (d *Daemon) collectBuildSubmit(ctx context.Context, rawTx []byte, refs []objectRef) ([]byte, error) {
	resynced := false

	for attempt := 0; attempt <= maxRetries; attempt++ {
		results, err := d.CollectAttestations(ctx, refs)
		if err == nil {
			atx := d.BuildATX(rawTx, results)
			return d.submitBody(atx)
		}

		if !errors.Is(err, ErrQuorumImpossible) {
			return nil, err
		}

		// Quorum impossible: resync once (a missed epoch transition surfaces here),
		// then keep retrying with bounded backoff for transient version races.
		if !resynced {
			_ = d.SyncValidators()
			resynced = true
		}

		backoff(ctx, attempt)
	}

	return nil, ErrQuorumImpossible
}

// BuildATX assembles an AttestedTransaction from the raw transaction, the
// collected replicated objects, their quorum proofs, and the synced attestation
// epoch. Singletons are not included.
func (d *Daemon) BuildATX(rawTx []byte, results []attestationResult) []byte {
	builder := flatbuffers.NewBuilder(len(rawTx) + 1024)

	tx := types.GetRootAsTransaction(rawTx, 0)
	txOffset := rebuildTx(builder, tx)

	objOffsets := make([]flatbuffers.UOffsetT, len(results))
	for i := range results {
		objOffsets[i] = rebuildObject(builder, results[i].ObjectData)
	}

	types.AttestedTransactionStartObjectsVector(builder, len(objOffsets))
	for i := len(objOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(objOffsets[i])
	}
	objectsVec := builder.EndVector(len(objOffsets))

	proofOffsets := make([]flatbuffers.UOffsetT, len(results))
	for i := range results {
		proofOffsets[i] = buildProof(builder, &results[i])
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
	types.AttestedTransactionAddAttestationEpoch(builder, d.Epoch())
	atxOffset := types.AttestedTransactionEnd(builder)

	builder.Finish(atxOffset)

	return builder.FinishedBytes()
}

// rebuildObject copies an Object FlatBuffer into the builder.
func rebuildObject(builder *flatbuffers.Builder, objData []byte) flatbuffers.UOffsetT {
	obj := types.GetRootAsObject(objData, 0)

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

// buildProof builds a QuorumProof from a collected attestation result.
func buildProof(builder *flatbuffers.Builder, result *attestationResult) flatbuffers.UOffsetT {
	objIDVec := builder.CreateByteVector(result.ObjectID[:])
	sigVec := builder.CreateByteVector(result.AggSig)
	bitmapVec := builder.CreateByteVector(result.Bitmap)

	types.QuorumProofStart(builder)
	types.QuorumProofAddObjectId(builder, objIDVec)
	types.QuorumProofAddBlsSignature(builder, sigVec)
	types.QuorumProofAddSignerBitmap(builder, bitmapVec)

	return types.QuorumProofEnd(builder)
}

// submitBody sends a raw transaction or ATX body to a node and returns the
// transaction hash on acceptance.
func (d *Daemon) submitBody(body []byte) ([]byte, error) {
	resp, err := d.roundTrip(network.EncodeSubmitTx(&network.SubmitTxRequest{Body: body}))
	if err != nil {
		return nil, fmt.Errorf("submit:\n%w", err)
	}

	parsed, err := network.DecodeSubmitTxResp(resp)
	if err != nil {
		return nil, fmt.Errorf("decode submit response:\n%w", err)
	}

	if parsed.Err != "" {
		return nil, fmt.Errorf("submission rejected: %s", parsed.Err)
	}

	return parsed.Hash, nil
}

// replicatedRefs inspects a raw transaction's object references and returns those
// that are replicated (replication > 0). It fetches each referenced object to
// learn its replication; singletons (including the gas coin) are excluded.
func replicatedRefs(d *Daemon, ctx context.Context, rawTx []byte) ([]objectRef, error) {
	tx := types.GetRootAsTransaction(rawTx, 0)

	candidates := collectRefs(tx)
	refs := make([]objectRef, 0, len(candidates))

	for _, c := range candidates {
		objData, err := d.GetObject(c.ID)
		if err != nil {
			return nil, fmt.Errorf("fetch ref %x:\n%w", c.ID[:4], err)
		}
		if objData == nil {
			// Unknown object: leave it for the validator's lifecycle checks.
			continue
		}

		obj := types.GetRootAsObject(objData, 0)
		if obj.Replication() == 0 {
			continue // singleton: never attested
		}

		refs = append(refs, c)
	}

	return refs, nil
}

// collectRefs extracts the read and mutable object references from a transaction,
// keyed by 32-byte ID (domain refs without an ID are skipped here).
func collectRefs(tx *types.Transaction) []objectRef {
	var refs []objectRef
	var ref types.ObjectRef

	for i := 0; i < tx.MutableRefsLength(); i++ {
		if tx.MutableRefs(&ref, i) {
			if r, ok := refFromObjectRef(&ref); ok {
				refs = append(refs, r)
			}
		}
	}

	for i := 0; i < tx.ReadRefsLength(); i++ {
		if tx.ReadRefs(&ref, i) {
			if r, ok := refFromObjectRef(&ref); ok {
				refs = append(refs, r)
			}
		}
	}

	return refs
}

// refFromObjectRef extracts an objectRef from an ObjectRef, or false when it
// carries no 32-byte ID (a domain-only reference).
func refFromObjectRef(ref *types.ObjectRef) (objectRef, bool) {
	idBytes := ref.IdBytes()
	if len(idBytes) != 32 {
		return objectRef{}, false
	}

	var r objectRef
	copy(r.ID[:], idBytes)
	r.Version = ref.Version()

	return r, true
}
