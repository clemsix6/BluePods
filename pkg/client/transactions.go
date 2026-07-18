package client

import (
	"crypto/ed25519"
	"encoding/binary"
	"fmt"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
)

const (
	// minClientGas mirrors the fee system's minimum gas (consensus FeeParams.MinGas).
	// A transaction below it is rejected at commit, so the client never goes lower.
	minClientGas uint64 = 100

	// clientMaxGas is the default gas budget the SDK attaches to every value-bearing
	// transaction. It sits above minClientGas and comfortably covers the compute,
	// transit, and storage components of a single coin or object operation.
	clientMaxGas uint64 = 1000
)

const (
	// reparentOpKind mirrors internal/consensus's reparentOp: DeclaredOp.kind=0
	// rebinds an object's parent edge. A transfer is a reparent to a KeyRoot.
	reparentOpKind byte = 0

	// deleteOpKind mirrors internal/consensus's deleteOp: DeclaredOp.kind=1
	// destroys an object and settles its storage deposit.
	deleteOpKind byte = 1

	// keyRootKind mirrors internal/consensus's keyRootKind: the reparent
	// target is an Ed25519 public key (that IS a transfer).
	keyRootKind byte = 0

	// objectParentKind mirrors internal/consensus's objectParentKind: the
	// reparent target is another tracked object's ID.
	objectParentKind byte = 1
)

// Split splits a coin, sending `amount` to `recipient`. The operated coin is a
// singleton owned by the sender, so it doubles as the transaction's gas coin.
// Returns the new coinID created for the recipient and the transaction hash.
func (w *Wallet) Split(c *Client, coinID [32]byte, amount uint64, recipient [32]byte) ([32]byte, [32]byte, error) {
	coin := w.coins[coinID]
	if coin == nil {
		return [32]byte{}, [32]byte{}, fmt.Errorf("coin not tracked: %x", coinID[:8])
	}

	args := encodeSplitArgs(amount, recipient)
	txBytes, txHash := w.buildCoinTx(c.systemPod, "split", args, []uint16{0}, coinID, coin.Version)
	newCoinID := computeNewObjectID(txHash)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, [32]byte{}, fmt.Errorf("submit split tx:\n%w", err)
	}

	return newCoinID, txHash, nil
}

// Transfer transfers a coin to a new owner. A coin is a singleton object, so a
// transfer is the protocol's declared reparent operation (kind 0, target_kind
// KeyRoot) moving the coin under the recipient's key — no pod call runs. The
// operated coin pays its own gas. Returns the transaction hash.
func (w *Wallet) Transfer(c *Client, coinID [32]byte, recipient [32]byte) ([32]byte, error) {
	coin := w.coins[coinID]
	if coin == nil {
		return [32]byte{}, fmt.Errorf("coin not tracked: %x", coinID[:8])
	}

	mutableRefs := buildMutableRef(coinID, coin.Version)
	op := reparentOpFor(coinID, keyRootKind, recipient[:])

	txBytes, txHash := w.buildOpsTx(mutableRefs, coinID, op)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, fmt.Errorf("submit transfer tx:\n%w", err)
	}

	return txHash, nil
}

// CreateObject creates a new replicated object with configurable replication.
// An object operation creates no spendable coin, so the caller supplies an owned
// singleton coin (gasCoinID) to pay the transaction's gas.
// Returns the predicted object ID and the transaction hash.
func (w *Wallet) CreateObject(c *Client, replication uint16, metadata []byte, gasCoinID [32]byte) ([32]byte, [32]byte, error) {
	args := encodeCreateObjectArgs(w.Pubkey(), replication, metadata)

	txBytes, txHash := w.buildObjectTx(c.systemPod, "create_object", args, []uint16{replication}, nil, gasCoinID)
	objectID := computeNewObjectID(txHash)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, [32]byte{}, fmt.Errorf("submit create_object tx:\n%w", err)
	}

	return objectID, txHash, nil
}

// TransferObject transfers an object to a new owner via the protocol's declared
// reparent operation (kind 0, target_kind KeyRoot) — a transfer IS a reparent
// to a KeyRoot, so no pod call runs. The caller supplies an owned singleton
// coin (gasCoinID) to pay gas, since the object itself is not a coin.
// Returns the transaction hash.
func (w *Wallet) TransferObject(c *Client, objectID [32]byte, recipient [32]byte, gasCoinID [32]byte) ([32]byte, error) {
	obj, err := c.GetObject(objectID)
	if err != nil {
		return [32]byte{}, fmt.Errorf("get object:\n%w", err)
	}

	mutableRefs := buildMutableRef(objectID, obj.Version)
	op := reparentOpFor(objectID, keyRootKind, recipient[:])

	txBytes, txHash := w.buildOpsTx(mutableRefs, gasCoinID, op)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, fmt.Errorf("submit transfer_object tx:\n%w", err)
	}

	return txHash, nil
}

// Reparent moves an object under a new ObjectParent via the protocol's declared
// reparent operation (kind 0, target_kind ObjectParent), rebinding its cascade
// control edge without a pod call. The sender must control both the object and
// the new parent, and the move must not close a cycle (enforced at commit). The
// caller supplies an owned singleton coin (gasCoinID) to pay gas, since the
// object itself is not a coin. Returns the transaction hash.
func (w *Wallet) Reparent(c *Client, objectID [32]byte, newParent [32]byte, gasCoinID [32]byte) ([32]byte, error) {
	obj, err := c.GetObject(objectID)
	if err != nil {
		return [32]byte{}, fmt.Errorf("get object:\n%w", err)
	}

	mutableRefs := buildMutableRef(objectID, obj.Version)
	op := reparentOpFor(objectID, objectParentKind, newParent[:])

	txBytes, txHash := w.buildOpsTx(mutableRefs, gasCoinID, op)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, fmt.Errorf("submit reparent tx:\n%w", err)
	}

	return txHash, nil
}

// DeleteObject destroys an object via the protocol's declared delete operation
// (kind 1); the object must have no remaining children. The caller supplies an
// owned singleton coin (gasCoinID) to pay gas — at commit, the protocol refunds
// the fee system's storage-refund share (currently 95%) of the object's
// released deposit to that SAME gas coin, and burns the remainder. Returns the
// transaction hash.
func (w *Wallet) DeleteObject(c *Client, objectID [32]byte, gasCoinID [32]byte) ([32]byte, error) {
	obj, err := c.GetObject(objectID)
	if err != nil {
		return [32]byte{}, fmt.Errorf("get object:\n%w", err)
	}

	mutableRefs := buildMutableRef(objectID, obj.Version)
	op := genesis.DeclaredOp{Kind: deleteOpKind, ObjectID: objectID[:]}

	txBytes, txHash := w.buildOpsTx(mutableRefs, gasCoinID, op)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, fmt.Errorf("submit delete tx:\n%w", err)
	}

	return txHash, nil
}

// SetObject overwrites the content of a replicated object. The object is placed
// in the transaction's mutable refs at its current version, so submission goes
// through the daemon's off-chain aggregation path (holder attestation collection).
// Ownership is enforced by the protocol's mutable-ref owner check. The caller
// supplies an owned singleton coin (gasCoinID) to pay gas.
// Returns the transaction hash.
func (w *Wallet) SetObject(c *Client, objectID [32]byte, content []byte, gasCoinID [32]byte) ([32]byte, error) {
	obj, err := c.GetObject(objectID)
	if err != nil {
		return [32]byte{}, fmt.Errorf("get object:\n%w", err)
	}

	args := encodeSetObjectArgs(objectID, content)
	mutableRefs := buildMutableRef(objectID, obj.Version)

	txBytes, txHash := w.buildObjectTx(c.systemPod, "set_object", args, nil, mutableRefs, gasCoinID)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, fmt.Errorf("submit set_object tx:\n%w", err)
	}

	return txHash, nil
}

// buildCoinTx builds a signed coin operation that pays its own gas from the
// operated coin. The coin is referenced as a mutable ref (at coinVersion) and as
// the gas coin, so the same singleton funds the fee. Returns the tx bytes and hash.
func (w *Wallet) buildCoinTx(pod [32]byte, funcName string, args []byte, createdReps []uint16, coinID [32]byte, coinVersion uint64) ([]byte, [32]byte) {
	mutableRefs := buildMutableRef(coinID, coinVersion)

	return buildSignedGasTx(w.privKey, pod, funcName, args, createdReps, mutableRefs, nil, coinID)
}

// buildObjectTx builds a signed object operation that pays gas from a separately
// owned singleton coin supplied by the caller. The object refs (if any) are
// passed through unchanged. Returns the tx bytes and hash.
func (w *Wallet) buildObjectTx(pod [32]byte, funcName string, args []byte, createdReps []uint16, mutableRefs []genesis.ObjectRefData, gasCoinID [32]byte) ([]byte, [32]byte) {
	return buildSignedGasTx(w.privKey, pod, funcName, args, createdReps, mutableRefs, nil, gasCoinID)
}

// buildOpsTx builds a signed transaction carrying one or more protocol-declared
// operations (reparent, delete) in place of a pod call. Gas is paid from
// gasCoinID, following the same convention as buildCoinTx/buildObjectTx.
// Returns the tx bytes and hash.
func (w *Wallet) buildOpsTx(mutableRefs []genesis.ObjectRefData, gasCoinID [32]byte, ops ...genesis.DeclaredOp) ([]byte, [32]byte) {
	return buildSignedOpsTx(w.privKey, mutableRefs, gasCoinID, ops)
}

// DeregisterValidator sends a deregister_validator transaction.
// The validator is removed from the active set at the next epoch boundary.
func (w *Wallet) DeregisterValidator(c *Client) error {
	txBytes, _ := buildSignedTx(w.privKey, c.systemPod, "deregister_validator", nil, nil, nil, nil)

	if err := c.submit(txBytes); err != nil {
		return fmt.Errorf("submit deregister_validator tx:\n%w", err)
	}

	return nil
}

// Bond stakes amount from a singleton coin the wallet owns behind this
// wallet's validator identity. The coin is both the staked coin (first
// mutable_ref) and the gas coin. Returns the tx hash.
func (w *Wallet) Bond(c *Client, coinID [32]byte, coinVersion uint64, amount uint64) ([32]byte, error) {
	txBytes, txHash := w.buildCoinTx(c.systemPod, "bond", encodeStakeArgs(amount), nil, coinID, coinVersion)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, fmt.Errorf("submit bond tx:\n%w", err)
	}

	return txHash, nil
}

// RegisterValidator (re-)registers this wallet's key as a validator, optionally
// designating a reward coin (zero value = no designation). Raw, gas-free tx.
func (w *Wallet) RegisterValidator(c *Client, quicAddr string, blsPubkey []byte, rewardCoin [32]byte) error {
	txBytes := w.buildRegisterValidatorTx(c.systemPod, quicAddr, blsPubkey, rewardCoin)

	if err := c.submit(txBytes); err != nil {
		return fmt.Errorf("submit register_validator tx:\n%w", err)
	}

	return nil
}

// buildRegisterValidatorTx builds this wallet's signed register_validator
// transaction, optionally designating a reward coin.
func (w *Wallet) buildRegisterValidatorTx(pod [32]byte, quicAddr string, blsPubkey []byte, rewardCoin [32]byte) []byte {
	return genesis.BuildRegisterValidatorRawTx(w.privKey, pod, quicAddr, blsPubkey, rewardCoin)
}

// buildMutableRef creates a single-element ObjectRefData slice for a mutable object.
func buildMutableRef(id [32]byte, version uint64) []genesis.ObjectRefData {
	return []genesis.ObjectRefData{{ID: id, Version: version}}
}

// reparentOpFor builds a kind-0 declared operation moving objectID under
// (targetKind, target) — an Ed25519 KeyRoot (that IS a transfer) or another
// tracked object's ID (ObjectParent).
func reparentOpFor(objectID [32]byte, targetKind byte, target []byte) genesis.DeclaredOp {
	return genesis.DeclaredOp{
		Kind:       reparentOpKind,
		ObjectID:   objectID[:],
		TargetKind: targetKind,
		Target:     target,
	}
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

// encodeSetObjectArgs encodes set_object arguments in Borsh format.
// Format: [u8; 32] object_id + u32 content_len (LE) + content bytes.
func encodeSetObjectArgs(objectID [32]byte, content []byte) []byte {
	buf := make([]byte, 32+4+len(content))
	copy(buf[:32], objectID[:])
	binary.LittleEndian.PutUint32(buf[32:], uint32(len(content)))
	copy(buf[36:], content)

	return buf
}

// encodeStakeArgs encodes bond/unbond function arguments in Borsh format.
// Format: u64 amount (LE).
func encodeStakeArgs(amount uint64) []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, amount)

	return buf
}

// encodeCreateObjectArgs encodes create_object arguments in Borsh format.
// Format: [u8; 32] owner + u16 replication + u32 metadata_len + metadata bytes.
func encodeCreateObjectArgs(owner [32]byte, replication uint16, metadata []byte) []byte {
	buf := make([]byte, 32+2+4+len(metadata))
	copy(buf[:32], owner[:])
	binary.LittleEndian.PutUint16(buf[32:], replication)
	binary.LittleEndian.PutUint32(buf[34:], uint32(len(metadata)))
	copy(buf[38:], metadata)

	return buf
}

// buildSignedTx builds a signed raw Transaction (not ATX).
// The client submits it through the daemon, which aggregates attestations for any
// replicated objects before submission; singleton-only transactions are submitted
// raw and wrapped by the validator.
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

// buildSignedGasTx builds a signed raw Transaction that pays gas from gasCoin.
// It mirrors buildSignedTx but threads the client's default gas budget and the
// gas-coin ID into the canonical body, so the transaction is no longer rejected
// at commit for lacking a funded gas coin.
// Returns the serialized Transaction bytes and the transaction hash.
func buildSignedGasTx(
	privKey ed25519.PrivateKey,
	pod [32]byte,
	funcName string,
	args []byte,
	createdObjectsReplication []uint16,
	mutableRefs []genesis.ObjectRefData,
	readRefs []genesis.ObjectRefData,
	gasCoin [32]byte,
) ([]byte, [32]byte) {
	pubKey := privKey.Public().(ed25519.PublicKey)

	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(
		pubKey, pod, funcName, args, createdObjectsReplication, 0, clientMaxGas, gasCoin[:], mutableRefs, readRefs,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(privKey, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOffset := genesis.BuildTxTableWithRefs(
		builder, pubKey, pod, funcName, args, createdObjectsReplication, 0, clientMaxGas, gasCoin[:], hash, sig, mutableRefs, readRefs,
	)
	builder.Finish(txOffset)

	return builder.FinishedBytes(), hash
}

// buildSignedOpsTx builds a signed raw Transaction carrying declared protocol
// operations (reparent, delete) instead of a pod call. The pod and function
// name stay zero/empty, so commit's txHasPodCall reports false and the
// operations — never WASM — decide the outcome; the two are mutually
// exclusive. Gas is paid from gasCoin, following the same convention as
// buildSignedGasTx. Returns the serialized Transaction bytes and the
// transaction hash.
func buildSignedOpsTx(
	privKey ed25519.PrivateKey,
	mutableRefs []genesis.ObjectRefData,
	gasCoin [32]byte,
	ops []genesis.DeclaredOp,
) ([]byte, [32]byte) {
	pubKey := privKey.Public().(ed25519.PublicKey)
	var zeroPod [32]byte

	unsignedBytes := genesis.BuildUnsignedTxBytesSponsored(
		pubKey, zeroPod, "", nil, nil, 0, clientMaxGas, gasCoin[:], mutableRefs, nil, genesis.Sponsorship{}, nil, ops,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(privKey, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOffset := genesis.BuildTxTableSponsored(
		builder, pubKey, zeroPod, "", nil, nil, 0, clientMaxGas, gasCoin[:], hash, sig, mutableRefs, nil, genesis.Sponsorship{}, nil, nil, ops,
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
