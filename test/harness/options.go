package harness

// clusterOpts holds a Cluster's configuration, defaulted by NewCluster and
// customized by Option values.
type clusterOpts struct {
	epochLength       uint64 // epochLength is rounds per epoch (0 = node default)
	minValidators     int    // minValidators is the consensus threshold (0 = size)
	gossipFanout      int    // gossipFanout is peers per vertex gossip (0 = node default, or size for big clusters)
	syncBuffer        int    // syncBuffer is the sync buffer in seconds (0 = node default)
	initialMint       uint64 // initialMint is the bootstrap mint amount
	transitionGrace   int    // transitionGrace is grace rounds after minValidators is reached (0 = node default)
	transitionBuffer  int    // transitionBuffer is buffer rounds after the grace period (0 = node default)
	maxChurn          int    // maxChurn is the max validator changes per epoch (0 = unlimited)
	stake             uint64 // stake overrides the default equal per-validator bond amount (0 = computed)
	withoutStakeSetup bool   // withoutStakeSetup skips NewCluster's default stake bonding
	withoutInvariants bool   // withoutInvariants skips NewCluster's teardown invariant check
}

// Option configures a Cluster at construction.
type Option func(*clusterOpts)

// WithEpochLength sets the rounds per epoch.
func WithEpochLength(n uint64) Option {
	return func(o *clusterOpts) { o.epochLength = n }
}

// WithMinValidators sets the consensus threshold.
func WithMinValidators(n int) Option {
	return func(o *clusterOpts) { o.minValidators = n }
}

// WithGossipFanout sets the gossip fanout (peers per vertex).
func WithGossipFanout(n int) Option {
	return func(o *clusterOpts) { o.gossipFanout = n }
}

// WithSyncBuffer sets the sync buffer in seconds.
func WithSyncBuffer(n int) Option {
	return func(o *clusterOpts) { o.syncBuffer = n }
}

// WithInitialMint sets the bootstrap mint amount.
func WithInitialMint(n uint64) Option {
	return func(o *clusterOpts) { o.initialMint = n }
}

// WithTransitionGrace sets the grace rounds after minValidators is reached.
func WithTransitionGrace(n int) Option {
	return func(o *clusterOpts) { o.transitionGrace = n }
}

// WithTransitionBuffer sets the buffer rounds after the grace period.
func WithTransitionBuffer(n int) Option {
	return func(o *clusterOpts) { o.transitionBuffer = n }
}

// WithMaxChurn sets the max validator changes per epoch.
func WithMaxChurn(n int) Option {
	return func(o *clusterOpts) { o.maxChurn = n }
}

// WithStake overrides the default equal per-validator bond amount (for
// example 1_000_000), bypassing the reserve-fitted computation NewCluster
// otherwise derives from InitialMint.
func WithStake(amount uint64) Option {
	return func(o *clusterOpts) { o.stake = amount }
}

// WithoutStakeSetup skips NewCluster's default equal-stake bonding, leaving
// only the founder's genesis self-stake — the founder-heavy regime.
func WithoutStakeSetup() Option {
	return func(o *clusterOpts) { o.withoutStakeSetup = true }
}

// WithoutInvariants skips the automatic teardown invariant check, for a
// scenario that deliberately ends in a state the checker would reject (for
// example inside an open partition).
func WithoutInvariants() Option {
	return func(o *clusterOpts) { o.withoutInvariants = true }
}
