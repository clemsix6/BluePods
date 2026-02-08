package sync

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/consensus"
	"BluePods/internal/storage"
	"BluePods/internal/types"
)

// createTestStorage creates a temporary storage for testing.
func createTestStorage(t *testing.T) (*storage.Storage, func()) {
	t.Helper()

	dir, err := os.MkdirTemp("", "sync_test_*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}

	db, err := storage.New(filepath.Join(dir, "db"))
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("create storage: %v", err)
	}

	cleanup := func() {
		db.Close()
		os.RemoveAll(dir)
	}

	return db, cleanup
}

// buildTestObject creates a FlatBuffers Object for testing.
func buildTestObject(id [32]byte, version uint64, content []byte) []byte {
	return buildTestObjectWithReplication(id, version, content, 0)
}

// buildTestObjectWithReplication creates a FlatBuffers Object with a specific replication factor.
func buildTestObjectWithReplication(id [32]byte, version uint64, content []byte, replication uint16) []byte {
	builder := flatbuffers.NewBuilder(256)

	idOffset := builder.CreateByteVector(id[:])
	ownerOffset := builder.CreateByteVector(make([]byte, 32))
	contentOffset := builder.CreateByteVector(content)

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idOffset)
	types.ObjectAddVersion(builder, version)
	types.ObjectAddOwner(builder, ownerOffset)
	types.ObjectAddReplication(builder, replication)
	types.ObjectAddContent(builder, contentOffset)
	offset := types.ObjectEnd(builder)
	builder.Finish(offset)

	return builder.FinishedBytes()
}

func TestCreateSnapshot_EmptyStorage(t *testing.T) {
	db, cleanup := createTestStorage(t)
	defer cleanup()

	data, err := CreateSnapshot(db, 0, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	snapshot := types.GetRootAsSnapshot(data, 0)

	if snapshot.Version() != snapshotVersion {
		t.Errorf("version = %d, want %d", snapshot.Version(), snapshotVersion)
	}

	if snapshot.LastCommittedRound() != 0 {
		t.Errorf("lastCommittedRound = %d, want 0", snapshot.LastCommittedRound())
	}

	if snapshot.ObjectsLength() != 0 {
		t.Errorf("objects length = %d, want 0", snapshot.ObjectsLength())
	}

	if snapshot.ChecksumLength() != 32 {
		t.Errorf("checksum length = %d, want 32", snapshot.ChecksumLength())
	}
}

func TestCreateSnapshot_WithObjects(t *testing.T) {
	db, cleanup := createTestStorage(t)
	defer cleanup()

	// Create test objects
	id1 := [32]byte{1}
	id2 := [32]byte{2}
	obj1 := buildTestObject(id1, 1, []byte("content1"))
	obj2 := buildTestObject(id2, 2, []byte("content2"))

	// Store objects (using 32-byte keys)
	if err := db.Set(id1[:], obj1); err != nil {
		t.Fatalf("store obj1: %v", err)
	}
	if err := db.Set(id2[:], obj2); err != nil {
		t.Fatalf("store obj2: %v", err)
	}

	data, err := CreateSnapshot(db, 42, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	snapshot := types.GetRootAsSnapshot(data, 0)

	if snapshot.LastCommittedRound() != 42 {
		t.Errorf("lastCommittedRound = %d, want 42", snapshot.LastCommittedRound())
	}

	if snapshot.ObjectsLength() != 2 {
		t.Errorf("objects length = %d, want 2", snapshot.ObjectsLength())
	}
}

func TestCreateSnapshot_IgnoresConsensusData(t *testing.T) {
	db, cleanup := createTestStorage(t)
	defer cleanup()

	// Store an object
	id := [32]byte{1}
	obj := buildTestObject(id, 1, []byte("content"))
	if err := db.Set(id[:], obj); err != nil {
		t.Fatalf("store obj: %v", err)
	}

	// Store consensus data (should be ignored)
	if err := db.Set([]byte("v:somehash"), []byte("vertex")); err != nil {
		t.Fatalf("store vertex: %v", err)
	}
	if err := db.Set([]byte("r:00000001:hash"), []byte("round")); err != nil {
		t.Fatalf("store round: %v", err)
	}
	if err := db.Set([]byte("m:latestRound"), []byte{0, 0, 0, 0, 0, 0, 0, 1}); err != nil {
		t.Fatalf("store meta: %v", err)
	}

	data, err := CreateSnapshot(db, 0, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	snapshot := types.GetRootAsSnapshot(data, 0)

	// Should only have the 1 object, not the consensus data
	if snapshot.ObjectsLength() != 1 {
		t.Errorf("objects length = %d, want 1 (consensus data should be ignored)", snapshot.ObjectsLength())
	}
}

func TestCompressDecompress_Roundtrip(t *testing.T) {
	original := []byte("test data for compression roundtrip")

	compressed, err := CompressSnapshot(original)
	if err != nil {
		t.Fatalf("CompressSnapshot: %v", err)
	}

	decompressed, err := DecompressSnapshot(compressed)
	if err != nil {
		t.Fatalf("DecompressSnapshot: %v", err)
	}

	if !bytes.Equal(original, decompressed) {
		t.Errorf("roundtrip failed: got %q, want %q", decompressed, original)
	}
}

func TestCompressSnapshot_Reduces_Size(t *testing.T) {
	// Create repetitive data that compresses well
	original := bytes.Repeat([]byte("abcdefghij"), 1000)

	compressed, err := CompressSnapshot(original)
	if err != nil {
		t.Fatalf("CompressSnapshot: %v", err)
	}

	if len(compressed) >= len(original) {
		t.Errorf("compression did not reduce size: %d >= %d", len(compressed), len(original))
	}
}

func TestApplySnapshot_EmptySnapshot(t *testing.T) {
	db, cleanup := createTestStorage(t)
	defer cleanup()

	// Create empty snapshot
	data, err := CreateSnapshot(db, 5, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// Apply to a new storage
	db2, cleanup2 := createTestStorage(t)
	defer cleanup2()

	snapshot, err := ApplySnapshot(db2, data)
	if err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}

	if snapshot.LastCommittedRound() != 5 {
		t.Errorf("lastCommittedRound = %d, want 5", snapshot.LastCommittedRound())
	}
}

func TestApplySnapshot_WithObjects(t *testing.T) {
	db, cleanup := createTestStorage(t)
	defer cleanup()

	// Create and store objects
	id1 := [32]byte{1}
	id2 := [32]byte{2}
	obj1 := buildTestObject(id1, 1, []byte("content1"))
	obj2 := buildTestObject(id2, 2, []byte("content2"))

	if err := db.Set(id1[:], obj1); err != nil {
		t.Fatalf("store obj1: %v", err)
	}
	if err := db.Set(id2[:], obj2); err != nil {
		t.Fatalf("store obj2: %v", err)
	}

	// Create snapshot
	data, err := CreateSnapshot(db, 100, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// Apply to a new storage
	db2, cleanup2 := createTestStorage(t)
	defer cleanup2()

	snapshot, err := ApplySnapshot(db2, data)
	if err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}

	if snapshot.ObjectsLength() != 2 {
		t.Errorf("objects length = %d, want 2", snapshot.ObjectsLength())
	}

	// Verify objects were written to storage
	retrieved1, err := db2.Get(id1[:])
	if err != nil {
		t.Fatalf("get obj1: %v", err)
	}
	if !bytes.Equal(retrieved1, obj1) {
		t.Errorf("obj1 mismatch")
	}

	retrieved2, err := db2.Get(id2[:])
	if err != nil {
		t.Fatalf("get obj2: %v", err)
	}
	if !bytes.Equal(retrieved2, obj2) {
		t.Errorf("obj2 mismatch")
	}
}

func TestChecksumVerification_Valid(t *testing.T) {
	db, cleanup := createTestStorage(t)
	defer cleanup()

	id := [32]byte{42}
	obj := buildTestObject(id, 1, []byte("test content"))
	if err := db.Set(id[:], obj); err != nil {
		t.Fatalf("store obj: %v", err)
	}

	data, err := CreateSnapshot(db, 10, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// Apply should succeed with valid checksum
	db2, cleanup2 := createTestStorage(t)
	defer cleanup2()

	_, err = ApplySnapshot(db2, data)
	if err != nil {
		t.Fatalf("ApplySnapshot with valid checksum should succeed: %v", err)
	}
}

func TestChecksumVerification_Corrupted(t *testing.T) {
	db, cleanup := createTestStorage(t)
	defer cleanup()

	id := [32]byte{42}
	obj := buildTestObject(id, 1, []byte("test content"))
	if err := db.Set(id[:], obj); err != nil {
		t.Fatalf("store obj: %v", err)
	}

	data, err := CreateSnapshot(db, 10, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// Corrupt the data (flip a byte in the middle)
	corrupted := make([]byte, len(data))
	copy(corrupted, data)
	corrupted[len(corrupted)/2] ^= 0xFF

	// Apply should fail with corrupted data
	db2, cleanup2 := createTestStorage(t)
	defer cleanup2()

	_, err = ApplySnapshot(db2, corrupted)
	if err == nil {
		t.Fatalf("ApplySnapshot with corrupted data should fail")
	}
}

func TestFullRoundtrip_CreateCompressDecompressApply(t *testing.T) {
	db, cleanup := createTestStorage(t)
	defer cleanup()

	// Create multiple objects
	for i := 0; i < 10; i++ {
		id := [32]byte{byte(i)}
		content := bytes.Repeat([]byte{byte(i)}, 100)
		obj := buildTestObject(id, uint64(i), content)
		if err := db.Set(id[:], obj); err != nil {
			t.Fatalf("store obj %d: %v", i, err)
		}
	}

	// Create snapshot
	data, err := CreateSnapshot(db, 999, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// Compress
	compressed, err := CompressSnapshot(data)
	if err != nil {
		t.Fatalf("CompressSnapshot: %v", err)
	}

	// Decompress
	decompressed, err := DecompressSnapshot(compressed)
	if err != nil {
		t.Fatalf("DecompressSnapshot: %v", err)
	}

	// Apply
	db2, cleanup2 := createTestStorage(t)
	defer cleanup2()

	snapshot, err := ApplySnapshot(db2, decompressed)
	if err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}

	if snapshot.LastCommittedRound() != 999 {
		t.Errorf("lastCommittedRound = %d, want 999", snapshot.LastCommittedRound())
	}

	if snapshot.ObjectsLength() != 10 {
		t.Errorf("objects length = %d, want 10", snapshot.ObjectsLength())
	}

	// Verify all objects
	for i := 0; i < 10; i++ {
		id := [32]byte{byte(i)}
		retrieved, err := db2.Get(id[:])
		if err != nil {
			t.Fatalf("get obj %d: %v", i, err)
		}
		if retrieved == nil {
			t.Errorf("obj %d not found", i)
		}
	}
}

func TestDeterministicChecksum(t *testing.T) {
	// Create same objects in different order, checksum should be identical
	db1, cleanup1 := createTestStorage(t)
	defer cleanup1()

	db2, cleanup2 := createTestStorage(t)
	defer cleanup2()

	id1 := [32]byte{1}
	id2 := [32]byte{2}
	obj1 := buildTestObject(id1, 1, []byte("content1"))
	obj2 := buildTestObject(id2, 2, []byte("content2"))

	// Store in order 1, 2
	db1.Set(id1[:], obj1)
	db1.Set(id2[:], obj2)

	// Store in order 2, 1
	db2.Set(id2[:], obj2)
	db2.Set(id1[:], obj1)

	data1, _ := CreateSnapshot(db1, 42, nil, nil, nil)
	data2, _ := CreateSnapshot(db2, 42, nil, nil, nil)

	snap1 := types.GetRootAsSnapshot(data1, 0)
	snap2 := types.GetRootAsSnapshot(data2, 0)

	checksum1 := snap1.ChecksumBytes()
	checksum2 := snap2.ChecksumBytes()

	if !bytes.Equal(checksum1, checksum2) {
		t.Errorf("checksums should be identical regardless of insertion order")
	}
}

func TestCreateSnapshot_WithValidators(t *testing.T) {
	db, cleanup := createTestStorage(t)
	defer cleanup()

	// Create validators
	v1 := &consensus.ValidatorInfo{HTTPAddr: "127.0.0.1:8001"}
	v1.Pubkey[0] = 1
	v2 := &consensus.ValidatorInfo{HTTPAddr: "127.0.0.1:8002"}
	v2.Pubkey[0] = 2
	validators := []*consensus.ValidatorInfo{v1, v2}

	data, err := CreateSnapshot(db, 100, validators, nil, nil)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	snapshot := types.GetRootAsSnapshot(data, 0)

	// Extract and verify validators
	extracted := ExtractValidators(snapshot)
	if len(extracted) != 2 {
		t.Fatalf("extracted validators count = %d, want 2", len(extracted))
	}

	// Validators should be sorted (pubkey[0]=1 < pubkey[0]=2)
	if extracted[0].Pubkey[0] != 1 {
		t.Errorf("first validator pubkey[0] = %d, want 1", extracted[0].Pubkey[0])
	}
	if extracted[1].Pubkey[0] != 2 {
		t.Errorf("second validator pubkey[0] = %d, want 2", extracted[1].Pubkey[0])
	}
}

func TestSnapshotReplicationAffectsChecksum(t *testing.T) {
	db, cleanup := createTestStorage(t)
	defer cleanup()

	// Same objects, different replication in tracker entries
	trackerA := []consensus.ObjectTrackerEntry{
		{ID: consensus.Hash{1}, Version: 1, Replication: 3},
	}
	trackerB := []consensus.ObjectTrackerEntry{
		{ID: consensus.Hash{1}, Version: 1, Replication: 5},
	}

	dataA, err := CreateSnapshot(db, 0, nil, nil, trackerA)
	if err != nil {
		t.Fatalf("CreateSnapshot A: %v", err)
	}

	dataB, err := CreateSnapshot(db, 0, nil, nil, trackerB)
	if err != nil {
		t.Fatalf("CreateSnapshot B: %v", err)
	}

	snapA := types.GetRootAsSnapshot(dataA, 0)
	snapB := types.GetRootAsSnapshot(dataB, 0)

	if bytes.Equal(snapA.ChecksumBytes(), snapB.ChecksumBytes()) {
		t.Error("different replication values should produce different checksums")
	}
}

func TestSnapshotReplicationPreserved(t *testing.T) {
	db, cleanup := createTestStorage(t)
	defer cleanup()

	id := [32]byte{1}
	obj := buildTestObjectWithReplication(id, 1, []byte("content"), 5)
	if err := db.Set(id[:], obj); err != nil {
		t.Fatalf("store obj: %v", err)
	}

	tracker := []consensus.ObjectTrackerEntry{
		{ID: consensus.Hash{1}, Version: 1, Replication: 5},
	}

	data, err := CreateSnapshot(db, 10, nil, nil, tracker)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	snapshot := types.GetRootAsSnapshot(data, 0)

	// Extract tracker entries and verify replication preserved
	entries := ExtractTrackerEntries(snapshot)
	if len(entries) != 1 {
		t.Fatalf("expected 1 tracker entry, got %d", len(entries))
	}

	if entries[0].Replication != 5 {
		t.Errorf("expected replication=5, got %d", entries[0].Replication)
	}

	if entries[0].Version != 1 {
		t.Errorf("expected version=1, got %d", entries[0].Version)
	}

	// Verify checksum is still valid (ApplySnapshot checks it)
	db2, cleanup2 := createTestStorage(t)
	defer cleanup2()

	_, err = ApplySnapshot(db2, data)
	if err != nil {
		t.Fatalf("ApplySnapshot should succeed with valid checksum: %v", err)
	}
}

func TestSnapshot_ValidatorsAffectChecksum(t *testing.T) {
	db, cleanup := createTestStorage(t)
	defer cleanup()

	v1 := &consensus.ValidatorInfo{}
	v1.Pubkey[0] = 1
	v2 := &consensus.ValidatorInfo{}
	v2.Pubkey[0] = 2

	data1, _ := CreateSnapshot(db, 0, []*consensus.ValidatorInfo{v1}, nil, nil)
	data2, _ := CreateSnapshot(db, 0, []*consensus.ValidatorInfo{v2}, nil, nil)

	snap1 := types.GetRootAsSnapshot(data1, 0)
	snap2 := types.GetRootAsSnapshot(data2, 0)

	if bytes.Equal(snap1.ChecksumBytes(), snap2.ChecksumBytes()) {
		t.Error("different validators should produce different checksums")
	}
}
