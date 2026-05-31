package attest

import (
	"testing"
)

// makeAggCheck builds one valid AggCheck over the given message with numSigners
// signers, returning the check. When corrupt is true the signature's first byte
// is flipped so the aggregated verification must fail.
func makeAggCheck(t testing.TB, message []byte, numSigners int, corrupt bool) AggCheck {
	t.Helper()

	sigs := make([][]byte, numSigners)
	pubkeys := make([][]byte, numSigners)

	for i := 0; i < numSigners; i++ {
		key, err := GenerateBLSKey()
		if err != nil {
			t.Fatalf("generate key %d: %v", i, err)
		}

		sigs[i] = key.Sign(message)
		pubkeys[i] = key.PublicKeyBytes()
	}

	aggSig, err := AggregateSignatures(sigs)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	if corrupt {
		aggSig[0] ^= 0xFF
	}

	return AggCheck{Signature: aggSig, Message: message, PublicKeys: pubkeys}
}

// TestVerifyAggregatedBatchMatchesSequential checks that a batch of valid proofs
// plus one invalid proof returns verdicts that match element-wise sequential
// VerifyAggregated, with input order preserved.
func TestVerifyAggregatedBatchMatchesSequential(t *testing.T) {
	const validBefore = 8
	const numSigners = 10

	var items []AggCheck

	for i := 0; i < validBefore; i++ {
		items = append(items, makeAggCheck(t, []byte("batch-valid"), numSigners, false))
	}

	invalidIndex := len(items)
	items = append(items, makeAggCheck(t, []byte("batch-invalid"), numSigners, true))

	// A couple more valid proofs after the invalid one to prove order is kept.
	items = append(items, makeAggCheck(t, []byte("batch-valid"), numSigners, false))
	items = append(items, makeAggCheck(t, []byte("batch-valid"), numSigners, false))

	batch := VerifyAggregatedBatch(items)

	if len(batch) != len(items) {
		t.Fatalf("batch length: got %d, want %d", len(batch), len(items))
	}

	for i, item := range items {
		want := VerifyAggregated(item.Signature, item.Message, item.PublicKeys)
		if batch[i] != want {
			t.Errorf("item %d: batch=%v sequential=%v", i, batch[i], want)
		}
	}

	if batch[invalidIndex] {
		t.Errorf("invalid item at index %d should be false", invalidIndex)
	}

	for i := range batch {
		if i == invalidIndex {
			continue
		}

		if !batch[i] {
			t.Errorf("valid item at index %d should be true", i)
		}
	}
}

// TestVerifyAggregatedBatchEmpty checks that an empty batch returns an empty,
// non-nil result.
func TestVerifyAggregatedBatchEmpty(t *testing.T) {
	got := VerifyAggregatedBatch(nil)
	if got == nil || len(got) != 0 {
		t.Errorf("empty batch: got %v, want empty slice", got)
	}
}

// TestVerifyAggregatedBatchDeterministic checks that repeated runs over the same
// items produce identical verdicts, so goroutine scheduling never leaks into the
// result.
func TestVerifyAggregatedBatchDeterministic(t *testing.T) {
	var items []AggCheck

	for i := 0; i < 16; i++ {
		corrupt := i%3 == 0
		items = append(items, makeAggCheck(t, []byte("determinism"), 7, corrupt))
	}

	first := VerifyAggregatedBatch(items)

	for run := 0; run < 5; run++ {
		again := VerifyAggregatedBatch(items)

		for i := range first {
			if again[i] != first[i] {
				t.Fatalf("run %d item %d differs: %v vs %v", run, i, again[i], first[i])
			}
		}
	}
}

// buildBenchAggChecks builds n valid AggCheck items over a shared message, each
// with the given number of signers.
func buildBenchAggChecks(b *testing.B, n, numSigners int) []AggCheck {
	b.Helper()

	message := []byte("benchmark-batch")
	items := make([]AggCheck, n)

	for i := 0; i < n; i++ {
		items[i] = makeAggCheck(b, message, numSigners, false)
	}

	return items
}

// BenchmarkVerifyAggregatedSequentialN verifies 100 proofs (10 signers each) one
// at a time with VerifyAggregated.
func BenchmarkVerifyAggregatedSequentialN(b *testing.B) {
	items := buildBenchAggChecks(b, 100, 10)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		for _, item := range items {
			VerifyAggregated(item.Signature, item.Message, item.PublicKeys)
		}
	}
}

// BenchmarkVerifyAggregatedBatchN verifies the same 100 proofs (10 signers each)
// in parallel with VerifyAggregatedBatch.
func BenchmarkVerifyAggregatedBatchN(b *testing.B) {
	items := buildBenchAggChecks(b, 100, 10)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		VerifyAggregatedBatch(items)
	}
}
