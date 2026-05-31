package attest

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// AggCheck is one aggregated-signature verification request: a signature over a
// message together with the public keys it should aggregate against.
type AggCheck struct {
	Signature  []byte   // Signature is the aggregated BLS signature to verify
	Message    []byte   // Message is the message the signature covers
	PublicKeys [][]byte // PublicKeys are the signer public keys to aggregate
}

// VerifyAggregatedBatch verifies each item with the same logic as
// VerifyAggregated, spread across a worker pool bounded to GOMAXPROCS. It
// returns one bool per item in input order, where result[i] is exactly equal to
// VerifyAggregated(items[i].Signature, items[i].Message, items[i].PublicKeys).
//
// Each verification is a pure, independent function of its own item, so the
// goroutine scheduling cannot influence any result: parallelism only changes
// when work runs, never the per-item verdict or the output ordering.
func VerifyAggregatedBatch(items []AggCheck) []bool {
	results := make([]bool, len(items))
	if len(items) == 0 {
		return results
	}

	workers := runtime.GOMAXPROCS(0)
	if workers > len(items) {
		workers = len(items)
	}

	runAggCheckPool(items, results, workers)

	return results
}

// runAggCheckPool spreads the items across the given number of workers, writing
// each verdict into its own slot in results. Workers pull indices from a shared
// counter; each writes only to results[i], so there is no shared mutable state
// to race on.
func runAggCheckPool(items []AggCheck, results []bool, workers int) {
	var next int64
	var wg sync.WaitGroup

	wg.Add(workers)

	for w := 0; w < workers; w++ {
		go aggCheckWorker(items, results, &next, &wg)
	}

	wg.Wait()
}

// aggCheckWorker verifies items until the shared counter is exhausted, writing
// each verdict into its own results slot.
func aggCheckWorker(items []AggCheck, results []bool, next *int64, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		i := int(atomic.AddInt64(next, 1) - 1)
		if i >= len(items) {
			return
		}

		item := items[i]
		results[i] = VerifyAggregated(item.Signature, item.Message, item.PublicKeys)
	}
}
