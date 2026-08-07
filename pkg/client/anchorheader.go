package client

import (
	"crypto/ed25519"
	"encoding/binary"
	"fmt"

	"github.com/zeebo/blake3"

	"BluePods/internal/network"
)

// The detached vertex header a light client verifies an anchor from. This
// package deliberately does not import internal/consensus (a client library
// linking the consensus engine would drag the whole node in), so the layout is
// reproduced here from the NORMATIVE wire contract stated on headerSize in
// internal/consensus/header.go: a positional, fixed-width, big-endian byte
// string with no lengths and no separators.
//
//	offset  size  field
//	0       32    producer
//	32      8     round
//	40      8     epoch
//	48      8     frontier_round
//	56      32    index_root
//	88      32    body_hash
//	120           total
//
// The vertex identity is BLAKE3(headerDomainTag || header) and the producer's
// Ed25519 signature is taken over that identity, never over the header bytes.
// TestAnchorHeader_GoldenLayout pins this reimplementation to the same fixed
// vector consensus pins its own encoding to, so a change on either side fails
// a test here instead of silently disagreeing on the wire.
const (
	// anchorHeaderSize is the header's width inside a bundle record, derived
	// from the record width rather than restated, so the two can never drift.
	anchorHeaderSize = network.IndexAnchorHeaderSize - ed25519.SignatureSize

	// headerDomainTag separates the header hash from the body hash. Omitting
	// it yields a different identity, which does not fail loudly — it silently
	// disagrees with every node on the network.
	headerDomainTag = 0x01

	offsetRound        = 32 // offsetRound is where the production round starts
	offsetEpoch        = 40 // offsetEpoch is where the producer's epoch starts
	offsetFrontier     = 48 // offsetFrontier is where the anchored frontier round starts
	offsetIndexRoot    = 56 // offsetIndexRoot is where the anchored index root starts
	offsetIndexRootEnd = 88 // offsetIndexRootEnd ends the index root
)

// anchorHeader is one producer's signed claim inside a quorum bundle: the
// index root it anchors, the frontier it anchors it at, and the epoch naming
// the validator set that weighs it.
type anchorHeader struct {
	Producer      [32]byte // Producer is the producing validator's Ed25519 public key
	Epoch         uint64   // Epoch is the producer's epoch at production time
	FrontierRound uint64   // FrontierRound is the committed round IndexRoot anchors
	IndexRoot     [32]byte // IndexRoot is the index root this producer signed for
}

// parseAnchorRecord reads one {header ‖ signature} record from a bundle and
// verifies the producer's own signature over the vertex identity its header
// implies. An unsigned or mis-signed record is an error, never a header with a
// flag: a record whose signature was not checked must not be reachable as a
// value at all, or a caller could weigh it by forgetting one boolean.
func parseAnchorRecord(record []byte) (anchorHeader, error) {
	if len(record) != network.IndexAnchorHeaderSize {
		return anchorHeader{}, fmt.Errorf("anchor record is %d bytes, want %d", len(record), network.IndexAnchorHeaderSize)
	}

	header := record[:anchorHeaderSize]

	var out anchorHeader
	copy(out.Producer[:], header[:offsetRound])
	out.Epoch = binary.BigEndian.Uint64(header[offsetEpoch:offsetFrontier])
	out.FrontierRound = binary.BigEndian.Uint64(header[offsetFrontier:offsetIndexRoot])
	copy(out.IndexRoot[:], header[offsetIndexRoot:offsetIndexRootEnd])

	identity := headerIdentity(header)
	if !ed25519.Verify(out.Producer[:], identity[:], record[anchorHeaderSize:]) {
		return anchorHeader{}, fmt.Errorf("anchor record for producer %x carries no valid signature", out.Producer[:8])
	}

	return out, nil
}

// headerIdentity returns the vertex identity a producer signs:
// BLAKE3(headerDomainTag || header).
func headerIdentity(header []byte) [32]byte {
	digest := blake3.New()
	_, _ = digest.Write([]byte{headerDomainTag})
	_, _ = digest.Write(header)

	var out [32]byte
	copy(out[:], digest.Sum(nil))

	return out
}
