package client

import (
	"errors"
	"fmt"

	"BluePods/internal/network"
)

// ErrCheckpointBehind reports a bundle whose headers all name an epoch
// outside the checkpoint's handoff window {Epoch, Epoch+1}: every header the
// node could possibly weigh has already moved past what this client's
// committee is authorized to attest. That is the weak-subjectivity boundary
// spec §5 describes, not a quorum failure — retrying gets the same refusal
// forever, and the caller's move is a fresh out-of-band checkpoint, never
// another poll.
var ErrCheckpointBehind = errors.New("checkpoint's handoff window no longer covers any served header")

// refusalError explains why neither epoch's own header subset carried set's
// quorum. It distinguishes the weak-subjectivity boundary — every PARSABLE
// header in the bundle names an epoch outside {set.Epoch, set.Epoch+1}, so
// nothing this committee is authorized to weigh even exists in the bundle —
// from an ordinary shortfall, which reads identically as "0 of N capped
// stake" unless called out on its own.
func refusalError(bundle *network.GetIndexAnchorResponse, set ValidatorSet, atCurrentEpoch, atNextEpoch map[[32]byte]bool) error {
	if len(atCurrentEpoch) == 0 && len(atNextEpoch) == 0 && len(bundle.Headers) > 0 && allHeadersOutsideWindow(bundle, set) {
		return fmt.Errorf("bundle at frontier %d: no header falls inside the handoff window {%d, %d}: obtain a fresh checkpoint:\n%w",
			bundle.FrontierRound, set.Epoch, set.Epoch+1, ErrCheckpointBehind)
	}

	currentAttesting, total := cappedStakeOf(set, atCurrentEpoch)
	nextAttesting, _ := cappedStakeOf(set, atNextEpoch)

	return fmt.Errorf("bundle at frontier %d carries %d of %d capped stake at epoch %d and %d of %d at epoch %d, short of two thirds at either epoch on its own",
		bundle.FrontierRound, currentAttesting, total, set.Epoch, nextAttesting, total, set.Epoch+1)
}

// allHeadersOutsideWindow reports whether every PARSABLE header the bundle
// carries names an epoch outside {set.Epoch, set.Epoch+1}, regardless of
// whether it otherwise repeats the bundle's claim or comes from a known
// member. A bundle with no parsable header at all — every record fails
// parseAnchorRecord, for instance a corrupted signature — names no epoch and
// so is never "outside the window": that reading would let a single
// malformed or malicious record evict a light client's valid checkpoint,
// which is the weak-subjectivity boundary's own remedy and not something a
// stray bad record should be able to trigger. At least one header must
// parse, and every one that does must fall outside the window, for this to
// report true; anything else is an ordinary quorum shortfall.
func allHeadersOutsideWindow(bundle *network.GetIndexAnchorResponse, set ValidatorSet) bool {
	sawParsable := false

	for _, record := range bundle.Headers {
		header, err := parseAnchorRecord(record)
		if err != nil {
			continue
		}

		sawParsable = true

		if header.Epoch == set.Epoch || header.Epoch == set.Epoch+1 {
			return false
		}
	}

	return sawParsable
}
