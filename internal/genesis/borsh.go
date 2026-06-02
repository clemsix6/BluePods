package genesis

import "encoding/binary"

// EncodeSplitArgs encodes split arguments in Borsh format.
// Format: u64 amount (little-endian) + [u8; 32] new_owner
func EncodeSplitArgs(amount uint64, newOwner [32]byte) []byte {
	buf := make([]byte, 8+32)
	binary.LittleEndian.PutUint64(buf[0:8], amount)
	copy(buf[8:], newOwner[:])

	return buf
}

// encodeRegisterValidatorArgs encodes register_validator arguments in Borsh format.
// Format: u32 len + quic_address bytes + u32 len + bls_pubkey bytes
// Note: ed25519 pubkey is taken from tx.sender, not from args.
func encodeRegisterValidatorArgs(quicAddr, blsPubkey []byte) []byte {
	buf := make([]byte, 0, 4+len(quicAddr)+4+len(blsPubkey))

	// quic_address (Vec<u8>: u32 length prefix + bytes)
	lenBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(lenBuf, uint32(len(quicAddr)))
	buf = append(buf, lenBuf...)
	buf = append(buf, quicAddr...)

	// bls_pubkey (Vec<u8>: u32 length prefix + bytes)
	binary.LittleEndian.PutUint32(lenBuf, uint32(len(blsPubkey)))
	buf = append(buf, lenBuf...)
	buf = append(buf, blsPubkey...)

	return buf
}

// DecodeRegisterValidatorArgs decodes register_validator arguments from Borsh format.
// Returns quicAddr and blsPubkey.
// Returns empty/nil values if data is malformed.
// The blsPubkey is optional for backward compatibility (returns nil if absent).
func DecodeRegisterValidatorArgs(data []byte) (quicAddr string, blsPubkey []byte) {
	if len(data) < 4 {
		return "", nil
	}

	// Read quic_address length
	quicLen := binary.LittleEndian.Uint32(data[0:4])
	if len(data) < int(4+quicLen) {
		return "", nil
	}

	quicAddr = string(data[4 : 4+quicLen])

	// Read bls_pubkey (optional)
	offset := 4 + quicLen
	if uint32(len(data)) < offset+4 {
		return quicAddr, nil
	}

	blsLen := binary.LittleEndian.Uint32(data[offset : offset+4])
	if uint32(len(data)) < offset+4+blsLen {
		return quicAddr, nil
	}

	blsPubkey = make([]byte, blsLen)
	copy(blsPubkey, data[offset+4:offset+4+blsLen])

	return quicAddr, blsPubkey
}
