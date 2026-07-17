package aggregation

import (
	"testing"

	"BluePods/internal/attest"
	"BluePods/internal/types"
	"BluePods/internal/validators"

	flatbuffers "github.com/google/flatbuffers/go"
)

// buildTestObject creates a FlatBuffers Object with the given replication.
func buildTestObject(id [32]byte, replication uint16) []byte {
	builder := flatbuffers.NewBuilder(256)

	idOffset := builder.CreateByteVector(id[:])
	contentOffset := builder.CreateByteVector([]byte("test content"))

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idOffset)
	types.ObjectAddVersion(builder, 1)
	types.ObjectAddReplication(builder, replication)
	types.ObjectAddContent(builder, contentOffset)

	objOffset := types.ObjectEnd(builder)
	builder.Finish(objOffset)

	return builder.FinishedBytes()
}

// buildVerifierFixture builds a validator set whose BLS keys are derived from a
// seed, plus a one-proof ATX over a replicated object stamped with attestationEpoch.
// It returns the validator set and the serialized ATX.
func buildVerifierFixture(t *testing.T, attestationEpoch uint64) (*validators.ValidatorSet, []byte) {
	t.Helper()

	const n = 4
	const replication = n

	vs := validators.NewValidatorSet(nil)
	keys := make([]*attest.BLSKeyPair, n)

	for i := 0; i < n; i++ {
		var pubkey validators.Hash
		pubkey[0] = byte(i + 1)

		seed := make([]byte, 32)
		seed[0] = byte(i + 1)
		key, err := attest.GenerateBLSKeyFromSeed(seed)
		if err != nil {
			t.Fatalf("derive bls key %d: %v", i, err)
		}
		keys[i] = key

		var blsPub [48]byte
		copy(blsPub[:], key.PublicKeyBytes())
		vs.Add(pubkey, "addr", blsPub)
	}

	objID := [32]byte{0x42}
	objData := buildTestObject(objID, replication)
	obj := types.GetRootAsObject(objData, 0)
	hash := attest.ComputeObjectHash(obj.ContentBytes(), obj.Version())

	holders := attest.ComputeHolders(vs, objID, replication)

	// All holders sign; aggregate their signatures and build the bitmap.
	var sigs [][]byte
	var indices []int
	for i, h := range holders {
		info := vs.Get(h)
		for k := 0; k < n; k++ {
			var pk validators.Hash
			pk[0] = byte(k + 1)
			if info.Pubkey == pk {
				sigs = append(sigs, keys[k].Sign(hash[:]))
				indices = append(indices, i)
			}
		}
	}

	aggSig, err := attest.AggregateSignatures(sigs)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	bitmap := attest.BuildSignerBitmap(indices, len(holders))

	return vs, buildATXWithProof(objID, objData, aggSig, bitmap, attestationEpoch)
}

// buildATXWithProof assembles a single-proof ATX carrying the object, the
// aggregated signature, the signer bitmap, and an attestation epoch.
func buildATXWithProof(objID [32]byte, objData, aggSig, bitmap []byte, attestationEpoch uint64) []byte {
	builder := flatbuffers.NewBuilder(1024)

	txOffset := serializeMinimalTx(builder)

	obj := types.GetRootAsObject(objData, 0)
	idVec := builder.CreateByteVector(obj.IdBytes())
	contentVec := builder.CreateByteVector(obj.ContentBytes())
	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, obj.Version())
	types.ObjectAddReplication(builder, obj.Replication())
	types.ObjectAddContent(builder, contentVec)
	objOffset := types.ObjectEnd(builder)

	types.AttestedTransactionStartObjectsVector(builder, 1)
	builder.PrependUOffsetT(objOffset)
	objectsVec := builder.EndVector(1)

	objIDVec := builder.CreateByteVector(objID[:])
	sigVec := builder.CreateByteVector(aggSig)
	bitmapVec := builder.CreateByteVector(bitmap)
	types.QuorumProofStart(builder)
	types.QuorumProofAddObjectId(builder, objIDVec)
	types.QuorumProofAddBlsSignature(builder, sigVec)
	types.QuorumProofAddSignerBitmap(builder, bitmapVec)
	proofOffset := types.QuorumProofEnd(builder)

	types.AttestedTransactionStartProofsVector(builder, 1)
	builder.PrependUOffsetT(proofOffset)
	proofsVec := builder.EndVector(1)

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOffset)
	types.AttestedTransactionAddObjects(builder, objectsVec)
	types.AttestedTransactionAddProofs(builder, proofsVec)
	types.AttestedTransactionAddAttestationEpoch(builder, attestationEpoch)
	atxOffset := types.AttestedTransactionEnd(builder)

	builder.Finish(atxOffset)

	return builder.FinishedBytes()
}

// serializeMinimalTx builds a minimal Transaction table.
func serializeMinimalTx(builder *flatbuffers.Builder) flatbuffers.UOffsetT {
	funcOffset := builder.CreateString("test_func")
	types.TransactionStart(builder)
	types.TransactionAddFunctionName(builder, funcOffset)
	return types.TransactionEnd(builder)
}

// fixedResolver returns an EpochResolver pinned to currentEpoch, exposing the
// current set as currentEpoch's snapshot and prevEpochHolders as the previous.
func fixedResolver(current uint64, vs *validators.ValidatorSet, epochLength uint64) EpochResolver {
	return EpochResolver{
		HoldersForEpoch: func(epoch uint64) (*validators.ValidatorSet, bool) {
			if epoch == current || (current > 0 && epoch == current-1) {
				return vs, true
			}
			return nil, false
		},
		CommitEpochForRound: func(round uint64) uint64 {
			e := round / epochLength
			if e > 0 && round%epochLength == 0 {
				return e - 1
			}
			return e
		},
		EpochLength: epochLength,
		GraceRounds: 50,
	}
}

// TestATXVerifierSameEpoch accepts an attestation from the commit epoch.
func TestATXVerifierSameEpoch(t *testing.T) {
	const epochLength = 100
	vs, atxBytes := buildVerifierFixture(t, 3)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	// Commit round 350 → commit epoch 3 (350/100). Attestation epoch 3 matches.
	v := NewATXVerifier(fixedResolver(3, vs, epochLength))
	if err := v.Verify(atx, 350); err != nil {
		t.Fatalf("expected accept for same epoch, got %v", err)
	}
}

// TestATXVerifierPrevWithinGrace accepts a previous-epoch attestation just past
// the boundary.
func TestATXVerifierPrevWithinGrace(t *testing.T) {
	const epochLength = 100
	vs, atxBytes := buildVerifierFixture(t, 2)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	// Commit round 320 → commit epoch 3. Attestation epoch 2 (== commitEpoch-1).
	// Boundary is 300; 320-300=20 < 50 grace.
	v := NewATXVerifier(fixedResolver(3, vs, epochLength))
	if err := v.Verify(atx, 320); err != nil {
		t.Fatalf("expected accept within grace, got %v", err)
	}
}

// TestATXVerifierPrevPastGrace rejects a previous-epoch attestation past grace.
func TestATXVerifierPrevPastGrace(t *testing.T) {
	const epochLength = 100
	vs, atxBytes := buildVerifierFixture(t, 2)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	// Commit round 360 → commit epoch 3. Boundary 300; 360-300=60 >= 50 grace.
	v := NewATXVerifier(fixedResolver(3, vs, epochLength))
	if err := v.Verify(atx, 360); err == nil {
		t.Fatal("expected reject past grace, got accept")
	}
}

// TestATXVerifierFutureEpoch rejects an attestation from a future epoch.
func TestATXVerifierFutureEpoch(t *testing.T) {
	const epochLength = 100
	vs, atxBytes := buildVerifierFixture(t, 5)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	// Commit round 350 → commit epoch 3. Attestation epoch 5 is in the future.
	v := NewATXVerifier(fixedResolver(3, vs, epochLength))
	if err := v.Verify(atx, 350); err == nil {
		t.Fatal("expected reject for future epoch, got accept")
	}
}

// TestATXVerifierRejectsProofBuiltOverDifferentHolderSet is the second-layer
// attested-transfer wedge at the unit seam: a quorum proof assembled over one
// holder set (the daemon's live validator set) is rejected by a verifier that
// reconstructs the holders from a DIFFERENT set (the epoch-frozen snapshot the
// chain verifies against), because the signer bitmap then resolves to the wrong
// public keys and the aggregated signature no longer matches. Reassembling the
// proof over the same frozen snapshot the verifier uses makes it verify.
//
// This is why the daemon must compute holders and collect attestations over the
// epoch-frozen holder snapshot, not the live validator set: when the two differ
// (validator churn, an epoch boundary frozen mid-bootstrap), a live-set proof is
// rejected at commit and the transfer wedges.
func TestATXVerifierRejectsProofBuiltOverDifferentHolderSet(t *testing.T) {
	const (
		epochLength      = 100
		replication      = 3
		commitRound      = 350
		attestationEpoch = 3
	)

	full, keys := buildBLSValidators(t, 5)

	objID := [32]byte{0x77}

	// The live top-3 for the object over the full set. The frozen snapshot drops
	// one of these holders, so its own top-3 differs, exactly as a node whose
	// epoch snapshot diverges from the daemon's live set would compute.
	liveHolders := attest.ComputeHolders(full, objID, replication)
	frozen := setExcluding(full, liveHolders[0])

	if sameHolders(attest.ComputeHolders(full, objID, replication), attest.ComputeHolders(frozen, objID, replication)) {
		t.Fatal("fixture invalid: frozen snapshot must yield a different holder set")
	}

	verifier := NewATXVerifier(fixedResolver(attestationEpoch, frozen, epochLength))

	// A proof assembled over the LIVE set (the pre-fix daemon) is rejected by the
	// frozen-snapshot verifier: the split that only lets a node whose snapshot
	// happens to match the live set apply the transfer.
	liveATX := signQuorumATX(t, full, keys, objID, replication, attestationEpoch)
	if err := verifier.Verify(types.GetRootAsAttestedTransaction(liveATX, 0), commitRound); err == nil {
		t.Fatal("expected reject: a proof built over the live set must fail against the frozen snapshot")
	}

	// The fix: assemble the proof over the SAME frozen snapshot the verifier uses.
	frozenATX := signQuorumATX(t, frozen, keys, objID, replication, attestationEpoch)
	if err := verifier.Verify(types.GetRootAsAttestedTransaction(frozenATX, 0), commitRound); err != nil {
		t.Fatalf("expected accept when the proof is built over the verifier's frozen snapshot: %v", err)
	}
}

// buildBLSValidators builds n validators with deterministic BLS keys, returning
// the set and a lookup from validator pubkey to its signing key.
func buildBLSValidators(t *testing.T, n int) (*validators.ValidatorSet, map[validators.Hash]*attest.BLSKeyPair) {
	t.Helper()

	vs := validators.NewValidatorSet(nil)
	keys := make(map[validators.Hash]*attest.BLSKeyPair, n)

	for i := 0; i < n; i++ {
		var pubkey validators.Hash
		pubkey[0] = byte(i + 1)

		seed := make([]byte, 32)
		seed[0] = byte(i + 1)
		key, err := attest.GenerateBLSKeyFromSeed(seed)
		if err != nil {
			t.Fatalf("derive bls key %d: %v", i, err)
		}

		var blsPub [48]byte
		copy(blsPub[:], key.PublicKeyBytes())
		vs.Add(pubkey, "addr", blsPub)
		keys[pubkey] = key
	}

	return vs, keys
}

// setExcluding returns a copy of vs without the given pubkey.
func setExcluding(vs *validators.ValidatorSet, drop validators.Hash) *validators.ValidatorSet {
	out := validators.NewValidatorSet(nil)

	for _, v := range vs.All() {
		if v.Pubkey == drop {
			continue
		}
		out.Add(v.Pubkey, v.QUICAddr, v.BLSPubkey)
	}

	return out
}

// signQuorumATX assembles a one-proof ATX for objID: it computes the holders
// over signVS, has each holder sign the object hash, aggregates the signatures,
// and builds the signer bitmap over those holders. This models the daemon
// assembling a quorum over whatever validator set it computed holders from.
func signQuorumATX(t *testing.T, signVS *validators.ValidatorSet, keys map[validators.Hash]*attest.BLSKeyPair, objID [32]byte, replication int, epoch uint64) []byte {
	t.Helper()

	objData := buildTestObject(objID, uint16(replication))
	obj := types.GetRootAsObject(objData, 0)
	hash := attest.ComputeObjectHash(obj.ContentBytes(), obj.Version())

	holders := attest.ComputeHolders(signVS, objID, replication)

	var sigs [][]byte
	var indices []int
	for i, h := range holders {
		sigs = append(sigs, keys[h].Sign(hash[:]))
		indices = append(indices, i)
	}

	aggSig, err := attest.AggregateSignatures(sigs)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	return buildATXWithProof(objID, objData, aggSig, attest.BuildSignerBitmap(indices, len(holders)), epoch)
}

// sameHolders reports whether two holder slices are identical in order.
func sameHolders(a, b []validators.Hash) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
