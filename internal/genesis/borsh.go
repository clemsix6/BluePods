package genesis

import "encoding/binary"

// EncodeMintArgs encodes mint arguments in Borsh format.
// Format: u64 amount (little-endian) + [u8; 32] owner
func EncodeMintArgs(amount uint64, owner [32]byte) []byte {
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

// DecodeRegisterValidatorArgs decodes register_validator arguments from Borsh format.
// Returns httpAddr and quicAddr as strings.
// Returns empty strings if data is malformed.
func DecodeRegisterValidatorArgs(data []byte) (httpAddr, quicAddr string) {
	if len(data) < 4 {
		return "", ""
	}

	// Read http_address length
	httpLen := binary.LittleEndian.Uint32(data[0:4])
	if len(data) < int(4+httpLen+4) {
		return "", ""
	}

	httpAddr = string(data[4 : 4+httpLen])

	// Read quic_address length
	offset := 4 + httpLen
	quicLen := binary.LittleEndian.Uint32(data[offset : offset+4])
	if len(data) < int(offset+4+quicLen) {
		return "", ""
	}

	quicAddr = string(data[offset+4 : offset+4+quicLen])

	return httpAddr, quicAddr
}
