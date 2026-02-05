package genesis

import "encoding/binary"

// encodeMintArgs encodes mint arguments in Borsh format.
// Format: u64 amount (little-endian) + [u8; 32] owner
func encodeMintArgs(amount uint64, owner [32]byte) []byte {
	buf := make([]byte, 8+32)
	binary.LittleEndian.PutUint64(buf[0:8], amount)
	copy(buf[8:], owner[:])

	return buf
}

// encodeRegisterValidatorArgs encodes register_validator arguments in Borsh format.
// Format: u32 len + http_address bytes + u32 len + quic_address bytes
// Note: pubkey is taken from tx.sender, not from args.
func encodeRegisterValidatorArgs(httpAddr, quicAddr []byte) []byte {
	buf := make([]byte, 0, 4+len(httpAddr)+4+len(quicAddr))

	// http_address (Vec<u8>: u32 length prefix + bytes)
	lenBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(lenBuf, uint32(len(httpAddr)))
	buf = append(buf, lenBuf...)
	buf = append(buf, httpAddr...)

	// quic_address (Vec<u8>: u32 length prefix + bytes)
	binary.LittleEndian.PutUint32(lenBuf, uint32(len(quicAddr)))
	buf = append(buf, lenBuf...)
	buf = append(buf, quicAddr...)

	return buf
}
