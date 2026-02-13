package client

import (
	"crypto/ed25519"
	"encoding/binary"
	"fmt"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
)

// Split splits a coin, sending `amount` to `recipient`.
// Returns the new coinID created for the recipient.
func (w *Wallet) Split(c *Client, coinID [32]byte, amount uint64, recipient [32]byte) ([32]byte, error) {
	coin := w.coins[coinID]
	if coin == nil {
		return [32]byte{}, fmt.Errorf("coin not tracked: %x", coinID[:8])
	}

	args := encodeSplitArgs(amount, recipient)
	mutableRefs := buildMutableRef(coinID, coin.Version)

	txBytes, txHash := buildSignedTx(w.privKey, c.systemPod, "split", args, []uint16{0}, mutableRefs, nil)
	newCoinID := computeNewObjectID(txHash)

	if err := submitTx(c.nodeAddr, txBytes); err != nil {
		return [32]byte{}, fmt.Errorf("submit split tx:\n%w", err)
	}

	return newCoinID, nil
}

// Transfer transfers a coin to a new owner.
func (w *Wallet) Transfer(c *Client, coinID [32]byte, recipient [32]byte) error {
	coin := w.coins[coinID]
	if coin == nil {
		return fmt.Errorf("coin not tracked: %x", coinID[:8])
	}

	args := encodeTransferArgs(recipient)
	mutableRefs := buildMutableRef(coinID, coin.Version)

	txBytes, _ := buildSignedTx(w.privKey, c.systemPod, "transfer", args, nil, mutableRefs, nil)

	if err := submitTx(c.nodeAddr, txBytes); err != nil {
		return fmt.Errorf("submit transfer tx:\n%w", err)
	}

	return nil
}

// CreateNFT creates a new NFT with configurable replication.
// Returns the predicted NFT object ID.
func (w *Wallet) CreateNFT(c *Client, replication uint16, metadata []byte) ([32]byte, error) {
	args := encodeCreateNftArgs(w.Pubkey(), replication, metadata)

	txBytes, txHash := buildSignedTx(w.privKey, c.systemPod, "create_nft", args, []uint16{replication}, nil, nil)
	nftID := computeNewObjectID(txHash)

	if err := submitTx(c.nodeAddr, txBytes); err != nil {
		return [32]byte{}, fmt.Errorf("submit create_nft tx:\n%w", err)
	}

	return nftID, nil
}

// TransferNFT transfers an NFT to a new owner.
func (w *Wallet) TransferNFT(c *Client, nftID [32]byte, recipient [32]byte) error {
	obj, err := c.GetObject(nftID)
	if err != nil {
		return fmt.Errorf("get nft object:\n%w", err)
	}

	args := encodeTransferArgs(recipient)
	mutableRefs := buildMutableRef(nftID, obj.Version)

	txBytes, _ := buildSignedTx(w.privKey, c.systemPod, "transfer_nft", args, nil, mutableRefs, nil)

	if err := submitTx(c.nodeAddr, txBytes); err != nil {
		return fmt.Errorf("submit transfer_nft tx:\n%w", err)
	}

	return nil
}

// DeregisterValidator sends a deregister_validator transaction.
// The validator is removed from the active set at the next epoch boundary.
func (w *Wallet) DeregisterValidator(c *Client) error {
	txBytes, _ := buildSignedTx(w.privKey, c.systemPod, "deregister_validator", nil, nil, nil, nil)

	if err := submitTx(c.nodeAddr, txBytes); err != nil {
		return fmt.Errorf("submit deregister_validator tx:\n%w", err)
	}

	return nil
}

// buildMutableRef creates a single-element ObjectRefData slice for a mutable object.
func buildMutableRef(id [32]byte, version uint64) []genesis.ObjectRefData {
	return []genesis.ObjectRefData{{ID: id, Version: version}}
}

// encodeSplitArgs encodes split function arguments in Borsh format.
// Format: u64 amount (LE) + [u8; 32] new_owner.
func encodeSplitArgs(amount uint64, newOwner [32]byte) []byte {
	buf := make([]byte, 40)
	binary.LittleEndian.PutUint64(buf[:8], amount)
	copy(buf[8:], newOwner[:])

	return buf
}

// encodeTransferArgs encodes transfer function arguments in Borsh format.
// Format: [u8; 32] new_owner.
func encodeTransferArgs(newOwner [32]byte) []byte {
	buf := make([]byte, 32)
	copy(buf, newOwner[:])

	return buf
}

// encodeCreateNftArgs encodes create_nft arguments in Borsh format.
// Format: [u8; 32] owner + u16 replication + u32 metadata_len + metadata bytes.
func encodeCreateNftArgs(owner [32]byte, replication uint16, metadata []byte) []byte {
	buf := make([]byte, 32+2+4+len(metadata))
	copy(buf[:32], owner[:])
	binary.LittleEndian.PutUint16(buf[32:], replication)
	binary.LittleEndian.PutUint32(buf[34:], uint32(len(metadata)))
	copy(buf[38:], metadata)

	return buf
}

// buildSignedTx builds a signed raw Transaction (not ATX).
// Per spec: the client sends a raw Transaction to the validator, which becomes the aggregator.
// Returns the serialized Transaction bytes and the transaction hash.
func buildSignedTx(
	privKey ed25519.PrivateKey,
	pod [32]byte,
	funcName string,
	args []byte,
	createdObjectsReplication []uint16,
	mutableRefs []genesis.ObjectRefData,
	readRefs []genesis.ObjectRefData,
) ([]byte, [32]byte) {
	pubKey := privKey.Public().(ed25519.PublicKey)

	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(
		pubKey, pod, funcName, args, createdObjectsReplication, 0, 0, nil, mutableRefs, readRefs,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(privKey, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOffset := genesis.BuildTxTableWithRefs(
		builder, pubKey, pod, funcName, args, createdObjectsReplication, 0, 0, nil, hash, sig, mutableRefs, readRefs,
	)
	builder.Finish(txOffset)

	return builder.FinishedBytes(), hash
}

// computeNewObjectID computes the deterministic ID for the first created object.
// objectID = blake3(txHash || 0_u32_LE).
func computeNewObjectID(txHash [32]byte) [32]byte {
	var buf [36]byte
	copy(buf[:32], txHash[:])
	binary.LittleEndian.PutUint32(buf[32:], 0)

	return blake3.Sum256(buf[:])
}
