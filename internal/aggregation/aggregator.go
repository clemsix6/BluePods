package aggregation

import (
	"context"
	"fmt"
	"sync"

	"BluePods/internal/consensus"
	"BluePods/internal/network"
	"BluePods/internal/state"
	"BluePods/internal/types"

	flatbuffers "github.com/google/flatbuffers/go"
)

// Aggregator orchestrates the full aggregation process.
// It transforms a Transaction into an AttestedTransaction.
type Aggregator struct {
	collector  *Collector              // collector handles attestation collection
	rendezvous *Rendezvous             // rendezvous computes object-holder mappings
	validators *consensus.ValidatorSet // validators is the set of active validators
	state      *state.State            // state is the local storage for object lookup
}

// NewAggregator creates a new Aggregator.
func NewAggregator(node *network.Node, vs *consensus.ValidatorSet, st *state.State) *Aggregator {
	rv := NewRendezvous(vs)

	return &Aggregator{
		collector:  NewCollector(node, rv, vs, st),
		rendezvous: rv,
		validators: vs,
		state:      st,
	}
}

// Aggregate collects attestations and builds an AttestedTransaction.
// Returns the serialized FlatBuffers AttestedTransaction.
func (a *Aggregator) Aggregate(ctx context.Context, txData []byte) ([]byte, error) {
	tx := types.GetRootAsTransaction(txData, 0)

	refs := a.extractObjectRefs(tx)
	if len(refs) == 0 {
		return a.buildAttestedTransaction(txData, nil)
	}

	results, err := a.collectAllObjects(ctx, refs)
	if err != nil {
		return nil, fmt.Errorf("collect objects:\n%w", err)
	}

	return a.buildAttestedTransaction(txData, results)
}

// extractObjectRefs extracts object references from a transaction.
func (a *Aggregator) extractObjectRefs(tx *types.Transaction) []ObjectRef {
	var refs []ObjectRef

	readBytes := tx.ReadObjectsBytes()
	refs = append(refs, a.parseObjectIDs(readBytes)...)

	mutableBytes := tx.MutableObjectsBytes()
	refs = append(refs, a.parseObjectIDs(mutableBytes)...)

	return refs
}

// parseObjectIDs parses a byte slice of concatenated 32-byte object IDs.
func (a *Aggregator) parseObjectIDs(data []byte) []ObjectRef {
	if len(data) == 0 {
		return nil
	}

	numObjects := len(data) / 32
	refs := make([]ObjectRef, 0, numObjects)

	for i := 0; i < numObjects; i++ {
		var ref ObjectRef
		copy(ref.ID[:], data[i*32:(i+1)*32])
		// TODO: Version should be fetched from storage or passed in
		ref.Version = 0

		refs = append(refs, ref)
	}

	return refs
}

// collectAllObjects collects attestations for all objects in parallel.
func (a *Aggregator) collectAllObjects(ctx context.Context, refs []ObjectRef) ([]*CollectionResult, error) {
	results := make([]*CollectionResult, len(refs))

	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex

	for i, ref := range refs {
		wg.Add(1)

		go func(idx int, r ObjectRef) {
			defer wg.Done()

			result := a.collector.CollectObject(ctx, r)
			results[idx] = result

			// Singletons don't have errors, they're just skipped
			if result.Error != nil && !result.IsSingleton {
				errMu.Lock()
				if firstErr == nil {
					firstErr = result.Error
				}
				errMu.Unlock()
			}
		}(i, ref)
	}

	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}

	return results, nil
}

// buildAttestedTransaction constructs the FlatBuffers AttestedTransaction.
func (a *Aggregator) buildAttestedTransaction(txData []byte, results []*CollectionResult) ([]byte, error) {
	builder := flatbuffers.NewBuilder(1024)

	txOffset := builder.CreateByteVector(txData)

	var objectOffsets, proofOffsets []flatbuffers.UOffsetT

	for _, result := range results {
		if result == nil || result.Error != nil {
			continue
		}

		// Skip singletons - they're already in local storage, no proof needed
		if result.IsSingleton {
			continue
		}

		objOffset := a.buildObject(builder, result)
		objectOffsets = append(objectOffsets, objOffset)

		proofOffset, err := a.buildQuorumProof(builder, result)
		if err != nil {
			return nil, fmt.Errorf("build quorum proof:\n%w", err)
		}

		proofOffsets = append(proofOffsets, proofOffset)
	}

	var objectsVecOffset flatbuffers.UOffsetT

	if len(objectOffsets) > 0 {
		types.AttestedTransactionStartObjectsVector(builder, len(objectOffsets))

		for i := len(objectOffsets) - 1; i >= 0; i-- {
			builder.PrependUOffsetT(objectOffsets[i])
		}

		objectsVecOffset = builder.EndVector(len(objectOffsets))
	}

	var proofsVecOffset flatbuffers.UOffsetT

	if len(proofOffsets) > 0 {
		types.AttestedTransactionStartProofsVector(builder, len(proofOffsets))

		for i := len(proofOffsets) - 1; i >= 0; i-- {
			builder.PrependUOffsetT(proofOffsets[i])
		}

		proofsVecOffset = builder.EndVector(len(proofOffsets))
	}

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOffset)

	if objectsVecOffset != 0 {
		types.AttestedTransactionAddObjects(builder, objectsVecOffset)
	}

	if proofsVecOffset != 0 {
		types.AttestedTransactionAddProofs(builder, proofsVecOffset)
	}

	attestedTxOffset := types.AttestedTransactionEnd(builder)
	builder.Finish(attestedTxOffset)

	return builder.FinishedBytes(), nil
}

// buildObject creates a FlatBuffers Object from CollectionResult.
func (a *Aggregator) buildObject(builder *flatbuffers.Builder, result *CollectionResult) flatbuffers.UOffsetT {
	idOffset := builder.CreateByteVector(result.ObjectID[:])

	var contentOffset flatbuffers.UOffsetT

	if len(result.ObjectData) > 0 {
		contentOffset = builder.CreateByteVector(result.ObjectData)
	}

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idOffset)
	types.ObjectAddVersion(builder, 0) // TODO: Include actual version
	types.ObjectAddReplication(builder, result.Replication)

	if contentOffset != 0 {
		types.ObjectAddContent(builder, contentOffset)
	}

	return types.ObjectEnd(builder)
}

// buildQuorumProof creates a FlatBuffers QuorumProof from CollectionResult.
func (a *Aggregator) buildQuorumProof(builder *flatbuffers.Builder, result *CollectionResult) (flatbuffers.UOffsetT, error) {
	aggSig, err := AggregateSignatures(result.Signatures)
	if err != nil {
		return 0, fmt.Errorf("aggregate signatures:\n%w", err)
	}

	objectIDOffset := builder.CreateByteVector(result.ObjectID[:])
	sigOffset := builder.CreateByteVector(aggSig)
	bitmapOffset := builder.CreateByteVector(result.SignerMask)

	types.QuorumProofStart(builder)
	types.QuorumProofAddObjectId(builder, objectIDOffset)
	types.QuorumProofAddBlsSignature(builder, sigOffset)
	types.QuorumProofAddSignerBitmap(builder, bitmapOffset)

	return types.QuorumProofEnd(builder), nil
}
