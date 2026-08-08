package sync

import (
	"bytes"
	"testing"
)

// faultEvidenceKey mirrors the key shape the consensus commit path writes anchor
// fault evidence under: the fault/ prefix followed by the convicted vertex's
// 32-byte identity.
func faultEvidenceKey(vertex byte) []byte {
	key := append([]byte(nil), []byte("fault/")...)
	key = append(key, vertex)

	return append(key, make([]byte, 31)...)
}

// TestComputeFingerprint_AnchorFaultEvidenceExcluded holds the line between
// evidence and state. Anchor fault records are node-local: a lie is convicted by
// whichever nodes had committed its frontier when they applied it, so two honest
// nodes legitimately hold different fault sets. If the evidence reached the
// convergence fingerprint, catching a liar would make every scenario's teardown
// check fail on the nodes that caught it — the detector would look like the
// divergence.
func TestComputeFingerprint_AnchorFaultEvidenceExcluded(t *testing.T) {
	dag, st, db := newFingerprintTestTrio(t)

	var tracked [32]byte
	tracked[0] = 0x07
	dag.TrackObject(tracked, 1, 5, 0, 0, [32]byte{})

	before := ComputeFingerprint(dag, st)

	if err := db.Set(faultEvidenceKey(0xA1), make([]byte, 216)); err != nil {
		t.Fatalf("write fault evidence: %v", err)
	}
	if err := db.Set(faultEvidenceKey(0xB2), make([]byte, 216)); err != nil {
		t.Fatalf("write fault evidence: %v", err)
	}

	after := ComputeFingerprint(dag, st)

	if after.Checksum != before.Checksum {
		t.Fatalf("stored anchor fault evidence changed the convergence checksum: %x vs %x",
			before.Checksum, after.Checksum)
	}
}

// TestCreateSnapshot_AnchorFaultEvidenceExcluded pins the same separation on the
// shipping side: evidence a node collected is not state it hands a joiner, so it
// must not ride the snapshot or move its checksum.
func TestCreateSnapshot_AnchorFaultEvidenceExcluded(t *testing.T) {
	_, _, db := newFingerprintTestTrio(t)

	before, err := CreateSnapshot(db, 4, nil, nil, nil, nil, 0, 0, 0, nil)
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	if err := db.Set(faultEvidenceKey(0xC3), make([]byte, 216)); err != nil {
		t.Fatalf("write fault evidence: %v", err)
	}

	after, err := CreateSnapshot(db, 4, nil, nil, nil, nil, 0, 0, 0, nil)
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	if !bytes.Equal(after, before) {
		t.Fatal("stored anchor fault evidence changed the snapshot a joiner receives")
	}
}
