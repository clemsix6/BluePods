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
// Format: u32 len + quic_address bytes + u32 len + bls_pubkey bytes + u32 len +
// bls_pop bytes. bls_pop proves possession of bls_pubkey (see
// attest.ProveKeyPossession); the commit path refuses a registration that claims
// a key without it.
// Note: ed25519 pubkey is taken from tx.sender, not from args.
func encodeRegisterValidatorArgs(quicAddr, blsPubkey, blsPoP []byte) []byte {
	buf := make([]byte, 0, 12+len(quicAddr)+len(blsPubkey)+len(blsPoP))
	lenBuf := make([]byte, 4)

	// Each field is a Borsh Vec<u8>: u32 length prefix + bytes.
	for _, field := range [][]byte{quicAddr, blsPubkey, blsPoP} {
		binary.LittleEndian.PutUint32(lenBuf, uint32(len(field)))
		buf = append(buf, lenBuf...)
		buf = append(buf, field...)
	}

	return buf
}

// EncodeRegisterValidatorArgs encodes register_validator arguments in Borsh
// format, optionally designating a reward coin. A zero rewardCoin omits the
// trailing field entirely, mirroring DecodeRegisterValidatorRewardCoin's
// ok=false absence case.
func EncodeRegisterValidatorArgs(quicAddr, blsPubkey, blsPoP []byte, rewardCoin [32]byte) []byte {
	buf := encodeRegisterValidatorArgs(quicAddr, blsPubkey, blsPoP)
	if rewardCoin == ([32]byte{}) {
		return buf
	}

	lenBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(lenBuf, uint32(len(rewardCoin)))
	buf = append(buf, lenBuf...)
	buf = append(buf, rewardCoin[:]...)

	return buf
}

// DecodeRegisterValidatorArgs decodes register_validator arguments from Borsh
// format. Returns the QUIC address, the claimed BLS public key, and the proof of
// possession behind it. Returns empty/nil values if data is malformed; a field
// absent from the args decodes as nil, and a nil proof is what makes a claimed
// key unverifiable, hence refused at commit.
func DecodeRegisterValidatorArgs(data []byte) (quicAddr string, blsPubkey, blsPoP []byte) {
	quicAddr, blsPubkey, blsPoP, _ = decodeRegisterValidatorArgs(data)
	return quicAddr, blsPubkey, blsPoP
}

// DecodeRegisterValidatorRewardCoin decodes the optional reward-coin designation
// that trails a register_validator's args (a Borsh Vec<u8> after the proof of
// possession). It returns ok=false when no 32-byte reward coin is present, so an
// absent designation is distinguishable from a zero one.
func DecodeRegisterValidatorRewardCoin(data []byte) (rewardCoin [32]byte, ok bool) {
	_, _, _, raw := decodeRegisterValidatorArgs(data)
	if len(raw) != 32 {
		return rewardCoin, false
	}

	copy(rewardCoin[:], raw)
	return rewardCoin, true
}

// decodeRegisterValidatorArgs parses the quic address, the claimed BLS key, its
// proof of possession, and the optional reward-coin bytes from
// register_validator args. Each field after the address is an independent Borsh
// Vec<u8> (u32 length prefix + bytes); a missing field yields a nil slice.
func decodeRegisterValidatorArgs(data []byte) (quicAddr string, blsPubkey, blsPoP, rewardCoin []byte) {
	if len(data) < 4 {
		return "", nil, nil, nil
	}

	quicLen := binary.LittleEndian.Uint32(data[0:4])
	if len(data) < int(4+quicLen) {
		return "", nil, nil, nil
	}
	quicAddr = string(data[4 : 4+quicLen])

	offset := 4 + quicLen
	blsPubkey, offset, ok := readBorshVec(data, offset)
	if !ok {
		return quicAddr, nil, nil, nil
	}

	blsPoP, offset, ok = readBorshVec(data, offset)
	if !ok {
		return quicAddr, blsPubkey, nil, nil
	}

	rewardCoin, _, _ = readBorshVec(data, offset)
	return quicAddr, blsPubkey, blsPoP, rewardCoin
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
