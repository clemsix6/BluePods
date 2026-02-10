package main

import (
	"bytes"
	"fmt"

	"BluePods/internal/aggregation"
	"BluePods/internal/consensus"
	"BluePods/internal/logger"
	"BluePods/internal/types"
)

// initAggregation initializes the aggregation subsystem.
func (n *Node) initAggregation(validators *consensus.ValidatorSet) {
	blsKey, err := aggregation.DeriveFromED25519(n.cfg.PrivateKey)
	if err != nil {
		logger.Warn("failed to derive BLS key, attestation handler disabled", "error", err)
		return
	}

	n.blsKey = blsKey
	n.attHandler = aggregation.NewHandler(n.state, n.blsKey)
	n.rendezvous = aggregation.NewRendezvous(validators)
	n.aggregator = aggregation.NewAggregator(n.network, validators, n.state)

	// Initialize epoch holders and set epoch transition callback
	if n.dag != nil {
		n.dag.InitEpochHolders()

		n.dag.OnEpochTransition(func(epoch uint64) {
			// Rebuild Rendezvous with the new epoch holders
			epochHolders := n.dag.EpochHolders()
			n.rendezvous = aggregation.NewRendezvous(epochHolders)

			logger.Info("rendezvous rebuilt for epoch",
				"epoch", epoch,
				"holders", epochHolders.Len(),
			)

			// Background scan for object redistribution
			go n.scanObjectsForEpoch()
		})
	}

	// Set up isHolder closure for execution and storage sharding
	myPubkey := n.myPubkey()
	isHolder := n.buildIsHolder(myPubkey)

	n.dag.SetIsHolder(isHolder)
	n.state.SetIsHolder(isHolder)

	// Wire object creation callback to tracker
	n.state.SetOnObjectCreated(func(id [32]byte, version uint64, replication uint16, fees uint64) {
		n.dag.TrackObject(id, version, replication, fees)
	})

	// Set up ATX proof verifier
	n.dag.SetATXProofVerifier(n.buildATXVerifier(validators))

	// Set up fee system
	n.initFeeSystem(validators)

	logger.Info("aggregation initialized")
}

// initFeeSystem configures protocol-level fee deduction and storage deposits.
func (n *Node) initFeeSystem(validators *consensus.ValidatorSet) {
	feeParams := consensus.DefaultFeeParams()

	// Build holder computation closure for replication ratio
	computeHolders := func(objectID [32]byte, replication int) []consensus.Hash {
		return n.rendezvous.ComputeHolders(objectID, replication)
	}

	// Wire into DAG for fee deduction at commit
	n.dag.SetFeeSystem(n.state, &feeParams, computeHolders)

	// Wire into state for storage deposits on object creation/deletion
	n.state.SetStorageFees(
		feeParams.StorageFee,
		feeParams.StorageRefundBPS,
		validators.Len(),
	)
}

// buildIsHolder creates a closure that checks if this node is a holder for an object.
func (n *Node) buildIsHolder(myPubkey consensus.Hash) func(objectID [32]byte, replication uint16) bool {
	return func(objectID [32]byte, replication uint16) bool {
		// Singletons: all validators hold them
		if replication == 0 {
			return true
		}

		holders := n.rendezvous.ComputeHolders(objectID, int(replication))
		for _, h := range holders {
			if h == myPubkey {
				return true
			}
		}

		return false
	}
}

// buildATXVerifier returns a closure that verifies all BLS quorum proofs in an ATX.
func (n *Node) buildATXVerifier(validators *consensus.ValidatorSet) func(*types.AttestedTransaction) error {
	return func(atx *types.AttestedTransaction) error {
		var proof types.QuorumProof

		for i := 0; i < atx.ProofsLength(); i++ {
			if !atx.Proofs(&proof, i) {
				return fmt.Errorf("cannot read proof %d", i)
			}

			if err := n.verifySingleProof(atx, &proof, validators); err != nil {
				return fmt.Errorf("proof %d:\n%w", i, err)
			}
		}

		return nil
	}
}

// verifySingleProof verifies one QuorumProof against the ATX objects and validator BLS keys.
func (n *Node) verifySingleProof(atx *types.AttestedTransaction, proof *types.QuorumProof, validators *consensus.ValidatorSet) error {
	// Find the matching object in the ATX
	objIdx := findATXObjectIndex(atx, proof.ObjectIdBytes())
	if objIdx < 0 {
		return fmt.Errorf("object not found in ATX")
	}

	var obj types.Object
	if !atx.Objects(&obj, objIdx) {
		return fmt.Errorf("cannot read object at index %d", objIdx)
	}

	// Recompute expected hash from object data
	hash := aggregation.ComputeObjectHash(obj.ContentBytes(), obj.Version())

	// Compute holders using rendezvous
	var objectID [32]byte
	copy(objectID[:], proof.ObjectIdBytes())
	holders := n.rendezvous.ComputeHolders(objectID, int(obj.Replication()))

	// Extract signer BLS keys from bitmap
	blsKeys, signerCount := extractSignerBLSKeys(proof.SignerBitmapBytes(), holders, validators)
	if signerCount == 0 {
		return fmt.Errorf("no signers in bitmap")
	}

	// Verify quorum
	quorum := aggregation.QuorumSize(len(holders))
	if signerCount < quorum {
		return fmt.Errorf("insufficient signers: got %d, need %d", signerCount, quorum)
	}

	// Verify aggregated BLS signature
	if !aggregation.VerifyAggregated(proof.BlsSignatureBytes(), hash[:], blsKeys) {
		return fmt.Errorf("aggregated BLS signature invalid")
	}

	return nil
}

// findATXObjectIndex returns the index of the object with the given ID in the ATX, or -1.
func findATXObjectIndex(atx *types.AttestedTransaction, objectID []byte) int {
	var obj types.Object

	for i := 0; i < atx.ObjectsLength(); i++ {
		if !atx.Objects(&obj, i) {
			continue
		}

		if bytes.Equal(obj.IdBytes(), objectID) {
			return i
		}
	}

	return -1
}

// extractSignerBLSKeys maps a signer bitmap to BLS public keys via rendezvous holders.
// Returns the BLS keys and the number of signers.
func extractSignerBLSKeys(bitmap []byte, holders []consensus.Hash, validators *consensus.ValidatorSet) ([][]byte, int) {
	indices := aggregation.ParseSignerBitmap(bitmap)
	var keys [][]byte

	for _, idx := range indices {
		if idx >= len(holders) {
			continue
		}

		info := validators.Get(holders[idx])
		if info == nil || info.BLSPubkey == [48]byte{} {
			continue
		}

		keys = append(keys, info.BLSPubkey[:])
	}

	return keys, len(keys)
}

// scanObjectsForEpoch performs a background scan after epoch transitions.
// Identifies objects that need to be fetched or can be dropped.
func (n *Node) scanObjectsForEpoch() {
	myPubkey := n.myPubkey()
	isHolder := n.buildIsHolder(myPubkey)

	hasLocal := func(id [32]byte) bool {
		return n.state.GetObject(id) != nil
	}

	result := n.dag.ScanObjects(isHolder, hasLocal)

	// Fetch objects we should hold but don't have
	for _, id := range result.NeedFetch {
		go func(objectID [32]byte) {
			if hr := n.newHolderRouter(); hr != nil {
				data, err := hr.RouteGetObject(objectID)
				if err != nil {
					logger.Debug("epoch scan: fetch failed", "id_prefix", objectID[:4], "error", err)
					return
				}

				n.state.SetObject(data)
			}
		}(id)
	}

	// Objects to drop are handled lazily (not deleted immediately)
}
