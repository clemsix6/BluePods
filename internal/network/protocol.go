package network

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	// maxMessageSize is the maximum allowed message size (16 MB).
	maxMessageSize = 16 << 20

	// lengthPrefixSize is the size of the length prefix in bytes.
	lengthPrefixSize = 4
)

// writeMessage writes a length-prefixed message to the writer.
// Format: [4 bytes big-endian length] [payload]
func writeMessage(w io.Writer, data []byte) error {
	if len(data) > maxMessageSize {
		return fmt.Errorf("message too large: %d > %d", len(data), maxMessageSize)
	}

	// Write length prefix
	var lengthBuf [lengthPrefixSize]byte
	binary.BigEndian.PutUint32(lengthBuf[:], uint32(len(data)))

	if _, err := w.Write(lengthBuf[:]); err != nil {
		return fmt.Errorf("write length: %w", err)
	}

	// Write payload
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}

	return nil
}

// readMessage reads a length-prefixed message from the reader.
func readMessage(r io.Reader) ([]byte, error) {
	// Read length prefix
	var lengthBuf [lengthPrefixSize]byte

	if _, err := io.ReadFull(r, lengthBuf[:]); err != nil {
		return nil, fmt.Errorf("read length: %w", err)
	}

	length := binary.BigEndian.Uint32(lengthBuf[:])

	if length > maxMessageSize {
		return nil, fmt.Errorf("message too large: %d > %d", length, maxMessageSize)
	}

	// Read payload
	data := make([]byte, length)

	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("read payload: %w", err)
	}

	return data, nil
}
