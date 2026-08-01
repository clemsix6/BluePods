package consensus

import (
	"encoding/hex"
	"testing"

	"BluePods/internal/events"
	"BluePods/internal/genesis"
	"BluePods/internal/index"
	"BluePods/internal/state"
	"BluePods/internal/types"
)

// =============================================================================
// Registration
// =============================================================================

// TestDomainRegister_FirstComeFirstServed registers a bare root name and then
// asserts a second registration of the same live name — by anyone, including
// the owner — is rejected and leaves the leaf untouched.
func TestDomainRegister_FirstComeFirstServed(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	first := env.object(domainAlice, Hash{0x11})
	second := env.object(domainBob, Hash{0x22})

	buf := captureEvents(t)
	if !env.apply(domainAlice, 0, registerOp("alpha", first, 5)) {
		t.Fatal("registering an absent root name must succeed")
	}

	leaf := env.leaf(t, "alpha")
	if leaf.objectID != first || leaf.owner != domainAlice || leaf.expiry != 5 {
		t.Fatalf("leaf = %+v, want object %x owned by domainAlice expiring at 5", leaf, first[:2])
	}

	if env.apply(domainBob, 0, registerOp("alpha", second, 5)) {
		t.Fatal("registering a live name must be rejected")
	}
	if env.apply(domainAlice, 0, registerOp("alpha", second, 5)) {
		t.Fatal("re-registering a live name must be rejected even for its owner")
	}
	if got := env.leaf(t, "alpha"); got != leaf {
		t.Errorf("leaf changed on a rejected registration: %+v", got)
	}

	recs := eventsNamed(t, buf, events.EvDomainRegistered)
	if len(recs) != 1 || recs[0]["name"] != "alpha" {
		t.Fatalf("want exactly one %s for alpha, got %v", events.EvDomainRegistered, recs)
	}
	if recs[0]["object"] != hex.EncodeToString(first[:]) {
		t.Errorf("registered object = %v, want %s", recs[0]["object"], hex.EncodeToString(first[:]))
	}

	if len(env.idx.applied) != 1 || env.idx.applied[0].name != "alpha" || env.idx.applied[0].owner != domainAlice {
		t.Errorf("index feed = %+v, want one alpha leaf owned by domainAlice", env.idx.applied)
	}
}

// TestDomainRegister_DottedNameRequiresParentOwner enforces the namespace rule:
// x.y is registrable only by the owner of y, and only while y exists.
func TestDomainRegister_DottedNameRequiresParentOwner(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	domainAliceObj := env.object(domainAlice, Hash{0x11})
	domainBobObj := env.object(domainBob, Hash{0x22})

	if env.apply(domainAlice, 0, registerOp("app.alpha", domainAliceObj, 5)) {
		t.Fatal("a dotted name whose parent is unregistered must be rejected")
	}

	if !env.apply(domainAlice, 0, registerOp("alpha", domainAliceObj, 5)) {
		t.Fatal("registering the parent name must succeed")
	}

	if env.apply(domainBob, 0, registerOp("app.alpha", domainBobObj, 5)) {
		t.Fatal("a dotted name registered by a non-owner of the parent must be rejected")
	}
	if _, ok := env.domains.leaves["app.alpha"]; ok {
		t.Fatal("the rejected sub-name was written anyway")
	}

	if !env.apply(domainAlice, 0, registerOp("app.alpha", domainAliceObj, 5)) {
		t.Fatal("the parent's owner must be able to register the sub-name")
	}
}

// TestDomainRegister_ReservedAndMalformedNames rejects the reserved system
// namespace and names that are not well-formed.
func TestDomainRegister_ReservedAndMalformedNames(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	obj := env.object(domainAlice, Hash{0x11})

	for _, name := range []string{"system", "system.pod", "", ".", "a.", ".a", "a..b"} {
		if env.apply(domainAlice, 0, registerOp(name, obj, 5)) {
			t.Errorf("registering %q must be rejected", name)
		}
	}
}

// TestDomainRegister_RequiresControlOfPointedObject rejects naming an object
// the sender does not control, which would otherwise alias a victim's object
// into the domain-reference ownership exemption.
func TestDomainRegister_RequiresControlOfPointedObject(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	foreign := env.object(domainBob, Hash{0x22})

	if env.apply(domainAlice, 0, registerOp("alpha", foreign, 5)) {
		t.Fatal("registering a name onto a non-controlled object must be rejected")
	}
	if env.apply(domainAlice, 0, registerOp("alpha", Hash{0x99}, 5)) {
		t.Fatal("registering a name onto an untracked object must be rejected")
	}
	if len(env.domains.leaves) != 0 {
		t.Errorf("a leaf was written despite rejection: %+v", env.domains.leaves)
	}
}

// TestDomainRegister_TermCapReverts asserts a term beyond the cap reverts
// rather than being clamped: the rent charged is rate x the declared term, so a
// clamped term would charge for epochs the lease does not get.
func TestDomainRegister_TermCapReverts(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	obj := env.object(domainAlice, Hash{0x11})

	if env.apply(domainAlice, 0, registerOp("alpha", obj, uint32(defaultMaxTermEpochs+1))) {
		t.Fatal("a term beyond the cap must revert, never clamp")
	}
	if env.apply(domainAlice, 0, registerOp("alpha", obj, 0)) {
		t.Fatal("a zero-epoch term must be rejected")
	}
	if len(env.domains.leaves) != 0 {
		t.Errorf("a leaf was written despite the reverted term: %+v", env.domains.leaves)
	}

	if !env.apply(domainAlice, 7, registerOp("alpha", obj, uint32(defaultMaxTermEpochs))) {
		t.Fatal("a term exactly at the cap must be accepted")
	}
	if got := env.leaf(t, "alpha").expiry; got != 7+defaultMaxTermEpochs {
		t.Errorf("expiry = %d, want %d", got, 7+defaultMaxTermEpochs)
	}
}

// =============================================================================
// Renewal
// =============================================================================

// TestDomainRenew_OwnerOnlyAndCapped renews from the later of the current
// expiry and the current epoch, rejects a non-owner, and reverts a renewal that
// would push the lease past the cap.
func TestDomainRenew_OwnerOnlyAndCapped(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	obj := env.object(domainAlice, Hash{0x11})
	if !env.apply(domainAlice, 2, registerOp("alpha", obj, 4)) {
		t.Fatal("registration must succeed")
	}

	if env.apply(domainBob, 3, renewOp("alpha", 2)) {
		t.Fatal("a non-owner must not renew")
	}
	if got := env.leaf(t, "alpha").expiry; got != 6 {
		t.Fatalf("expiry = %d, want 6 (unchanged by the rejected renewal)", got)
	}

	buf := captureEvents(t)
	if !env.apply(domainAlice, 3, renewOp("alpha", 2)) {
		t.Fatal("the owner must be able to renew")
	}
	if got := env.leaf(t, "alpha").expiry; got != 8 {
		t.Errorf("expiry = %d, want 8 (prepaid from the current expiry)", got)
	}

	recs := eventsNamed(t, buf, events.EvDomainRenewed)
	if len(recs) != 1 || recs[0]["name"] != "alpha" || recs[0]["expiry"] != float64(8) {
		t.Fatalf("want one %s for alpha at expiry 8, got %v", events.EvDomainRenewed, recs)
	}

	if env.apply(domainAlice, 3, renewOp("alpha", uint32(defaultMaxTermEpochs))) {
		t.Fatal("a renewal past the cap must revert")
	}
	if got := env.leaf(t, "alpha").expiry; got != 8 {
		t.Errorf("expiry = %d, want 8 (unchanged by the reverted renewal)", got)
	}
}

// TestDomainRenew_DuringGrace asserts an expired name still renews for its
// owner (grace reserves exactly that right) and that the renewal restarts from
// the current epoch, never from the stale expiry.
func TestDomainRenew_DuringGrace(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	obj := env.object(domainAlice, Hash{0x11})
	if !env.apply(domainAlice, 0, registerOp("alpha", obj, 2)) {
		t.Fatal("registration must succeed")
	}

	if env.apply(domainBob, 5, renewOp("alpha", 2)) {
		t.Fatal("grace reserves renewal for the owner alone")
	}

	if !env.apply(domainAlice, 5, renewOp("alpha", 2)) {
		t.Fatal("the owner must be able to renew an expired name during grace")
	}
	if got := env.leaf(t, "alpha").expiry; got != 7 {
		t.Errorf("expiry = %d, want 7 (restarted from the current epoch)", got)
	}
}

// TestDomainRegister_ExpiredNameStillOccupied asserts an expired-but-unswept
// name is not re-registrable: the leaf lives until the epoch sweep removes it,
// and only then does the name become free.
func TestDomainRegister_ExpiredNameStillOccupied(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	domainAliceObj := env.object(domainAlice, Hash{0x11})
	domainBobObj := env.object(domainBob, Hash{0x22})

	if !env.apply(domainAlice, 0, registerOp("alpha", domainAliceObj, 1)) {
		t.Fatal("registration must succeed")
	}

	if env.apply(domainBob, 9, registerOp("alpha", domainBobObj, 1)) {
		t.Fatal("an expired but unswept name must not be re-registrable")
	}
	if env.leaf(t, "alpha").owner != domainAlice {
		t.Error("the expired leaf lost its owner")
	}
}

// TestDomainRegister_ExpiredParentGrantsNoNamespace asserts an expired parent
// name mints no sub-names: grace reserves renewal, not continued authority.
func TestDomainRegister_ExpiredParentGrantsNoNamespace(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	obj := env.object(domainAlice, Hash{0x11})
	if !env.apply(domainAlice, 0, registerOp("alpha", obj, 1)) {
		t.Fatal("registration must succeed")
	}

	if env.apply(domainAlice, 4, registerOp("app.alpha", obj, 1)) {
		t.Fatal("an expired parent name must grant no namespace authority")
	}
}

// =============================================================================
// Update, transfer, delete
// =============================================================================

// TestDomainUpdate_RepointsForOwnerOnly repoints a name at another controlled
// object, leaving owner and expiry alone, and rejects both a non-owner and a
// non-controlled target.
func TestDomainUpdate_RepointsForOwnerOnly(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	first := env.object(domainAlice, Hash{0x11})
	second := env.object(domainAlice, Hash{0x12})
	foreign := env.object(domainBob, Hash{0x22})

	if !env.apply(domainAlice, 1, registerOp("alpha", first, 4)) {
		t.Fatal("registration must succeed")
	}

	if env.apply(domainBob, 1, updateOp("alpha", foreign)) {
		t.Fatal("a non-owner must not update a name")
	}
	if env.apply(domainAlice, 1, updateOp("alpha", foreign)) {
		t.Fatal("repointing at a non-controlled object must be rejected")
	}

	buf := captureEvents(t)
	if !env.apply(domainAlice, 1, updateOp("alpha", second)) {
		t.Fatal("the owner must be able to repoint the name")
	}

	leaf := env.leaf(t, "alpha")
	if leaf.objectID != second || leaf.owner != domainAlice || leaf.expiry != 5 {
		t.Errorf("leaf = %+v, want the second object with owner and expiry unchanged", leaf)
	}

	recs := eventsNamed(t, buf, events.EvDomainUpdated)
	if len(recs) != 1 || recs[0]["object"] != hex.EncodeToString(second[:]) {
		t.Fatalf("want one %s naming the new object, got %v", events.EvDomainUpdated, recs)
	}
	if last := env.idx.applied[len(env.idx.applied)-1]; last.objectID != second {
		t.Errorf("index feed last apply = %+v, want the repointed object", last)
	}
}

// TestDomainTransfer_HandsRenewalRights transfers a name and asserts the rights
// move with it: the new owner renews, the old one cannot.
func TestDomainTransfer_HandsRenewalRights(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	obj := env.object(domainAlice, Hash{0x11})
	if !env.apply(domainAlice, 1, registerOp("alpha", obj, 4)) {
		t.Fatal("registration must succeed")
	}

	if env.apply(domainBob, 1, transferOp("alpha", domainBob)) {
		t.Fatal("a non-owner must not transfer a name")
	}
	if env.apply(domainAlice, 1, transferOp("alpha", Hash{})) {
		t.Fatal("transferring to the zero key must be rejected")
	}

	buf := captureEvents(t)
	if !env.apply(domainAlice, 1, transferOp("alpha", domainBob)) {
		t.Fatal("the owner must be able to transfer the name")
	}

	leaf := env.leaf(t, "alpha")
	if leaf.owner != domainBob || leaf.objectID != obj || leaf.expiry != 5 {
		t.Fatalf("leaf = %+v, want only the owner changed", leaf)
	}

	recs := eventsNamed(t, buf, events.EvDomainTransferred)
	if len(recs) != 1 || recs[0]["owner"] != hex.EncodeToString(domainBob[:]) {
		t.Fatalf("want one %s naming the new owner, got %v", events.EvDomainTransferred, recs)
	}

	if env.apply(domainAlice, 1, renewOp("alpha", 1)) {
		t.Error("the former owner must lose the renewal right")
	}
	if !env.apply(domainBob, 1, renewOp("alpha", 1)) {
		t.Error("the new owner must gain the renewal right")
	}
}

// TestDomainDelete_OwnerOnly deletes a name, asserting the leaf leaves both the
// store and the index and that a non-owner cannot.
func TestDomainDelete_OwnerOnly(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	obj := env.object(domainAlice, Hash{0x11})
	if !env.apply(domainAlice, 1, registerOp("alpha", obj, 4)) {
		t.Fatal("registration must succeed")
	}

	if env.apply(domainBob, 1, deleteDomainOp("alpha")) {
		t.Fatal("a non-owner must not delete a name")
	}
	if _, ok := env.domains.leaves["alpha"]; !ok {
		t.Fatal("the leaf was removed by a rejected delete")
	}

	buf := captureEvents(t)
	if !env.apply(domainAlice, 1, deleteDomainOp("alpha")) {
		t.Fatal("the owner must be able to delete the name")
	}
	if _, ok := env.domains.leaves["alpha"]; ok {
		t.Error("the leaf survived the delete")
	}

	recs := eventsNamed(t, buf, events.EvDomainDeleted)
	if len(recs) != 1 || recs[0]["name"] != "alpha" {
		t.Fatalf("want one %s for alpha, got %v", events.EvDomainDeleted, recs)
	}
	if len(env.idx.removed) != 1 || env.idx.removed[0] != "alpha" {
		t.Errorf("index removals = %v, want [alpha]", env.idx.removed)
	}

	if env.apply(domainAlice, 1, renewOp("alpha", 1)) {
		t.Error("a deleted name must not be renewable")
	}
	if !env.apply(domainBob, 1, registerOp("alpha", env.object(domainBob, Hash{0x22}), 1)) {
		t.Error("a deleted name must be registrable again")
	}
}

// =============================================================================
// List semantics
// =============================================================================

// TestDomainOps_IntraTxStagingAndRollback asserts a multi-operation list sees
// its own effects in order — a sub-name registered under a parent registered by
// the same transaction — and that a later failure discards the whole list.
func TestDomainOps_IntraTxStagingAndRollback(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	obj := env.object(domainAlice, Hash{0x11})

	if !env.apply(domainAlice, 0, registerOp("alpha", obj, 3), registerOp("app.alpha", obj, 3)) {
		t.Fatal("a sub-name registered under a parent from the same list must apply")
	}
	if _, ok := env.domains.leaves["app.alpha"]; !ok {
		t.Fatal("the sub-name was not written")
	}

	before := env.leaf(t, "alpha")
	if env.apply(domainAlice, 0, renewOp("alpha", 1), registerOp("app.alpha", obj, 3)) {
		t.Fatal("a list whose second operation fails must be rejected wholesale")
	}
	if got := env.leaf(t, "alpha"); got != before {
		t.Errorf("leaf = %+v, want %+v (the renewal must not survive the failed list)", got, before)
	}
}

// TestDomainOps_TouchNoObjectVersion asserts domain operations carry no object
// reference: the named object's version is untouched, and the transaction needs
// no mutable ref to name it.
func TestDomainOps_TouchNoObjectVersion(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	obj := env.object(domainAlice, Hash{0x11})
	env.dag.tracker.trackObject(obj, 4, 0, 0, keyRootKind, domainAlice)

	if !env.apply(domainAlice, 0, registerOp("alpha", obj, 3)) {
		t.Fatal("registration must succeed without a mutable ref")
	}
	if v := env.dag.tracker.getVersion(obj); v != 4 {
		t.Errorf("object version = %d, want 4 (domain operations mutate no object)", v)
	}
}

// TestDomainOps_RejectedWithoutDomainStore asserts a DAG with no domain store
// wired fails domain operations closed instead of panicking.
func TestDomainOps_RejectedWithoutDomainStore(t *testing.T) {
	dag := opsTestDAG(t)
	defer dag.Close()

	obj := Hash{0x11}
	dag.tracker.trackObject(obj, 0, 0, 0, keyRootKind, domainAlice)

	tx := opsTx(t, domainAlice, "", nil, nil, []genesis.DeclaredOp{registerOp("alpha", obj, 3)}, Hash{0xD0})
	if dag.handleDeclaredOps(tx, 0) {
		t.Fatal("a domain operation without a domain store must be rejected")
	}
}

// TestExecuteTx_DomainRegisterCommitsAtRoundEpoch drives a registration through
// the full commit path and asserts the lease is stamped with the epoch the
// COMMIT round belongs to, the same value on every node, not a live clock read.
func TestExecuteTx_DomainRegisterCommitsAtRoundEpoch(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	env.dag.epochLength = 10
	obj := env.object(domainAlice, Hash{0x11})

	ops := []genesis.DeclaredOp{registerOp("alpha", obj, 3)}
	atx := types.GetRootAsAttestedTransaction(buildOpsATX(t, domainAlice, "", nil, nil, ops, Hash{0xD1}), 0)

	buf := captureEvents(t)
	env.dag.executeTx(atx, 25, env.producer, nil, Hash{0xE1})

	assertSingleTxCommitted(t, buf, 25, true, "", Hash{0xE1})
	if got := env.leaf(t, "alpha").expiry; got != 5 {
		t.Errorf("expiry = %d, want 5 (epoch 2 for round 25, plus a 3-epoch term)", got)
	}
}

// =============================================================================
// Test helpers
// =============================================================================

// domainAlice and domainBob are the two controlling keys the domain tests contend over.
var (
	domainAlice = Hash{0xA1}
	domainBob   = Hash{0xB2}
)

// domainLeafRecord is the stored shape the mock domain store keeps, mirroring
// the state package's persisted leaf.
type domainLeafRecord struct {
	objectID Hash
	owner    Hash
	expiry   uint64
}

// mockDomainStore is an in-memory DomainStore, so the commit-path rules are
// tested without a Pebble-backed state.
type mockDomainStore struct {
	leaves map[string]domainLeafRecord
}

func newMockDomainStore() *mockDomainStore {
	return &mockDomainStore{leaves: make(map[string]domainLeafRecord)}
}

func (m *mockDomainStore) DomainLeaf(name string) ([32]byte, [32]byte, uint64, bool) {
	leaf, ok := m.leaves[name]

	return leaf.objectID, leaf.owner, leaf.expiry, ok
}

func (m *mockDomainStore) SetDomainLeaf(name string, objectID, owner [32]byte, expiry uint64) {
	m.leaves[name] = domainLeafRecord{objectID: objectID, owner: owner, expiry: expiry}
}

func (m *mockDomainStore) DeleteDomainLeaf(name string) {
	delete(m.leaves, name)
}

// ExportDomains returns every leaf the mock holds, in Go's randomized map
// order — exercising this on purpose so a sweep test that assumes sorted
// input without sorting it itself fails regardless of which order the map
// happens to yield on a given run.
func (m *mockDomainStore) ExportDomains() []state.DomainEntry {
	entries := make([]state.DomainEntry, 0, len(m.leaves))
	for name, leaf := range m.leaves {
		entries = append(entries, state.DomainEntry{
			Name:        name,
			ObjectID:    state.Hash(leaf.objectID),
			Owner:       state.Hash(leaf.owner),
			ExpiryEpoch: leaf.expiry,
		})
	}

	return entries
}

// domainIndexer records the domain leg of the index feed, so a test can assert
// every leaf mutation reaches the tree the anchored root is computed over.
type domainIndexer struct {
	applied []domainLeafFeed
	removed []string
}

// domainLeafFeed is one recorded ApplyDomain call.
type domainLeafFeed struct {
	name     string
	objectID Hash
	owner    Hash
	expiry   uint64
}

func (d *domainIndexer) BuildFromState(tracker []index.TrackerEntry, domains []index.DomainLeaf, validators []index.ValidatorLeaf) {
}

func (d *domainIndexer) ApplyEdge(child [32]byte, kind byte, parent [32]byte) {}

func (d *domainIndexer) RemoveObject(child [32]byte) {}

func (d *domainIndexer) ApplyDomain(name string, objectID, owner [32]byte, expiryEpoch uint64) {
	d.applied = append(d.applied, domainLeafFeed{name: name, objectID: objectID, owner: owner, expiry: expiryEpoch})
}

func (d *domainIndexer) RemoveDomain(name string) {
	d.removed = append(d.removed, name)
}

func (d *domainIndexer) RebuildValidators(entries []index.ValidatorLeaf) {}

func (d *domainIndexer) SetFrontier(round uint64) {}

func (d *domainIndexer) CommittedFrontier() (uint64, [32]byte) { return 0, [32]byte{} }

func (d *domainIndexer) RootAt(round uint64) ([32]byte, bool) { return [32]byte{}, false }

// domainEnv is one domain test's world: a DAG wired to an in-memory domain
// store and a recording index feed.
type domainEnv struct {
	t        *testing.T
	dag      *DAG
	domains  *mockDomainStore
	idx      *domainIndexer
	producer Hash
}

// newDomainEnv builds a DAG with commit authenticity disabled, a domain store,
// and a recording indexer. The governed fee parameters are wired at
// construction, as every production path wires them: the lease cap and the
// grace window are read off them with no fallback (mustFeeParams), so a domain
// test on an unwired DAG panics instead of exercising anything.
func newDomainEnv(t *testing.T) *domainEnv {
	t.Helper()

	validators, vs := newTestValidatorSet(3)
	params := DefaultFeeParams()
	dag := New(newTestStorage(t), vs, &mockBroadcaster{}, testSystemPod, 0, validators[0].privKey, nil, WithFeeParams(&params))
	disableTxAuth(dag)

	env := &domainEnv{t: t, dag: dag, domains: newMockDomainStore(), idx: &domainIndexer{}, producer: validators[0].pubKey}
	dag.SetDomainStore(env.domains)
	dag.SetIndexer(env.idx)

	return env
}

// object tracks an object under owner and returns its ID.
func (e *domainEnv) object(owner, id Hash) Hash {
	e.dag.tracker.trackObject(id, 0, 0, 0, keyRootKind, owner)

	return id
}

// apply runs one declared-operation transaction at the given epoch and reports
// whether it applied.
func (e *domainEnv) apply(sender Hash, epoch uint64, ops ...genesis.DeclaredOp) bool {
	tx := opsTx(e.t, sender, "", nil, nil, ops, Hash{0xD0, byte(len(e.domains.leaves))})

	return e.dag.handleDeclaredOps(tx, epoch)
}

// leaf reads a name's stored leaf, failing the test when it is absent.
func (e *domainEnv) leaf(t *testing.T, name string) domainLeafRecord {
	t.Helper()

	leaf, ok := e.domains.leaves[name]
	if !ok {
		t.Fatalf("domain %q is not registered", name)
	}

	return leaf
}

// registerOp builds a domain_register operation.
func registerOp(name string, objectID Hash, term uint32) genesis.DeclaredOp {
	return genesis.DeclaredOp{Kind: domainRegisterOp, ObjectID: objectID[:], Name: name, TermEpochs: term}
}

// renewOp builds a domain_renew operation.
func renewOp(name string, term uint32) genesis.DeclaredOp {
	return genesis.DeclaredOp{Kind: domainRenewOp, Name: name, TermEpochs: term}
}

// updateOp builds a domain_update operation.
func updateOp(name string, objectID Hash) genesis.DeclaredOp {
	return genesis.DeclaredOp{Kind: domainUpdateOp, ObjectID: objectID[:], Name: name}
}

// transferOp builds a domain_transfer operation.
func transferOp(name string, owner Hash) genesis.DeclaredOp {
	return genesis.DeclaredOp{Kind: domainTransferOp, Name: name, Target: owner[:]}
}

// deleteDomainOp builds a domain_delete operation.
func deleteDomainOp(name string) genesis.DeclaredOp {
	return genesis.DeclaredOp{Kind: domainDeleteOp, Name: name}
}
