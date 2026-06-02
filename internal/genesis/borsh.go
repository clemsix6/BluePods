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
	quicAddr, blsPubkey, _ = decodeRegisterValidatorArgs(data)
	return quicAddr, blsPubkey
}

// DecodeRegisterValidatorRewardCoin decodes the optional reward-coin designation
// that trails a register_validator's args (a Borsh Vec<u8> after the BLS key). It
// returns ok=false when no 32-byte reward coin is present, so an absent
// designation is distinguishable from a zero one.
func DecodeRegisterValidatorRewardCoin(data []byte) (rewardCoin [32]byte, ok bool) {
	_, _, raw := decodeRegisterValidatorArgs(data)
	if len(raw) != 32 {
		return rewardCoin, false
	}

	copy(rewardCoin[:], raw)
	return rewardCoin, true
}

// decodeRegisterValidatorArgs parses the quic address, the optional BLS key, and
// the optional reward-coin bytes from register_validator args. Each trailing
// field is an independent Borsh Vec<u8> (u32 length prefix + bytes); a missing
// field yields a nil slice so older two-field args decode unchanged.
func decodeRegisterValidatorArgs(data []byte) (quicAddr string, blsPubkey, rewardCoin []byte) {
	if len(data) < 4 {
		return "", nil, nil
	}

	quicLen := binary.LittleEndian.Uint32(data[0:4])
	if len(data) < int(4+quicLen) {
		return "", nil, nil
	}
	quicAddr = string(data[4 : 4+quicLen])

	offset := 4 + quicLen
	blsPubkey, offset, ok := readBorshVec(data, offset)
	if !ok {
		return quicAddr, nil, nil
	}

	rewardCoin, _, _ = readBorshVec(data, offset)
	return quicAddr, blsPubkey, rewardCoin
}

// readBorshVec reads a Borsh Vec<u8> (u32 little-endian length + bytes) at offset.
// It returns the bytes, the offset past the field, and ok=false when the field is
// absent or truncated.
func readBorshVec(data []byte, offset uint32) (value []byte, next uint32, ok bool) {
	if uint32(len(data)) < offset+4 {
		return nil, offset, false
	}

	length := binary.LittleEndian.Uint32(data[offset : offset+4])
	if uint32(len(data)) < offset+4+length {
		return nil, offset, false
	}

	value = make([]byte, length)
	copy(value, data[offset+4:offset+4+length])
	return value, offset + 4 + length, true
}
