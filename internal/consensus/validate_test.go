package consensus

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// =============================================================================
// Vertex Validation Tests
// =============================================================================

// TestValidateSignature_InvalidSig verifies corrupt signature is rejected.
func TestValidateSignature_InvalidSig(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	// Build a valid vertex, then corrupt the signature
	data := buildTestVertex(t, validators[1], 0, nil, 1)
	vertex := types.GetRootAsVertex(data, 0)

	// Corrupt one byte of the signature in the buffer
	sigBytes := vertex.SignatureBytes()
	if len(sigBytes) > 0 {
		sigBytes[0] ^= 0xFF
	}

	err := dag.validateSignature(vertex)
	if err == nil {
		t.Fatal("expected error for invalid signature")
	}

	if !strings.Contains(err.Error(), "invalid signature") {
		t.Errorf("expected 'invalid signature', got: %v", err)
	}
}

// TestValidateEpoch_Mismatch verifies an epoch the vertex's own round cannot have
// reached is rejected.
func TestValidateEpoch_Mismatch(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil, WithEpochLength(10))
	defer dag.Close()

	// Round 0 belongs to epoch 0: no producer can claim epoch 2 there.
	data := buildTestVertex(t, validators[1], 0, nil, 2)
	vertex := types.GetRootAsVertex(data, 0)

	err := dag.validateEpoch(vertex)
	if err == nil {
		t.Fatal("expected error for epoch mismatch")
	}

	if !strings.Contains(err.Error(), "epoch mismatch") {
		t.Errorf("expected 'epoch mismatch', got: %v", err)
	}
}

// TestValidateEpoch_Window pins the two-sided, receiver-independent window a
// header's epoch must fall in: the epoch the vertex's OWN round commits in, or
// that value plus or minus one. The window tolerates the two honest skews (a
// producer that has not transitioned yet, and one filling a round below the
// boundary it just crossed) and nothing else, so the field a light client reads
// to pick a validator tree can neither run ahead nor be dragged back to a stale
// validator set.
//
// The receiver's own currentEpoch is left at 0 throughout: every case is decided
// from the vertex's round alone.
func TestValidateEpoch_Window(t *testing.T) {
	tests := []struct {
		name        string // name describes the skew being pinned
		epochLength uint64 // epochLength is the receiver's configured epoch length
		round       uint64 // round is the round the vertex claims
		epoch       uint64 // epoch is the epoch the vertex's header claims
		wantReject  bool   // wantReject is true when the claim must be rejected
	}{
		{
			// Production resumes at lastProducedRound+1, which can sit BELOW the
			// commit cursor after a stall: the node commits round 1000, moves to
			// epoch 1, and stamps epoch 1 on the round-998 vertex it produces next.
			name:        "post-stall production one epoch above its round",
			epochLength: 1000,
			round:       998,
			epoch:       1,
		},
		{
			name:        "the round's own epoch",
			epochLength: 1000,
			round:       998,
			epoch:       0,
		},
		{
			name:        "two epochs above the round is forged",
			epochLength: 1000,
			round:       998,
			epoch:       2,
			wantReject:  true,
		},
		{
			name:        "one epoch below the round (producer has not transitioned)",
			epochLength: 1000,
			round:       5500,
			epoch:       4,
		},
		{
			name:        "two epochs below the round is a stale validator set",
			epochLength: 1000,
			round:       5500,
			epoch:       3,
			wantReject:  true,
		},
		{
			name:        "one epoch above the round",
			epochLength: 1000,
			round:       5500,
			epoch:       6,
		},
		{
			name:        "epochs disabled: epoch 0 is the only claim",
			epochLength: 0,
			round:       5500,
			epoch:       0,
		},
		{
			name:        "epochs disabled: a nonzero epoch is rejected",
			epochLength: 0,
			round:       5500,
			epoch:       1,
			wantReject:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newTestStorage(t)
			validators, vs := newTestValidatorSet(4)

			dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil, WithEpochLength(tt.epochLength))
			defer dag.Close()

			data := buildTestVertex(t, validators[1], tt.round, nil, tt.epoch)
			err := dag.validateEpoch(types.GetRootAsVertex(data, 0))

			if tt.wantReject && !errors.Is(err, errWrongEpoch) {
				t.Fatalf("epoch %d at round %d must be rejected, got: %v", tt.epoch, tt.round, err)
			}

			if !tt.wantReject && err != nil {
				t.Fatalf("epoch %d at round %d must validate: %v", tt.epoch, tt.round, err)
			}
		})
	}
}

// TestValidateParents_Missing verifies parent not in store is rejected.
func TestValidateParents_Missing(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	// Build vertex referencing a parent hash that doesn't exist in the store
	fakeParent := Hash{0xDE, 0xAD}
	data := buildTestVertexWithParentLinks(t, validators[1], 1, 1,
		[]parentLink{{hash: fakeParent, producer: validators[0].pubKey}},
	)
	vertex := types.GetRootAsVertex(data, 0)

	err := dag.validateParents(vertex)
	if err == nil {
		t.Fatal("expected error for missing parent")
	}

	if !strings.Contains(err.Error(), "parent not found") {
		t.Errorf("expected 'parent not found', got: %v", err)
	}
}

// TestValidateParents_WrongRound verifies parent from wrong round is rejected.
func TestValidateParents_WrongRound(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	// Store a round-0 vertex
	r0data := buildTestVertex(t, validators[0], 0, nil, 1)
	r0vertex := types.GetRootAsVertex(r0data, 0)
	var r0hash Hash
	copy(r0hash[:], r0vertex.HashBytes())
	dag.store.add(r0data, r0hash, 0, validators[0].pubKey)

	// Build a round-2 vertex referencing the round-0 parent
	// validateParents expects parents from round N-1 = 1, but our parent is round 0
	data := buildTestVertexWithParentLinks(t, validators[1], 2, 1,
		[]parentLink{{hash: r0hash, producer: validators[0].pubKey}},
	)
	vertex := types.GetRootAsVertex(data, 0)

	err := dag.validateParents(vertex)
	if err == nil {
		t.Fatal("expected error for parent round mismatch")
	}

	if !strings.Contains(err.Error(), "parent round mismatch") {
		t.Errorf("expected 'parent round mismatch', got: %v", err)
	}
}

// TestValidateParentsQuorum_MinimumKnownParent verifies that at least 1 known parent
// producer is required. BFT quorum is only enforced during local production.
func TestValidateParentsQuorum_MinimumKnownParent(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	// Store a round-0 vertex from validator[0]
	r0data := buildTestVertex(t, validators[0], 0, nil, 1)
	r0vertex := types.GetRootAsVertex(r0data, 0)
	var r0hash Hash
	copy(r0hash[:], r0vertex.HashBytes())
	dag.store.add(r0data, r0hash, 0, validators[0].pubKey)

	// Build round-1 vertex with 1 known parent — should pass
	data := buildTestVertexWithParentLinks(t, validators[1], 1, 1,
		[]parentLink{{hash: r0hash, producer: validators[0].pubKey}},
	)
	vertex := types.GetRootAsVertex(data, 0)

	err := dag.validateParentsQuorum(vertex)
	if err != nil {
		t.Fatalf("expected quorum to pass with 1 known parent, got: %v", err)
	}
}

// =============================================================================
// Fee Summary Validation Tests
// =============================================================================

// TestValidateFeeSummary_Correct verifies correct fee summary passes.
func TestValidateFeeSummary_Correct(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	params := DefaultFeeParams()
	dag.SetFeeSystem(newMockCoinStore(), &params, nil)

	// Build a fee-test ATX with gas_coin and max_gas
	sender := Hash{0x01}
	gasCoin := Hash{0xCC}
	maxGas := uint64(500)
	atxBytes := buildFeeTestATX(t, sender, gasCoin, maxGas, []uint16{0})

	// Calculate the expected fee/split
	consumed, _ := dag.calculateTxFeeSplit(
		types.GetRootAsAttestedTransaction(atxBytes, 0).Transaction(nil),
		types.GetRootAsAttestedTransaction(atxBytes, 0),
	)
	split := SplitFee(consumed, params)

	// Build vertex with correct fee summary
	data := buildVertexWithFeeSummary(t, validators[0], 0, 1,
		&feeSummaryValues{split.Total, split.Burned, split.Epoch},
		[][]byte{atxBytes},
	)
	vertex := types.GetRootAsVertex(data, 0)

	err := dag.validateFeeSummary(vertex)
	if err != nil {
		t.Fatalf("expected no error for correct summary, got: %v", err)
	}
}

// TestValidateFeeSummary_WrongTotalFees verifies wrong total_fees is rejected.
func TestValidateFeeSummary_WrongTotalFees(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	params := DefaultFeeParams()
	dag.SetFeeSystem(newMockCoinStore(), &params, nil)

	sender := Hash{0x01}
	gasCoin := Hash{0xCC}
	atxBytes := buildFeeTestATX(t, sender, gasCoin, 500, []uint16{0})

	consumed, _ := dag.calculateTxFeeSplit(
		types.GetRootAsAttestedTransaction(atxBytes, 0).Transaction(nil),
		types.GetRootAsAttestedTransaction(atxBytes, 0),
	)
	split := SplitFee(consumed, params)

	// total_fees off by 1
	data := buildVertexWithFeeSummary(t, validators[0], 0, 1,
		&feeSummaryValues{split.Total + 1, split.Burned, split.Epoch},
		[][]byte{atxBytes},
	)
	vertex := types.GetRootAsVertex(data, 0)

	err := dag.validateFeeSummary(vertex)
	if err == nil {
		t.Fatal("expected error for wrong total_fees")
	}

	if !strings.Contains(err.Error(), "total_fees mismatch") {
		t.Errorf("expected 'total_fees mismatch', got: %v", err)
	}
}

// TestValidateFeeSummary_WrongBurned verifies wrong burned is rejected.
func TestValidateFeeSummary_WrongBurned(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	params := DefaultFeeParams()
	dag.SetFeeSystem(newMockCoinStore(), &params, nil)

	sender := Hash{0x01}
	gasCoin := Hash{0xCC}
	atxBytes := buildFeeTestATX(t, sender, gasCoin, 500, []uint16{0})

	consumed, _ := dag.calculateTxFeeSplit(
		types.GetRootAsAttestedTransaction(atxBytes, 0).Transaction(nil),
		types.GetRootAsAttestedTransaction(atxBytes, 0),
	)
	split := SplitFee(consumed, params)

	data := buildVertexWithFeeSummary(t, validators[0], 0, 1,
		&feeSummaryValues{split.Total, split.Burned + 1, split.Epoch},
		[][]byte{atxBytes},
	)
	vertex := types.GetRootAsVertex(data, 0)

	err := dag.validateFeeSummary(vertex)
	if err == nil {
		t.Fatal("expected error for wrong burned")
	}

	if !strings.Contains(err.Error(), "total_burned mismatch") {
		t.Errorf("expected 'total_burned mismatch', got: %v", err)
	}
}

// TestValidateFeeSummary_WrongEpoch verifies wrong epoch is rejected.
func TestValidateFeeSummary_WrongEpoch(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	params := DefaultFeeParams()
	dag.SetFeeSystem(newMockCoinStore(), &params, nil)

	sender := Hash{0x01}
	gasCoin := Hash{0xCC}
	atxBytes := buildFeeTestATX(t, sender, gasCoin, 500, []uint16{0})

	consumed, _ := dag.calculateTxFeeSplit(
		types.GetRootAsAttestedTransaction(atxBytes, 0).Transaction(nil),
		types.GetRootAsAttestedTransaction(atxBytes, 0),
	)
	split := SplitFee(consumed, params)

	data := buildVertexWithFeeSummary(t, validators[0], 0, 1,
		&feeSummaryValues{split.Total, split.Burned, split.Epoch + 1},
		[][]byte{atxBytes},
	)
	vertex := types.GetRootAsVertex(data, 0)

	err := dag.validateFeeSummary(vertex)
	if err == nil {
		t.Fatal("expected error for wrong epoch")
	}

	if !strings.Contains(err.Error(), "total_epoch mismatch") {
		t.Errorf("expected 'total_epoch mismatch', got: %v", err)
	}
}

// TestValidateFeeSummary_NoSummaryNoTxs verifies no summary with 0 txs passes.
func TestValidateFeeSummary_NoSummaryNoTxs(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	params := DefaultFeeParams()
	dag.SetFeeSystem(newMockCoinStore(), &params, nil)

	// Vertex with no transactions and no fee summary
	data := buildTestVertex(t, validators[0], 0, nil, 1)
	vertex := types.GetRootAsVertex(data, 0)

	err := dag.validateFeeSummary(vertex)
	if err != nil {
		t.Fatalf("expected nil error for no summary + no txs, got: %v", err)
	}
}

// TestValidateFeeSummary_NoSummaryWithTxs verifies no summary with >0 txs fails.
func TestValidateFeeSummary_NoSummaryWithTxs(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	params := DefaultFeeParams()
	dag.SetFeeSystem(newMockCoinStore(), &params, nil)

	// Vertex with transactions but no fee summary
	sender := Hash{0x01}
	gasCoin := Hash{0xCC}
	atxBytes := buildFeeTestATX(t, sender, gasCoin, 500, []uint16{0})

	data := buildVertexWithFeeSummary(t, validators[0], 0, 1, nil, [][]byte{atxBytes})
	vertex := types.GetRootAsVertex(data, 0)

	err := dag.validateFeeSummary(vertex)
	if err == nil {
		t.Fatal("expected error for missing fee_summary with transactions")
	}

	if !strings.Contains(err.Error(), "missing fee_summary") {
		t.Errorf("expected 'missing fee_summary', got: %v", err)
	}
}

// TestValidateFeeSummary_FeesDisabled verifies fees disabled skips validation.
func TestValidateFeeSummary_FeesDisabled(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	// feeParams=nil → fees disabled
	data := buildTestVertex(t, validators[0], 0, nil, 1)
	vertex := types.GetRootAsVertex(data, 0)

	err := dag.validateFeeSummary(vertex)
	if err != nil {
		t.Fatalf("expected nil error when fees disabled, got: %v", err)
	}
}

// TestValidateFeeSummary_TxWithoutGasCoinSkipped verifies txs without gas_coin are excluded from fee recalc.
func TestValidateFeeSummary_TxWithoutGasCoinSkipped(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	params := DefaultFeeParams()
	dag.SetFeeSystem(newMockCoinStore(), &params, nil)

	// One ATX with gas_coin, one without
	sender := Hash{0x01}
	gasCoin := Hash{0xCC}
	atxWithGas := buildFeeTestATX(t, sender, gasCoin, 500, []uint16{0})
	atxWithoutGas := buildTestATX(t, "no_gas_func", nil, nil, 0)

	// Calculate the consumed-only fee for the ATX with gas_coin
	consumed, _ := dag.calculateTxFeeSplit(
		types.GetRootAsAttestedTransaction(atxWithGas, 0).Transaction(nil),
		types.GetRootAsAttestedTransaction(atxWithGas, 0),
	)
	split := SplitFee(consumed, params)

	// Build vertex with correct summary (only counting ATX with gas_coin)
	data := buildVertexWithFeeSummary(t, validators[0], 0, 1,
		&feeSummaryValues{split.Total, split.Burned, split.Epoch},
		[][]byte{atxWithGas, atxWithoutGas},
	)
	vertex := types.GetRootAsVertex(data, 0)

	err := dag.validateFeeSummary(vertex)
	if err != nil {
		t.Fatalf("expected no error when tx without gas_coin is skipped, got: %v", err)
	}
}

// =============================================================================
// Test Helpers
// =============================================================================

// parentLink represents a parent reference with a hash and producer.
type parentLink struct {
	hash     Hash // hash is the parent vertex hash
	producer Hash // producer is the parent vertex producer
}

// feeSummaryValues holds the 3 fee summary fields.
type feeSummaryValues struct {
	totalFees   uint64
	totalBurned uint64
	totalEpoch  uint64
}

// buildTestVertexWithParentLinks creates a signed vertex with specific parent links.
func buildTestVertexWithParentLinks(t *testing.T, v testValidator, round uint64, epoch uint64, parents []parentLink) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(2048)

	// Build parent links
	parentOffsets := make([]flatbuffers.UOffsetT, len(parents))
	for i, p := range parents {
		hVec := builder.CreateByteVector(p.hash[:])
		pVec := builder.CreateByteVector(p.producer[:])

		types.VertexLinkStart(builder)
		types.VertexLinkAddHash(builder, hVec)
		types.VertexLinkAddProducer(builder, pVec)
		parentOffsets[i] = types.VertexLinkEnd(builder)
	}

	types.VertexStartParentsVector(builder, len(parentOffsets))
	for i := len(parentOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(parentOffsets[i])
	}
	parentsVec := builder.EndVector(len(parentOffsets))

	types.VertexStartTransactionsVector(builder, 0)
	txsVec := builder.EndVector(0)

	producerVec := builder.CreateByteVector(v.pubKey[:])

	// Build unsigned vertex first
	types.VertexStart(builder)
	types.VertexAddRound(builder, round)
	types.VertexAddProducer(builder, producerVec)
	types.VertexAddParents(builder, parentsVec)
	types.VertexAddTransactions(builder, txsVec)
	types.VertexAddEpoch(builder, epoch)
	vertexOff := types.VertexEnd(builder)
	builder.Finish(vertexOff)

	unsigned := builder.FinishedBytes()
	hash, bodyHash := vertexIdentity(types.GetRootAsVertex(unsigned, 0))
	sig := ed25519.Sign(v.privKey, hash[:])

	// Rebuild with hash, body hash and signature
	builder.Reset()

	parentOffsets = make([]flatbuffers.UOffsetT, len(parents))
	for i, p := range parents {
		hVec := builder.CreateByteVector(p.hash[:])
		pVec := builder.CreateByteVector(p.producer[:])

		types.VertexLinkStart(builder)
		types.VertexLinkAddHash(builder, hVec)
		types.VertexLinkAddProducer(builder, pVec)
		parentOffsets[i] = types.VertexLinkEnd(builder)
	}

	types.VertexStartParentsVector(builder, len(parentOffsets))
	for i := len(parentOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(parentOffsets[i])
	}
	parentsVec = builder.EndVector(len(parentOffsets))

	types.VertexStartTransactionsVector(builder, 0)
	txsVec = builder.EndVector(0)

	hashVec := builder.CreateByteVector(hash[:])
	bodyHashVec := builder.CreateByteVector(bodyHash[:])
	sigVec := builder.CreateByteVector(sig)
	producerVec = builder.CreateByteVector(v.pubKey[:])

	types.VertexStart(builder)
	types.VertexAddHash(builder, hashVec)
	types.VertexAddRound(builder, round)
	types.VertexAddProducer(builder, producerVec)
	types.VertexAddSignature(builder, sigVec)
	types.VertexAddParents(builder, parentsVec)
	types.VertexAddTransactions(builder, txsVec)
	types.VertexAddEpoch(builder, epoch)
	types.VertexAddBodyHash(builder, bodyHashVec)
	vertexOff = types.VertexEnd(builder)

	builder.Finish(vertexOff)

	return builder.FinishedBytes()
}

// buildVertexWithFeeSummary creates a signed vertex with a FeeSummary and ATX transactions.
// If summary is nil, no FeeSummary is added (used to test missing summary).
func buildVertexWithFeeSummary(t *testing.T, v testValidator, round uint64, epoch uint64, summary *feeSummaryValues, atxBytesList [][]byte) []byte {
	t.Helper()

	// Two-pass build: first unsigned for the header hash, then signed.
	data := buildVertexWithFeeSummaryInner(v, round, epoch, summary, atxBytesList, nil, nil, nil)
	hash, bodyHash := vertexIdentity(types.GetRootAsVertex(data, 0))
	sig := ed25519.Sign(v.privKey, hash[:])

	return buildVertexWithFeeSummaryInner(v, round, epoch, summary, atxBytesList, hash[:], bodyHash[:], sig)
}

// buildVertexWithFeeSummaryInner builds a vertex with optional hash/body hash/sig.
func buildVertexWithFeeSummaryInner(v testValidator, round uint64, epoch uint64, summary *feeSummaryValues, atxBytesList [][]byte, hash, bodyHash, sig []byte) []byte {
	builder := flatbuffers.NewBuilder(8192)

	// Rebuild all ATXs inside the builder
	atxOffsets := make([]flatbuffers.UOffsetT, len(atxBytesList))
	for i, atxBytes := range atxBytesList {
		atxOffsets[i] = rebuildATXInBuilder(builder, atxBytes)
	}

	// Build transactions vector
	types.VertexStartTransactionsVector(builder, len(atxOffsets))
	for i := len(atxOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(atxOffsets[i])
	}
	txsVec := builder.EndVector(len(atxOffsets))

	// Empty parents
	types.VertexStartParentsVector(builder, 0)
	parentsVec := builder.EndVector(0)

	// Build FeeSummary if provided
	var feeSummaryOff flatbuffers.UOffsetT
	if summary != nil {
		types.FeeSummaryStart(builder)
		types.FeeSummaryAddTotalFees(builder, summary.totalFees)
		types.FeeSummaryAddTotalBurned(builder, summary.totalBurned)
		types.FeeSummaryAddTotalEpoch(builder, summary.totalEpoch)
		feeSummaryOff = types.FeeSummaryEnd(builder)
	}

	producerVec := builder.CreateByteVector(v.pubKey[:])

	var hashVec, bodyHashVec, sigVec flatbuffers.UOffsetT
	if hash != nil {
		hashVec = builder.CreateByteVector(hash)
	}
	if bodyHash != nil {
		bodyHashVec = builder.CreateByteVector(bodyHash)
	}
	if sig != nil {
		sigVec = builder.CreateByteVector(sig)
	}

	types.VertexStart(builder)

	if hashVec != 0 {
		types.VertexAddHash(builder, hashVec)
	}

	types.VertexAddRound(builder, round)
	types.VertexAddProducer(builder, producerVec)

	if sigVec != 0 {
		types.VertexAddSignature(builder, sigVec)
	}

	types.VertexAddParents(builder, parentsVec)
	types.VertexAddTransactions(builder, txsVec)
	types.VertexAddEpoch(builder, epoch)

	if feeSummaryOff != 0 {
		types.VertexAddFeeSummary(builder, feeSummaryOff)
	}

	if bodyHashVec != 0 {
		types.VertexAddBodyHash(builder, bodyHashVec)
	}

	vertexOff := types.VertexEnd(builder)
	builder.Finish(vertexOff)

	return builder.FinishedBytes()
}
