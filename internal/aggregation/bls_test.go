package aggregation

import (
	"bytes"
	"testing"
)

// TestBLSSignVerify tests basic sign and verify.
func TestBLSSignVerify(t *testing.T) {
	key, err := GenerateBLSKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	message := []byte("hello, bls!")
	signature := key.Sign(message)

	if len(signature) != BLSSignatureSize {
		t.Errorf("signature size: got %d, want %d", len(signature), BLSSignatureSize)
	}

	if !Verify(signature, message, key.PublicKeyBytes()) {
		t.Error("valid signature should verify")
	}
}

// TestBLSSignVerifyWrongMessage tests verification with wrong message.
func TestBLSSignVerifyWrongMessage(t *testing.T) {
	key, err := GenerateBLSKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	message := []byte("hello, bls!")
	signature := key.Sign(message)

	wrongMessage := []byte("wrong message")
	if Verify(signature, wrongMessage, key.PublicKeyBytes()) {
		t.Error("signature should not verify with wrong message")
	}
}

// TestBLSSignVerifyWrongKey tests verification with wrong key.
func TestBLSSignVerifyWrongKey(t *testing.T) {
	key1, _ := GenerateBLSKey()
	key2, _ := GenerateBLSKey()

	message := []byte("hello, bls!")
	signature := key1.Sign(message)

	if Verify(signature, message, key2.PublicKeyBytes()) {
		t.Error("signature should not verify with wrong key")
	}
}

// TestBLSDeterministicKey tests that seed produces deterministic keys.
func TestBLSDeterministicKey(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}

	key1, _ := GenerateBLSKeyFromSeed(seed)
	key2, _ := GenerateBLSKeyFromSeed(seed)

	if !bytes.Equal(key1.PublicKeyBytes(), key2.PublicKeyBytes()) {
		t.Error("same seed should produce same key")
	}
}

// TestBLSAggregation tests signature aggregation and verification.
func TestBLSAggregation(t *testing.T) {
	const numSigners = 5
	keys := make([]*BLSKeyPair, numSigners)
	sigs := make([][]byte, numSigners)
	pubkeys := make([][]byte, numSigners)

	message := []byte("aggregate me!")

	for i := 0; i < numSigners; i++ {
		key, err := GenerateBLSKey()
		if err != nil {
			t.Fatalf("generate key %d: %v", i, err)
		}

		keys[i] = key
		sigs[i] = key.Sign(message)
		pubkeys[i] = key.PublicKeyBytes()
	}

	// Aggregate signatures
	aggSig, err := AggregateSignatures(sigs)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	if len(aggSig) != BLSSignatureSize {
		t.Errorf("aggregated signature size: got %d, want %d", len(aggSig), BLSSignatureSize)
	}

	// Verify aggregated signature
	if !VerifyAggregated(aggSig, message, pubkeys) {
		t.Error("aggregated signature should verify")
	}
}

// TestBLSAggregationSubset tests verification with subset of signers.
func TestBLSAggregationSubset(t *testing.T) {
	const numSigners = 5
	keys := make([]*BLSKeyPair, numSigners)
	message := []byte("partial aggregate")

	for i := 0; i < numSigners; i++ {
		keys[i], _ = GenerateBLSKey()
	}

	// Only 3 of 5 sign
	signerIndices := []int{0, 2, 4}
	sigs := make([][]byte, len(signerIndices))
	pubkeys := make([][]byte, len(signerIndices))

	for i, idx := range signerIndices {
		sigs[i] = keys[idx].Sign(message)
		pubkeys[i] = keys[idx].PublicKeyBytes()
	}

	aggSig, err := AggregateSignatures(sigs)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	// Should verify with correct subset of pubkeys
	if !VerifyAggregated(aggSig, message, pubkeys) {
		t.Error("aggregated signature should verify with correct pubkeys")
	}

	// Should NOT verify with all pubkeys (includes non-signers)
	allPubkeys := make([][]byte, numSigners)
	for i := 0; i < numSigners; i++ {
		allPubkeys[i] = keys[i].PublicKeyBytes()
	}

	if VerifyAggregated(aggSig, message, allPubkeys) {
		t.Error("aggregated signature should not verify with non-signers included")
	}
}

// TestBLSAggregationEmpty tests aggregation with no signatures.
func TestBLSAggregationEmpty(t *testing.T) {
	_, err := AggregateSignatures(nil)
	if err == nil {
		t.Error("aggregating empty slice should error")
	}

	_, err = AggregateSignatures([][]byte{})
	if err == nil {
		t.Error("aggregating empty slice should error")
	}
}

// TestBLSInvalidInputs tests verification with invalid inputs.
func TestBLSInvalidInputs(t *testing.T) {
	key, _ := GenerateBLSKey()
	message := []byte("test")
	signature := key.Sign(message)
	pubkey := key.PublicKeyBytes()

	// Invalid signature size
	if Verify([]byte("short"), message, pubkey) {
		t.Error("short signature should not verify")
	}

	// Invalid pubkey size
	if Verify(signature, message, []byte("short")) {
		t.Error("short pubkey should not verify")
	}

	// Corrupt signature
	corruptSig := make([]byte, len(signature))
	copy(corruptSig, signature)
	corruptSig[0] ^= 0xFF

	if Verify(corruptSig, message, pubkey) {
		t.Error("corrupt signature should not verify")
	}
}

// TestSignerBitmap tests bitmap building and parsing.
func TestSignerBitmap(t *testing.T) {
	tests := []struct {
		indices []int
		total   int
	}{
		{[]int{0}, 8},
		{[]int{7}, 8},
		{[]int{0, 1, 2}, 8},
		{[]int{0, 7}, 8},
		{[]int{0, 8, 15}, 16},
		{[]int{0, 2, 4, 6, 8, 10}, 12},
		{[]int{}, 8},
	}

	for _, tc := range tests {
		bitmap := BuildSignerBitmap(tc.indices, tc.total)
		parsed := ParseSignerBitmap(bitmap)

		// Check length
		expectedBytes := (tc.total + 7) / 8
		if len(bitmap) != expectedBytes {
			t.Errorf("bitmap size for total=%d: got %d, want %d", tc.total, len(bitmap), expectedBytes)
		}

		// Check round-trip
		if len(tc.indices) != len(parsed) {
			t.Errorf("parsed length mismatch: got %d, want %d", len(parsed), len(tc.indices))
			continue
		}

		for i, idx := range tc.indices {
			if parsed[i] != idx {
				t.Errorf("parsed[%d] = %d, want %d", i, parsed[i], idx)
			}
		}
	}
}

// TestSignerBitmapInvalidIndex tests bitmap with out-of-range indices.
func TestSignerBitmapInvalidIndex(t *testing.T) {
	// Negative index should be ignored
	bitmap := BuildSignerBitmap([]int{-1, 0, 1}, 8)
	parsed := ParseSignerBitmap(bitmap)

	if len(parsed) != 2 || parsed[0] != 0 || parsed[1] != 1 {
		t.Errorf("invalid index should be ignored: got %v", parsed)
	}

	// Index >= total should be ignored
	bitmap = BuildSignerBitmap([]int{0, 8}, 8)
	parsed = ParseSignerBitmap(bitmap)

	if len(parsed) != 1 || parsed[0] != 0 {
		t.Errorf("out-of-range index should be ignored: got %v", parsed)
	}
}

// BenchmarkBLSSign benchmarks BLS signing.
func BenchmarkBLSSign(b *testing.B) {
	key, _ := GenerateBLSKey()
	message := []byte("benchmark message")

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		key.Sign(message)
	}
}

// BenchmarkBLSVerify benchmarks BLS verification.
func BenchmarkBLSVerify(b *testing.B) {
	key, _ := GenerateBLSKey()
	message := []byte("benchmark message")
	signature := key.Sign(message)
	pubkey := key.PublicKeyBytes()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		Verify(signature, message, pubkey)
	}
}

// BenchmarkBLSAggregate benchmarks signature aggregation.
func BenchmarkBLSAggregate10(b *testing.B) {
	benchmarkBLSAggregate(b, 10)
}

// BenchmarkBLSAggregate100 benchmarks with 100 signatures.
func BenchmarkBLSAggregate100(b *testing.B) {
	benchmarkBLSAggregate(b, 100)
}

func benchmarkBLSAggregate(b *testing.B, numSigs int) {
	message := []byte("benchmark")
	sigs := make([][]byte, numSigs)

	for i := 0; i < numSigs; i++ {
		key, _ := GenerateBLSKey()
		sigs[i] = key.Sign(message)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		AggregateSignatures(sigs)
	}
}

// BenchmarkBLSVerifyAggregated benchmarks aggregated verification.
func BenchmarkBLSVerifyAggregated10(b *testing.B) {
	benchmarkBLSVerifyAggregated(b, 10)
}

// BenchmarkBLSVerifyAggregated100 benchmarks with 100 signers.
func BenchmarkBLSVerifyAggregated100(b *testing.B) {
	benchmarkBLSVerifyAggregated(b, 100)
}

func benchmarkBLSVerifyAggregated(b *testing.B, numSigners int) {
	message := []byte("benchmark")
	sigs := make([][]byte, numSigners)
	pubkeys := make([][]byte, numSigners)

	for i := 0; i < numSigners; i++ {
		key, _ := GenerateBLSKey()
		sigs[i] = key.Sign(message)
		pubkeys[i] = key.PublicKeyBytes()
	}

	aggSig, _ := AggregateSignatures(sigs)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		VerifyAggregated(aggSig, message, pubkeys)
	}
}
