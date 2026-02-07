package aggregation

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"sync"
	"sync/atomic"

	"BluePods/internal/consensus"
	"BluePods/internal/network"
	"BluePods/internal/state"
	"BluePods/internal/types"
)

// Collector handles attestation collection from holders.
type Collector struct {
	node       *network.Node           // node is the network layer for peer communication
	rendezvous *Rendezvous             // rendezvous computes object-holder mappings
	validators *consensus.ValidatorSet // validators is the set of active validators
	state      *state.State            // state is the local storage for object lookup
}

// NewCollector creates a new attestation Collector.
func NewCollector(node *network.Node, rv *Rendezvous, vs *consensus.ValidatorSet, st *state.State) *Collector {
	return &Collector{
		node:       node,
		rendezvous: rv,
		validators: vs,
		state:      st,
	}
}

// CollectObject collects attestations for a single object.
// Flow:
//  1. Check local storage first for optimization
//  2. If singleton in local storage: bypass completely (no network)
//  3. If object in local storage (non-singleton): use local data, collect attestations only
//  4. Otherwise: request full object from top-1 holder and collect attestations
func (c *Collector) CollectObject(ctx context.Context, obj ObjectRef) *CollectionResult {
	result := &CollectionResult{ObjectID: obj.ID}

	// Step 1: Check local storage first
	if c.state != nil {
		localData := c.state.GetObject(obj.ID)
		if localData != nil {
			fbObj := types.GetRootAsObject(localData, 0)
			replication := fbObj.Replication()

			// Singleton in local storage → bypass completely
			if replication == 0 {
				result.IsSingleton = true
				return result
			}

			// Non-singleton exists locally → use local data, only collect attestations
			return c.collectAttestationsOnly(ctx, obj, localData, replication, fbObj.Version())
		}
	}

	// Step 2: Not in local storage → fetch from top-1
	top1Att, replication, err := c.fetchFromTop1(ctx, obj)
	if err != nil {
		result.Error = err
		return result
	}

	// Step 3: Check if singleton
	if replication == 0 {
		result.IsSingleton = true
		return result
	}

	// Step 4: Parallel collection from remaining holders
	remoteObj := types.GetRootAsObject(top1Att.ObjectData, 0)
	return c.collectFromRemainingHolders(ctx, obj, top1Att, replication, remoteObj.Version())
}

// fetchFromTop1 requests the full object from the top-1 holder.
// Returns the attestation, replication count, and any error.
func (c *Collector) fetchFromTop1(ctx context.Context, obj ObjectRef) (*HolderAttestation, uint16, error) {
	// Compute top-1 holder (use high replication to ensure we get the actual top-1)
	holders := c.rendezvous.ComputeHolders(obj.ID, c.validators.Len())
	if len(holders) == 0 {
		return nil, 0, fmt.Errorf("no validators available")
	}

	// Request from top-1 with full object data
	att := c.requestAttestation(ctx, holders[0], obj, true, 0)
	if att == nil {
		return nil, 0, fmt.Errorf("failed to fetch object from top-1 holder")
	}

	if att.IsNegative {
		return nil, 0, fmt.Errorf("top-1 holder does not have object")
	}

	if len(att.ObjectData) == 0 {
		return nil, 0, fmt.Errorf("top-1 holder returned empty object data")
	}

	// Parse object to extract replication count
	fbObj := types.GetRootAsObject(att.ObjectData, 0)
	replication := fbObj.Replication()

	return att, replication, nil
}

// collectFromRemainingHolders collects attestations from top-2 to top-N holders.
func (c *Collector) collectFromRemainingHolders(
	ctx context.Context,
	obj ObjectRef,
	top1Att *HolderAttestation,
	replication uint16,
	version uint64,
) *CollectionResult {
	result := &CollectionResult{
		ObjectID:    obj.ID,
		ObjectData:  top1Att.ObjectData,
		Version:     version,
		Replication: replication,
	}

	// Compute all holders for this replication factor
	holders := c.rendezvous.ComputeHolders(obj.ID, int(replication))
	if len(holders) == 0 {
		result.Error = fmt.Errorf("no holders for replication %d", replication)
		return result
	}

	quorum := QuorumSize(len(holders))

	// We already have top-1 attestation
	attestations := []*HolderAttestation{top1Att}

	// If we only need 1 attestation (quorum=1), we're done
	if quorum <= 1 {
		return c.finalizeResult(result, attestations, quorum, holders)
	}

	// Parallel collection from remaining holders (top-2 to top-N)
	remainingHolders := holders[1:]
	attestCh := make(chan *HolderAttestation, len(remainingHolders))

	var successCount atomic.Int32
	successCount.Store(1) // top-1 already succeeded

	var negativeCount atomic.Int32
	var wg sync.WaitGroup

	for i, holder := range remainingHolders {
		wg.Add(1)

		go func(idx int, h consensus.Hash) {
			defer wg.Done()

			// holderIndex is idx+1 because idx 0 is top-1
			att := c.requestAttestation(ctx, h, obj, false, idx+1)

			if att != nil {
				if att.IsNegative {
					negativeCount.Add(1)
				} else {
					successCount.Add(1)
				}
			}

			attestCh <- att
		}(i, holder)
	}

	// Close channel when all goroutines complete
	go func() {
		wg.Wait()
		close(attestCh)
	}()

	// Collect results until quorum or failure
	for att := range attestCh {
		if att == nil {
			continue
		}

		attestations = append(attestations, att)

		// Fail-fast on quorum of negatives
		if int(negativeCount.Load()) >= quorum {
			result.Error = fmt.Errorf("quorum of negative attestations")
			return result
		}

		// Early exit on quorum of positives
		if int(successCount.Load()) >= quorum {
			break
		}
	}

	return c.finalizeResult(result, attestations, quorum, holders)
}

// collectAttestationsOnly collects attestations when we already have the object locally.
// This skips the top-1 fetch entirely and goes straight to parallel attestation collection.
func (c *Collector) collectAttestationsOnly(
	ctx context.Context,
	obj ObjectRef,
	localData []byte,
	replication uint16,
	version uint64,
) *CollectionResult {
	result := &CollectionResult{
		ObjectID:    obj.ID,
		ObjectData:  localData,
		Version:     version,
		Replication: replication,
	}

	holders := c.rendezvous.ComputeHolders(obj.ID, int(replication))
	if len(holders) == 0 {
		result.Error = fmt.Errorf("no holders for replication %d", replication)
		return result
	}

	quorum := QuorumSize(len(holders))

	// Request attestations from ALL holders in parallel (no top-1 special case)
	attestCh := make(chan *HolderAttestation, len(holders))

	var successCount atomic.Int32
	var negativeCount atomic.Int32
	var wg sync.WaitGroup

	for i, holder := range holders {
		wg.Add(1)

		go func(idx int, h consensus.Hash) {
			defer wg.Done()

			att := c.requestAttestation(ctx, h, obj, false, idx)

			if att != nil {
				if att.IsNegative {
					negativeCount.Add(1)
				} else {
					successCount.Add(1)
				}
			}

			attestCh <- att
		}(i, holder)
	}

	go func() {
		wg.Wait()
		close(attestCh)
	}()

	var attestations []*HolderAttestation

	for att := range attestCh {
		if att == nil {
			continue
		}

		attestations = append(attestations, att)

		if int(negativeCount.Load()) >= quorum {
			result.Error = fmt.Errorf("quorum of negative attestations")
			return result
		}

		if int(successCount.Load()) >= quorum {
			break
		}
	}

	return c.finalizeResult(result, attestations, quorum, holders)
}

// requestAttestation sends an attestation request to a holder.
func (c *Collector) requestAttestation(
	ctx context.Context,
	holder consensus.Hash,
	obj ObjectRef,
	wantFull bool,
	holderIndex int,
) *HolderAttestation {
	peer := c.node.GetPeer(ed25519.PublicKey(holder[:]))
	if peer == nil {
		return nil
	}

	req := &AttestationRequest{
		ObjectID: obj.ID,
		Version:  obj.Version,
		WantFull: wantFull,
	}

	respData, err := peer.Request(ctx, EncodeRequest(req))
	if err != nil {
		return nil
	}

	return c.parseResponse(respData, obj.ID, holderIndex)
}

// parseResponse parses a holder's response into an attestation.
func (c *Collector) parseResponse(data []byte, objectID [32]byte, holderIndex int) *HolderAttestation {
	msgType, err := GetMessageType(data)
	if err != nil {
		return nil
	}

	switch msgType {
	case msgTypePositive:
		resp, err := DecodePositiveResponse(data)
		if err != nil {
			return nil
		}

		return &HolderAttestation{
			ObjectID:    objectID,
			Hash:        resp.Hash,
			Signature:   resp.Signature,
			ObjectData:  resp.Data,
			IsNegative:  false,
			HolderIndex: holderIndex,
		}

	case msgTypeNegative:
		resp, err := DecodeNegativeResponse(data)
		if err != nil {
			return nil
		}

		return &HolderAttestation{
			ObjectID:    objectID,
			Signature:   resp.Signature,
			IsNegative:  true,
			HolderIndex: holderIndex,
		}

	default:
		return nil
	}
}

// finalizeResult aggregates attestations into the final CollectionResult.
// Verifies each individual BLS signature and validates object hash integrity.
func (c *Collector) finalizeResult(
	result *CollectionResult,
	attestations []*HolderAttestation,
	quorum int,
	holders []consensus.Hash,
) *CollectionResult {
	// Filter positive attestations
	var positives []*HolderAttestation

	for _, att := range attestations {
		if !att.IsNegative {
			positives = append(positives, att)
		}
	}

	if len(positives) < quorum {
		result.Error = fmt.Errorf("insufficient attestations: got %d, need %d", len(positives), quorum)
		return result
	}

	// Verify all hashes match
	if len(positives) > 1 {
		expectedHash := positives[0].Hash

		for i := 1; i < len(positives); i++ {
			if positives[i].Hash != expectedHash {
				result.Error = fmt.Errorf("hash mismatch between holders")
				return result
			}
		}
	}

	// Verify each individual BLS signature against the holder's known BLS pubkey
	for _, att := range positives {
		if att.HolderIndex >= len(holders) {
			result.Error = fmt.Errorf("holder index %d out of range", att.HolderIndex)
			return result
		}

		holderInfo := c.validators.Get(holders[att.HolderIndex])
		if holderInfo == nil || holderInfo.BLSPubkey == [48]byte{} {
			// Skip verification if BLS pubkey not yet known (backward compat)
			continue
		}

		if !Verify(att.Signature, att.Hash[:], holderInfo.BLSPubkey[:]) {
			result.Error = fmt.Errorf("BLS signature from holder %d is invalid", att.HolderIndex)
			return result
		}
	}

	// Verify object data matches the attested hash
	if len(result.ObjectData) > 0 && len(positives) > 0 {
		expectedHash := ComputeObjectHash(result.ObjectData, result.Version)
		if expectedHash != positives[0].Hash {
			result.Error = fmt.Errorf("object data does not match attested hash")
			return result
		}
	}

	// Collect signatures and build signer bitmap
	result.Signatures = make([][]byte, len(positives))
	signerIndices := make([]int, len(positives))

	for i, att := range positives {
		result.Signatures[i] = att.Signature
		signerIndices[i] = att.HolderIndex
	}

	result.SignerMask = BuildSignerBitmap(signerIndices, int(result.Replication))

	return result
}
