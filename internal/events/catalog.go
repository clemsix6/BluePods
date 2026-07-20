package events

// Event name constants. Each is the stable, dotted name emitted by exactly one
// constructor in this package (see node.go, ingress.go, consensus.go, tx.go,
// state.go, fees.go, stake.go, supply.go, epoch.go, sync.go, net.go). Names and
// their existing attributes are stable; renaming or removing one is a breaking
// change and must be called out in the commit that does it.
const (
	// EvNodeStarted marks a node beginning startup.
	EvNodeStarted = "node.started"
	// EvNodeReady marks a node's listener coming up.
	EvNodeReady = "node.ready"
	// EvNodeStopping marks a node beginning shutdown.
	EvNodeStopping = "node.stopping"

	// EvIngressTxReceived marks a submission accepted into the mempool path.
	EvIngressTxReceived = "ingress.tx.received"
	// EvIngressTxRejected marks a submission rejected before consensus.
	EvIngressTxRejected = "ingress.tx.rejected"

	// EvVertexProduced marks a vertex signed and stored by this node.
	EvVertexProduced = "consensus.vertex.produced"
	// EvVertexReceived marks a remote vertex accepted into the DAG.
	EvVertexReceived = "consensus.vertex.received"
	// EvVertexRejected marks a vertex terminally rejected during validation.
	EvVertexRejected = "consensus.vertex.rejected"
	// EvRoundAdvanced marks the production round observed from the commit loop
	// (NOT the commit cursor) advancing.
	EvRoundAdvanced = "consensus.round.advanced"
	// EvAnchorCommitted marks an anchor decided at a round.
	EvAnchorCommitted = "consensus.anchor.committed"
	// EvRoundSkipped marks a round decided with no anchor.
	EvRoundSkipped = "consensus.round.skipped"

	// EvTxCommitted marks a transaction decided at commit.
	EvTxCommitted = "tx.committed"
	// EvTxExecuted marks the executor call for a transaction returning.
	EvTxExecuted = "tx.executed"

	// EvObjectCreated marks a new object stored.
	EvObjectCreated = "state.object.created"
	// EvObjectUpdated marks an existing object's content replaced.
	EvObjectUpdated = "state.object.updated"
	// EvObjectDeleted marks an object removed from state.
	EvObjectDeleted = "state.object.deleted"
	// EvObjectReparented marks an object's parent edge changed by a declared
	// reparent operation (a transfer is a reparent to a KeyRoot).
	EvObjectReparented = "state.object.reparented"
	// EvDomainRegistered marks a new domain name bound to an object.
	EvDomainRegistered = "state.domain.registered"
	// EvDomainUpdated marks a domain name rebound to a different object.
	EvDomainUpdated = "state.domain.updated"
	// EvDomainDeleted marks a domain name removed from the registry.
	EvDomainDeleted = "state.domain.deleted"

	// EvFeesDeducted marks a fee taken from a coin.
	EvFeesDeducted = "fees.deducted"
	// EvDepositLocked marks a storage deposit stamped on a tracked object.
	EvDepositLocked = "fees.deposit.locked"
	// EvDepositRefunded marks a locked deposit credited back on deletion.
	EvDepositRefunded = "fees.deposit.refunded"

	// EvStakeBonded marks a validator raising its self-stake.
	EvStakeBonded = "stake.bonded"
	// EvStakeUnbonded marks a validator lowering its self-stake.
	EvStakeUnbonded = "stake.unbonded"
	// EvStakeDelegated marks a new delegation position opened.
	EvStakeDelegated = "stake.delegated"
	// EvStakeUndelegated marks a delegation position closed.
	EvStakeUndelegated = "stake.undelegated"
	// EvStakeReleased marks a deregistered validator's self-stake returned to its
	// reward coin at the epoch boundary.
	EvStakeReleased = "stake.released"

	// EvSupplyIssued marks new supply minted into the reward pool.
	EvSupplyIssued = "supply.issued"
	// EvSupplyBurned marks supply permanently removed.
	EvSupplyBurned = "supply.burned"

	// EvEpochTransitioned marks the epoch boundary landing.
	EvEpochTransitioned = "epoch.transitioned"
	// EvValidatorRegistered marks a validator (re-)registering.
	EvValidatorRegistered = "epoch.validator.registered"
	// EvValidatorDeregistered marks a validator scheduled for removal.
	EvValidatorDeregistered = "epoch.validator.deregistered"
	// EvRewardsDistributed marks an epoch's issuance pool credited.
	EvRewardsDistributed = "epoch.rewards.distributed"

	// EvSnapshotCreated marks a state snapshot exported.
	EvSnapshotCreated = "sync.snapshot.created"
	// EvSnapshotApplied marks a received snapshot applied to local state.
	EvSnapshotApplied = "sync.snapshot.applied"
	// EvSyncCompleted marks a node finishing catch-up sync.
	EvSyncCompleted = "sync.completed"

	// EvPeerConnected marks a QUIC peer connection established.
	EvPeerConnected = "net.peer.connected"
	// EvPeerDisconnected marks a QUIC peer connection torn down.
	EvPeerDisconnected = "net.peer.disconnected"
	// EvPartitionApplied marks the local blocklist populated.
	EvPartitionApplied = "net.partition.applied"
	// EvPartitionCleared marks the local blocklist emptied.
	EvPartitionCleared = "net.partition.cleared"
)

// Names lists every event name in the catalog, for tests and tooling that need
// to enumerate the full taxonomy.
var Names = []string{
	EvNodeStarted,
	EvNodeReady,
	EvNodeStopping,

	EvIngressTxReceived,
	EvIngressTxRejected,

	EvVertexProduced,
	EvVertexReceived,
	EvVertexRejected,
	EvRoundAdvanced,
	EvAnchorCommitted,
	EvRoundSkipped,

	EvTxCommitted,
	EvTxExecuted,

	EvObjectCreated,
	EvObjectUpdated,
	EvObjectDeleted,
	EvObjectReparented,
	EvDomainRegistered,
	EvDomainUpdated,
	EvDomainDeleted,

	EvFeesDeducted,
	EvDepositLocked,
	EvDepositRefunded,

	EvStakeBonded,
	EvStakeUnbonded,
	EvStakeDelegated,
	EvStakeUndelegated,
	EvStakeReleased,

	EvSupplyIssued,
	EvSupplyBurned,

	EvEpochTransitioned,
	EvValidatorRegistered,
	EvValidatorDeregistered,
	EvRewardsDistributed,

	EvSnapshotCreated,
	EvSnapshotApplied,
	EvSyncCompleted,

	EvPeerConnected,
	EvPeerDisconnected,
	EvPartitionApplied,
	EvPartitionCleared,
}
