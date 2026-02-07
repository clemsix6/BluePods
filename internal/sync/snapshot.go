package sync

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/klauspost/compress/zstd"
	"github.com/zeebo/blake3"

	"BluePods/internal/consensus"
	"BluePods/internal/storage"
	"BluePods/internal/types"
)

const (
	// snapshotVersion is the current snapshot format version.
	snapshotVersion = 1

	// objectKeySize is the size of object keys (32 bytes for ID).
	objectKeySize = 32
)

// Storage key prefixes used by consensus (must be skipped when iterating objects).
var (
	prefixVertex = []byte("v:")
	prefixRound  = []byte("r:")
	prefixMeta   = []byte("m:")
)

// CreateSnapshot creates a snapshot of the current committed state.
// It iterates over all objects in storage, excluding consensus data.
func CreateSnapshot(db *storage.Storage, lastCommittedRound uint64, validators []*consensus.ValidatorInfo, vertices []consensus.VertexEntry, versions []consensus.ObjectVersionEntry) ([]byte, error) {
	objects, err := collectObjects(db)
	if err != nil {
		return nil, fmt.Errorf("collect objects:\n%w", err)
	}

	data := buildSnapshot(lastCommittedRound, objects, validators, vertices, versions)

	return data, nil
}

// collectObjects iterates storage and returns all objects (excluding consensus data).
func collectObjects(db *storage.Storage) ([]objectEntry, error) {
	var objects []objectEntry

	err := db.Iterate(func(key, value []byte) error {
		if isConsensusKey(key) {
			return nil
		}

		// Object keys are exactly 32 bytes
		if len(key) != objectKeySize {
			return nil
		}

		// Copy key and value to avoid iterator invalidation
		keyCopy := make([]byte, len(key))
		copy(keyCopy, key)

		valueCopy := make([]byte, len(value))
		copy(valueCopy, value)

		objects = append(objects, objectEntry{
			id:   keyCopy,
			data: valueCopy,
		})

		return nil
	})

	if err != nil {
		return nil, err
	}

	return objects, nil
}

// objectEntry holds an object's ID and serialized data.
type objectEntry struct {
	id   []byte
	data []byte
}

// isConsensusKey returns true if the key belongs to consensus data.
func isConsensusKey(key []byte) bool {
	return bytes.HasPrefix(key, prefixVertex) ||
		bytes.HasPrefix(key, prefixRound) ||
		bytes.HasPrefix(key, prefixMeta)
}

// buildSnapshot creates the FlatBuffers snapshot with checksum.
func buildSnapshot(lastCommittedRound uint64, objects []objectEntry, validators []*consensus.ValidatorInfo, vertices []consensus.VertexEntry, versions []consensus.ObjectVersionEntry) []byte {
	// Sort objects by ID for deterministic checksum
	sortObjects(objects)

	// Sort validators for deterministic checksum
	sortValidatorInfos(validators)

	// Compute checksum over canonical data
	checksum := computeChecksumWithInfo(snapshotVersion, lastCommittedRound, objects, validators)

	// Build FlatBuffers
	builder := flatbuffers.NewBuilder(1024)

	// Build objects vector
	objectOffsets := make([]flatbuffers.UOffsetT, len(objects))
	for i, obj := range objects {
		idOffset := builder.CreateByteVector(obj.id)
		dataOffset := builder.CreateByteVector(obj.data)

		types.SnapshotObjectStart(builder)
		types.SnapshotObjectAddId(builder, idOffset)
		types.SnapshotObjectAddData(builder, dataOffset)
		objectOffsets[i] = types.SnapshotObjectEnd(builder)
	}

	types.SnapshotStartObjectsVector(builder, len(objectOffsets))
	for i := len(objectOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(objectOffsets[i])
	}
	objectsVector := builder.EndVector(len(objectOffsets))

	// Build validators vector (encoded with addresses)
	validatorsData := encodeValidators(validators)
	validatorsOffset := builder.CreateByteVector(validatorsData)

	checksumOffset := builder.CreateByteVector(checksum[:])

	// Build vertices vector
	vertexOffsets := make([]flatbuffers.UOffsetT, len(vertices))
	for i, v := range vertices {
		dataOffset := builder.CreateByteVector(v.Data)

		types.SnapshotVertexStart(builder)
		types.SnapshotVertexAddRound(builder, v.Round)
		types.SnapshotVertexAddData(builder, dataOffset)
		vertexOffsets[i] = types.SnapshotVertexEnd(builder)
	}

	types.SnapshotStartVerticesVector(builder, len(vertexOffsets))
	for i := len(vertexOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(vertexOffsets[i])
	}
	verticesVector := builder.EndVector(len(vertexOffsets))

	// Build object versions vector
	versionOffsets := make([]flatbuffers.UOffsetT, len(versions))
	for i, v := range versions {
		idOffset := builder.CreateByteVector(v.ID[:])

		types.ObjectVersionStart(builder)
		types.ObjectVersionAddId(builder, idOffset)
		types.ObjectVersionAddVersion(builder, v.Version)
		versionOffsets[i] = types.ObjectVersionEnd(builder)
	}

	types.SnapshotStartObjectVersionsVector(builder, len(versionOffsets))
	for i := len(versionOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(versionOffsets[i])
	}
	versionsVector := builder.EndVector(len(versionOffsets))

	types.SnapshotStart(builder)
	types.SnapshotAddVersion(builder, snapshotVersion)
	types.SnapshotAddLastCommittedRound(builder, lastCommittedRound)
	types.SnapshotAddObjects(builder, objectsVector)
	types.SnapshotAddValidators(builder, validatorsOffset)
	types.SnapshotAddChecksum(builder, checksumOffset)
	types.SnapshotAddVertices(builder, verticesVector)
	types.SnapshotAddObjectVersions(builder, versionsVector)
	offset := types.SnapshotEnd(builder)
	builder.Finish(offset)

	return builder.FinishedBytes()
}

// sortValidatorInfos sorts validators by pubkey for deterministic ordering.
func sortValidatorInfos(validators []*consensus.ValidatorInfo) {
	sort.Slice(validators, func(i, j int) bool {
		return bytes.Compare(validators[i].Pubkey[:], validators[j].Pubkey[:]) < 0
	})
}

// encodeValidators encodes validators with their addresses and BLS pubkey.
// Format: for each validator: 32-byte pubkey + u16 http_len + http_bytes + u16 quic_len + quic_bytes + 48-byte bls_pubkey
func encodeValidators(validators []*consensus.ValidatorInfo) []byte {
	var buf bytes.Buffer

	for _, v := range validators {
		// Pubkey (32 bytes)
		buf.Write(v.Pubkey[:])

		// HTTP address (u16 len + bytes)
		httpBytes := []byte(v.HTTPAddr)
		lenBuf := make([]byte, 2)
		binary.LittleEndian.PutUint16(lenBuf, uint16(len(httpBytes)))
		buf.Write(lenBuf)
		buf.Write(httpBytes)

		// QUIC address (u16 len + bytes)
		quicBytes := []byte(v.QUICAddr)
		binary.LittleEndian.PutUint16(lenBuf, uint16(len(quicBytes)))
		buf.Write(lenBuf)
		buf.Write(quicBytes)

		// BLS pubkey (48 bytes fixed)
		buf.Write(v.BLSPubkey[:])
	}

	return buf.Bytes()
}

// decodeValidators decodes validators from snapshot format.
func decodeValidators(data []byte) []*consensus.ValidatorInfo {
	var validators []*consensus.ValidatorInfo

	for len(data) >= 32 {
		v := &consensus.ValidatorInfo{}

		// Pubkey (32 bytes)
		copy(v.Pubkey[:], data[:32])
		data = data[32:]

		// HTTP address
		if len(data) < 2 {
			break
		}
		httpLen := binary.LittleEndian.Uint16(data[:2])
		data = data[2:]
		if len(data) < int(httpLen) {
			break
		}
		v.HTTPAddr = string(data[:httpLen])
		data = data[httpLen:]

		// QUIC address
		if len(data) < 2 {
			break
		}
		quicLen := binary.LittleEndian.Uint16(data[:2])
		data = data[2:]
		if len(data) < int(quicLen) {
			break
		}
		v.QUICAddr = string(data[:quicLen])
		data = data[quicLen:]

		// BLS pubkey (48 bytes fixed)
		if len(data) < 48 {
			break
		}
		copy(v.BLSPubkey[:], data[:48])
		data = data[48:]

		validators = append(validators, v)
	}

	return validators
}

// sortObjects sorts objects by ID for deterministic ordering.
func sortObjects(objects []objectEntry) {
	sort.Slice(objects, func(i, j int) bool {
		return bytes.Compare(objects[i].id, objects[j].id) < 0
	})
}

// computeChecksumWithInfo computes a blake3 checksum over canonical snapshot data.
// Format: version (4 bytes) + round (8 bytes) + encoded validators + objects
func computeChecksumWithInfo(version uint32, round uint64, objects []objectEntry, validators []*consensus.ValidatorInfo) [32]byte {
	hasher := blake3.New()

	// Write version
	var buf [8]byte
	binary.BigEndian.PutUint32(buf[:4], version)
	hasher.Write(buf[:4])

	// Write round
	binary.BigEndian.PutUint64(buf[:], round)
	hasher.Write(buf[:])

	// Write encoded validators (already sorted)
	validatorsData := encodeValidators(validators)
	binary.BigEndian.PutUint32(buf[:4], uint32(len(validatorsData)))
	hasher.Write(buf[:4])
	hasher.Write(validatorsData)

	// Write each object (already sorted)
	for _, obj := range objects {
		hasher.Write(obj.id)
		binary.BigEndian.PutUint32(buf[:4], uint32(len(obj.data)))
		hasher.Write(buf[:4])
		hasher.Write(obj.data)
	}

	var checksum [32]byte
	hasher.Sum(checksum[:0])

	return checksum
}

// CompressSnapshot compresses snapshot data using zstd.
func CompressSnapshot(data []byte) ([]byte, error) {
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, fmt.Errorf("create encoder:\n%w", err)
	}
	defer encoder.Close()

	return encoder.EncodeAll(data, nil), nil
}

// DecompressSnapshot decompresses zstd-compressed snapshot data.
func DecompressSnapshot(data []byte) ([]byte, error) {
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("create decoder:\n%w", err)
	}
	defer decoder.Close()

	return decoder.DecodeAll(data, nil)
}

// ApplySnapshot applies a snapshot to storage, replacing all objects.
func ApplySnapshot(db *storage.Storage, data []byte) (*types.Snapshot, error) {
	snapshot := types.GetRootAsSnapshot(data, 0)

	// Verify checksum
	if err := verifyChecksum(data, snapshot); err != nil {
		return nil, fmt.Errorf("verify checksum:\n%w", err)
	}

	// Collect objects to write
	pairs := make([]storage.KeyValue, snapshot.ObjectsLength())
	var obj types.SnapshotObject

	for i := 0; i < snapshot.ObjectsLength(); i++ {
		if !snapshot.Objects(&obj, i) {
			return nil, fmt.Errorf("read object %d", i)
		}

		pairs[i] = storage.KeyValue{
			Key:   obj.IdBytes(),
			Value: obj.DataBytes(),
		}
	}

	// Write all objects atomically
	if err := db.SetBatch(pairs); err != nil {
		return nil, fmt.Errorf("write objects:\n%w", err)
	}

	return snapshot, nil
}

// verifyChecksum verifies the snapshot's integrity.
func verifyChecksum(data []byte, snapshot *types.Snapshot) error {
	storedChecksum := snapshot.ChecksumBytes()
	if len(storedChecksum) != 32 {
		return fmt.Errorf("invalid checksum length: %d", len(storedChecksum))
	}

	// Extract objects from snapshot (must copy bytes as FlatBuffers reuses buffer)
	objects := make([]objectEntry, snapshot.ObjectsLength())
	var obj types.SnapshotObject

	for i := 0; i < snapshot.ObjectsLength(); i++ {
		if !snapshot.Objects(&obj, i) {
			return fmt.Errorf("read object %d", i)
		}

		// Copy bytes to avoid FlatBuffers buffer reuse issues
		idBytes := obj.IdBytes()
		dataBytes := obj.DataBytes()

		id := make([]byte, len(idBytes))
		copy(id, idBytes)

		objData := make([]byte, len(dataBytes))
		copy(objData, dataBytes)

		objects[i] = objectEntry{
			id:   id,
			data: objData,
		}
	}

	// Extract validators
	validators := ExtractValidators(snapshot)

	// Sort and compute checksum
	sortObjects(objects)
	sortValidatorInfos(validators)
	computed := computeChecksumWithInfo(snapshot.Version(), snapshot.LastCommittedRound(), objects, validators)

	// Compare
	if !bytes.Equal(computed[:], storedChecksum) {
		return fmt.Errorf("checksum mismatch")
	}

	return nil
}

// ExtractValidators extracts validators with addresses from a snapshot.
func ExtractValidators(snapshot *types.Snapshot) []*consensus.ValidatorInfo {
	validatorsData := snapshot.ValidatorsBytes()
	if len(validatorsData) == 0 {
		return nil
	}

	return decodeValidators(validatorsData)
}

// ExtractVertices extracts DAG vertices from a snapshot.
func ExtractVertices(snapshot *types.Snapshot) []consensus.VertexEntry {
	length := snapshot.VerticesLength()
	if length == 0 {
		return nil
	}

	entries := make([]consensus.VertexEntry, length)
	var v types.SnapshotVertex

	for i := 0; i < length; i++ {
		if !snapshot.Vertices(&v, i) {
			continue
		}

		// Copy data to avoid FlatBuffers buffer reuse
		data := v.DataBytes()
		dataCopy := make([]byte, len(data))
		copy(dataCopy, data)

		entries[i] = consensus.VertexEntry{
			Round: v.Round(),
			Data:  dataCopy,
		}
	}

	return entries
}

// ExtractVersions extracts object versions from a snapshot.
func ExtractVersions(snapshot *types.Snapshot) []consensus.ObjectVersionEntry {
	length := snapshot.ObjectVersionsLength()
	if length == 0 {
		return nil
	}

	entries := make([]consensus.ObjectVersionEntry, length)
	var v types.ObjectVersion

	for i := 0; i < length; i++ {
		if !snapshot.ObjectVersions(&v, i) {
			continue
		}

		var id consensus.Hash
		idBytes := v.IdBytes()
		if len(idBytes) == 32 {
			copy(id[:], idBytes)
		}

		entries[i] = consensus.ObjectVersionEntry{
			ID:      id,
			Version: v.Version(),
		}
	}

	return entries
}
