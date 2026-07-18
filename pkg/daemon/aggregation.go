package daemon

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"BluePods/internal/attest"
	"BluePods/internal/types"
	"BluePods/internal/validators"
)

const (
	// maxRetries bounds how many times SubmitTransaction recollects on a version
	// race before surfacing a typed error to the application.
	maxRetries = 4

	// backoffBase is the base delay for the randomized retry backoff.
	backoffBase = 25 * time.Millisecond
)

// ErrQuorumImpossible is returned when enough holders refuse to attest (a version
// race or an unreachable holder set) that the per-object quorum can no longer be
// reached, even after a validator-set resync and bounded retries.
var ErrQuorumImpossible = errors.New("attestation quorum impossible")

// objectRef is a replicated object the daemon must collect attestations for.
type objectRef struct {
	ID      [32]byte // ID is the object identifier
	Version uint64   // Version is the version declared by the transaction
}

// attestationResult holds the collected quorum proof for one replicated object.
type attestationResult struct {
	ObjectID    [32]byte // ObjectID is the attested object
	ObjectData  []byte   // ObjectData is the fetched Object FlatBuffer
	Version     uint64   // Version is the attested current version
	Replication int      // Replication is the object's replication factor
	AggSig      []byte   // AggSig is the aggregated BLS signature over H
	Bitmap      []byte   // Bitmap marks which holders (in rendezvous order) signed
}

// CollectAttestations gathers a quorum proof for each replicated object ref. For
// each ref it fetches the object to learn its content, version, and replication,
// then computes the holders by rendezvous hashing, then requests attestations
// from them in parallel with fail-fast when quorum becomes impossible. It
// verifies locally that every signature is on the same H and that H matches the
// fetched object's content. Singletons must be excluded by the caller.
func (d *Daemon) CollectAttestations(ctx context.Context, refs []objectRef) ([]attestationResult, error) {
	results := make([]attestationResult, 0, len(refs))

	for _, ref := range refs {
		result, err := d.collectOne(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("collect object %x:\n%w", ref.ID[:4], err)
		}

		results = append(results, result)
	}

	return results, nil
}

// collectOne fetches one object, computes its holders, and collects a quorum of
// attestations over the canonical hash of its content.
func (d *Daemon) collectOne(ctx context.Context, ref objectRef) (attestationResult, error) {
	objData, err := d.GetObject(ref.ID)
	if err != nil {
		return attestationResult{}, fmt.Errorf("fetch object:\n%w", err)
	}
	if objData == nil {
		return attestationResult{}, fmt.Errorf("object not found")
	}

	obj := types.GetRootAsObject(objData, 0)
	version := obj.Version()
	replication := int(obj.Replication())

	if replication == 0 {
		return attestationResult{}, fmt.Errorf("singleton must not be collected")
	}

	// Bind the fetched object's owner into the hash the holders sign and this
	// collector verifies against, matching the commit-time recompute. A quorum is
	// then only assembled for the owner the holders actually attest.
	hash := attest.ComputeObjectHash(obj.ContentBytes(), version, obj.OwnerBytes())

	vs := d.Validators()
	holders := attest.ComputeHolders(vs, ref.ID, replication)
	quorum := attest.QuorumSize(len(holders))

	sigs, indices, err := d.requestQuorum(ctx, vs, holders, ref.ID, version, hash, quorum)
	if err != nil {
		return attestationResult{}, err
	}

	aggSig, err := attest.AggregateSignatures(sigs)
	if err != nil {
		return attestationResult{}, fmt.Errorf("aggregate signatures:\n%w", err)
	}

	return attestationResult{
		ObjectID:    ref.ID,
		ObjectData:  objData,
		Version:     version,
		Replication: replication,
		AggSig:      aggSig,
		Bitmap:      attest.BuildSignerBitmap(indices, len(holders)),
	}, nil
}

// holderSig pairs a holder's rendezvous index with its returned signature.
type holderSig struct {
	index int    // index is the holder's position in rendezvous order
	sig   []byte // sig is the BLS signature over H
}

// requestQuorum requests attestations from holders in parallel. It collects
// signatures until quorum is met and fails fast (cancelling outstanding requests)
// once enough negatives make quorum impossible. Each returned signature is locally
// verified to be on the expected hash H.
func (d *Daemon) requestQuorum(ctx context.Context, vs *validators.ValidatorSet, holders []validators.Hash, id [32]byte, version uint64, hash [32]byte, quorum int) ([][]byte, []int, error) {
	collectCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan holderSig, len(holders))
	failures := make(chan struct{}, len(holders))

	var wg sync.WaitGroup
	for i, h := range holders {
		info := vs.Get(h)
		if info == nil || info.QUICAddr == "" {
			failures <- struct{}{}
			continue
		}

		wg.Add(1)
		go func(index int, addr string, blsPub [48]byte) {
			defer wg.Done()
			d.requestOne(collectCtx, index, addr, blsPub, id, version, hash, results, failures)
		}(i, info.QUICAddr, info.BLSPubkey)
	}

	go func() { wg.Wait(); close(results) }()

	return collectUntilQuorum(results, failures, len(holders), quorum)
}

// requestOne asks one holder for an attestation and verifies the returned
// signature is on the expected hash before forwarding it.
func (d *Daemon) requestOne(ctx context.Context, index int, addr string, blsPub [48]byte, id [32]byte, version uint64, hash [32]byte, results chan<- holderSig, failures chan<- struct{}) {
	resp, err := d.attestRoundTrip(ctx, addr, id, version)
	if err != nil {
		failures <- struct{}{}
		return
	}

	msgType, err := attest.GetMessageType(resp)
	if err != nil || msgType != attest.MsgTypePositive {
		failures <- struct{}{}
		return
	}

	positive, err := attest.DecodePositiveResponse(resp)
	if err != nil || positive.Hash != hash {
		failures <- struct{}{}
		return
	}

	if !attest.Verify(positive.Signature, hash[:], blsPub[:]) {
		failures <- struct{}{}
		return
	}

	results <- holderSig{index: index, sig: positive.Signature}
}

// collectUntilQuorum drains results and failures, returning the collected
// signatures and their holder indices once quorum is met, or ErrQuorumImpossible
// once too many holders fail to leave a path to quorum.
func collectUntilQuorum(results <-chan holderSig, failures <-chan struct{}, total, quorum int) ([][]byte, []int, error) {
	var sigs [][]byte
	var indices []int
	failed := 0

	for {
		select {
		case hs, ok := <-results:
			if !ok {
				if len(sigs) >= quorum {
					return sigs, indices, nil
				}
				return nil, nil, ErrQuorumImpossible
			}

			sigs = append(sigs, hs.sig)
			indices = append(indices, hs.index)

			if len(sigs) >= quorum {
				return sigs, indices, nil
			}
		case <-failures:
			failed++
			if total-failed < quorum {
				return nil, nil, ErrQuorumImpossible
			}
		}
	}
}

// attestRoundTrip dials a holder over QUIC and performs one attestation request.
func (d *Daemon) attestRoundTrip(ctx context.Context, addr string, id [32]byte, version uint64) ([]byte, error) {
	dialCtx, dialCancel := context.WithTimeout(ctx, dialTimeout)
	defer dialCancel()

	conn, err := quic.DialAddr(dialCtx, addr, d.tlsConfig, d.quicConfig)
	if err != nil {
		return nil, fmt.Errorf("dial holder:\n%w", err)
	}
	defer conn.CloseWithError(0, "")

	reqCtx, reqCancel := context.WithTimeout(ctx, requestTimeout)
	defer reqCancel()

	stream, err := conn.OpenStreamSync(reqCtx)
	if err != nil {
		return nil, fmt.Errorf("open stream:\n%w", err)
	}
	defer stream.Close()

	if deadline, ok := reqCtx.Deadline(); ok {
		stream.SetDeadline(deadline)
	}

	request := attest.EncodeRequest(&attest.AttestationRequest{ObjectID: id, Version: version})
	if err := writeFrame(stream, request); err != nil {
		return nil, fmt.Errorf("write request:\n%w", err)
	}

	return readFrame(stream)
}

// backoff sleeps a bounded randomized delay before a retry.
func backoff(ctx context.Context, attempt int) {
	delay := backoffBase * time.Duration(1<<attempt)
	jitter := time.Duration(rand.Int63n(int64(backoffBase) + 1))

	select {
	case <-ctx.Done():
	case <-time.After(delay + jitter):
	}
}
