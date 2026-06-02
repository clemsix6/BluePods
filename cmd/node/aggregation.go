package main

import (
	"BluePods/internal/aggregation"
	"BluePods/internal/attest"
	"BluePods/internal/consensus"
	"BluePods/internal/logger"
)

// initAggregation initializes the aggregation subsystem.
func (n *Node) initAggregation(validators *consensus.ValidatorSet) {
	blsKey, err := aggregation.DeriveFromED25519(n.cfg.PrivateKey)
	if err != nil {
		logger.Warn("failed to derive BLS key, attestation handler disabled", "error", err)
		return
	}

	n.blsKey = blsKey
	n.rendezvous = aggregation.NewRendezvous(validators)

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

	// Attestation handler serves stored signatures and may sign on a bounded miss
	// for objects it holds at their current version.
	n.attHandler = aggregation.NewHandler(n.state, n.blsKey, n.storage, isHolder)

	// Wire object creation callback to tracker
	n.state.SetOnObjectCreated(func(id [32]byte, version uint64, replication uint16, fees uint64) {
		n.dag.TrackObject(id, version, replication, fees)
	})

	// Eager signing: at execution, a holder signs the persisted version and
	// stores it durably next to the object so attestation requests are pure reads.
	n.state.SetObjectSigner(func(id [32]byte, content []byte, version uint64, replication uint16) {
		hash := attest.ComputeObjectHash(content, version)
		sig := n.blsKey.Sign(hash[:])

		if err := aggregation.PutObjectSig(n.storage, id, version, sig); err != nil {
			logger.Warn("store object signature failed", "id_prefix", id[:4], "error", err)
		}
	})

	// Set up ATX proof verifier. The verifier selects the holder snapshot from
	// the attestation epoch carried on the ATX, validated against the commit
	// round's deterministic epoch (same epoch, or previous within grace).
	atxVerifier := aggregation.NewATXVerifier(aggregation.EpochResolver{
		HoldersForEpoch:     n.dag.HoldersForEpoch,
		CommitEpochForRound: n.dag.CommitEpochForRound,
		EpochLength:         n.dag.EpochLength(),
		GraceRounds:         consensus.EpochGraceRounds,
	})

	// Batch verifier: the round commit loop verifies a round's proofs across
	// cores in one parallel pass. SetATXProofVerifier stays wired as the inline
	// fallback, so a single-ATX verdict is identical either way.
	n.dag.SetATXProofVerifier(atxVerifier.Verify)
	n.dag.SetATXProofBatchVerifier(atxVerifier.VerifyBatch)

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

	// Bind the storage-deposit formula to the SAME live validator set consensus
	// reads, so the deposit stamped on a created object always equals the storage
	// fee debited at commit, across both validator-set growth and shrinkage.
	n.state.SetValidatorCount(validators.Len)
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

// scanObjectsForEpoch performs a background scan after epoch transitions.
// Identifies objects that need to be fetched or can be dropped.
func (n *Node) scanObjectsForEpoch() {
	myPubkey := n.myPubkey()
	isHolder := n.buildIsHolder(myPubkey)

	hasLocal := func(id [32]byte) bool {
		return n.state.GetObject(id) != nil
	}

	result := n.dag.ScanObjects(isHolder, hasLocal)

	// Fetch objects we should hold but don't have, over the QUIC mesh.
	for _, id := range result.NeedFetch {
		go func(objectID [32]byte) {
			data := n.fetchObjectFromHolder(objectID)
			if data == nil {
				logger.Debug("epoch scan: fetch failed", "id_prefix", objectID[:4])
				return
			}

			n.state.SetObject(data)
		}(id)
	}

	// Objects to drop are handled lazily (not deleted immediately)
}
