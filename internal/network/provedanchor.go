package network

import "encoding/binary"

// provedAnchorSize is the fixed width of the anchoring block every proved
// index response opens with: [1B anchored] [8B frontierRound] then the index
// root and the four component roots that combine into it, 32 bytes each.
const provedAnchorSize = 1 + 8 + 5*32

// ProvedIndexAnchor is that block. The five roots always describe the index
// state the accompanying proofs were taken against, so a proof always folds to
// one of them, and the four component roots are what let a verifier recompute
// IndexRoot (a proof folds to ONE component root, never to the combination).
// FrontierRound is the committed round IndexRoot is anchored at and is
// meaningful only when Anchored: verification runs by matching IndexRoot
// against a GetIndexAnchor bundle, which exists only for a root some committed
// frontier recorded. Anchored=false means the serving node's index currently
// sits between a mutation and the commit that will record it — the answer is a
// live unproven read and the client retries for a provable one.
type ProvedIndexAnchor struct {
	Anchored      bool     // Anchored reports whether IndexRoot is the root recorded at FrontierRound
	FrontierRound uint64   // FrontierRound is the committed round IndexRoot anchors, when Anchored
	IndexRoot     [32]byte // IndexRoot is the combined index root the proofs are tied to
	DomainRoot    [32]byte // DomainRoot is the domain tree's root
	ParentRoot    [32]byte // ParentRoot is the parent tree's root
	ChildrenRoot  [32]byte // ChildrenRoot is the children top tree's root
	ValidatorRoot [32]byte // ValidatorRoot is the validator tree's root
}

// putProvedAnchor writes the anchoring block at the front of buf, which must
// hold at least provedAnchorSize bytes, and returns how many it wrote.
func putProvedAnchor(buf []byte, a ProvedIndexAnchor) int {
	if a.Anchored {
		buf[0] = 1
	}

	binary.BigEndian.PutUint64(buf[1:9], a.FrontierRound)
	copy(buf[9:41], a.IndexRoot[:])
	copy(buf[41:73], a.DomainRoot[:])
	copy(buf[73:105], a.ParentRoot[:])
	copy(buf[105:137], a.ChildrenRoot[:])
	copy(buf[137:169], a.ValidatorRoot[:])

	return provedAnchorSize
}

// readProvedAnchor reads the anchoring block from the front of data, which the
// caller has already checked is at least provedAnchorSize bytes long.
func readProvedAnchor(data []byte) ProvedIndexAnchor {
	a := ProvedIndexAnchor{
		Anchored:      data[0] == 1,
		FrontierRound: binary.BigEndian.Uint64(data[1:9]),
	}

	copy(a.IndexRoot[:], data[9:41])
	copy(a.DomainRoot[:], data[41:73])
	copy(a.ParentRoot[:], data[73:105])
	copy(a.ChildrenRoot[:], data[105:137])
	copy(a.ValidatorRoot[:], data[137:169])

	return a
}

// putBlob writes a 4-byte big-endian length followed by blob at the front of
// buf, and returns how many bytes it wrote.
func putBlob(buf, blob []byte) int {
	binary.BigEndian.PutUint32(buf[:4], uint32(len(blob)))
	copy(buf[4:], blob)

	return 4 + len(blob)
}

// readBlob reads a length-prefixed blob from the front of data and returns it
// with the remaining bytes. ok is false on any truncation, so a short or
// malformed payload from a Byzantine peer surfaces as a decode error rather
// than a silently shortened value.
func readBlob(data []byte) (blob, rest []byte, ok bool) {
	if len(data) < 4 {
		return nil, nil, false
	}

	n := int(binary.BigEndian.Uint32(data[:4]))
	if len(data) < 4+n {
		return nil, nil, false
	}

	if n == 0 {
		return nil, data[4:], true
	}

	return append([]byte(nil), data[4:4+n]...), data[4+n:], true
}
