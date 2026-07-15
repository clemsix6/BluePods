package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"

	"BluePods/internal/network"
	"BluePods/internal/types"
	"BluePods/pkg/daemon"
)

// Client connects to a BluePods node over QUIC. It submits singleton-only
// transactions raw and routes transactions that touch replicated objects through
// the client daemon (rendezvous, attestation collection, ATX assembly).
type Client struct {
	nodeAddr  string         // nodeAddr is the node's QUIC address (e.g. "127.0.0.1:9000")
	systemPod [32]byte       // systemPod is the system pod ID
	transport *QUICTransport // transport is the SDK's QUIC request transport
	daemon    *daemon.Daemon // daemon collects attestations for replicated objects
}

// Wallet holds a keypair and tracks owned coins.
type Wallet struct {
	privKey ed25519.PrivateKey     // privKey is the Ed25519 private key
	pubKey  ed25519.PublicKey      // pubKey is the Ed25519 public key
	coins   map[[32]byte]*CoinInfo // coins tracks owned coins by ID
	objects map[[32]byte]bool      // objects tracks IDs of created objects
}

// CoinInfo holds metadata about a coin.
type CoinInfo struct {
	ID      [32]byte // ID is the unique object identifier
	Version uint64   // Version is the current object version
	Balance uint64   // Balance is the coin amount
}

// ObjectInfo holds parsed object data.
type ObjectInfo struct {
	ID          [32]byte // ID is the object identifier
	Version     uint64   // Version is the current version
	Owner       [32]byte // Owner is the owner's public key
	Replication uint16   // Replication is the replication factor
	Content     []byte   // Content is the raw content bytes
}

// ValidatorInfo holds validator network information.
type ValidatorInfo struct {
	Pubkey   [32]byte // Pubkey is the validator's public key
	QUICAddr string   // QUICAddr is the QUIC endpoint
}

// NewClient creates a client connected to a node over QUIC. It synchronizes the
// validator set and epoch through the daemon (MsgGetValidators / MsgStatus). The
// system pod ID is supplied by the caller, as it is not network-discoverable over
// the client QUIC surface.
func NewClient(nodeAddr string, systemPod [32]byte) (*Client, error) {
	d, err := daemon.New([]string{nodeAddr})
	if err != nil {
		return nil, fmt.Errorf("init daemon:\n%w", err)
	}

	// Confirm the node is reachable and synced for status.
	if _, err := NewQUICTransport(nodeAddr).Status(); err != nil {
		return nil, fmt.Errorf("get status:\n%w", err)
	}

	c := &Client{
		nodeAddr:  nodeAddr,
		systemPod: systemPod,
		transport: NewQUICTransport(nodeAddr),
		daemon:    d,
	}

	return c, nil
}

// SystemPod returns the system pod ID.
func (c *Client) SystemPod() [32]byte {
	return c.systemPod
}

// NewWallet creates a new wallet with a random Ed25519 keypair.
func NewWallet() *Wallet {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	return &Wallet{
		privKey: priv,
		pubKey:  pub,
		coins:   make(map[[32]byte]*CoinInfo),
		objects: make(map[[32]byte]bool),
	}
}

// NewWalletFromKey creates a wallet from an existing Ed25519 private key.
// Used for integration testing where a node's private key is loaded from file.
func NewWalletFromKey(privKey ed25519.PrivateKey) *Wallet {
	return &Wallet{
		privKey: privKey,
		pubKey:  privKey.Public().(ed25519.PublicKey),
		coins:   make(map[[32]byte]*CoinInfo),
		objects: make(map[[32]byte]bool),
	}
}

// Pubkey returns the wallet's public key as a 32-byte array.
func (w *Wallet) Pubkey() [32]byte {
	var pk [32]byte
	copy(pk[:], w.pubKey)
	return pk
}

// GetCoin returns coin info by ID, or nil if not tracked.
func (w *Wallet) GetCoin(id [32]byte) *CoinInfo {
	return w.coins[id]
}

// Faucet requests tokens from the node's faucet over QUIC.
// Returns the minted coinID and the transaction hash.
func (c *Client) Faucet(pubkey [32]byte, amount uint64) ([32]byte, [32]byte, error) {
	coinID, txHash, err := c.transport.Faucet(pubkey, amount)
	if err != nil {
		return [32]byte{}, [32]byte{}, fmt.Errorf("faucet:\n%w", err)
	}

	return coinID, txHash, nil
}

// Validators returns the list of active validators from the daemon's synced set.
func (c *Client) Validators() ([]ValidatorInfo, error) {
	vs := c.daemon.Validators()
	if vs == nil {
		return nil, fmt.Errorf("validator set not synced")
	}

	all := vs.All()
	result := make([]ValidatorInfo, len(all))

	for i, v := range all {
		result[i].Pubkey = v.Pubkey
		result[i].QUICAddr = v.QUICAddr
	}

	return result, nil
}

// GetObject retrieves an object by ID from the node over QUIC.
func (c *Client) GetObject(id [32]byte) (*ObjectInfo, error) {
	data, err := c.transport.GetObject(id)
	if err != nil {
		return nil, fmt.Errorf("get object:\n%w", err)
	}

	if data == nil {
		return nil, fmt.Errorf("object not found: %x", id[:8])
	}

	return parseObject(data), nil
}

// GetObjectLocal retrieves an object only if the node holds it locally, without
// routing to a remote holder. It returns nil (no error) when the node is not a
// holder, which a caller uses to probe holdership.
func (c *Client) GetObjectLocal(id [32]byte) (*ObjectInfo, error) {
	data, err := c.transport.GetObjectLocal(id)
	if err != nil {
		return nil, fmt.Errorf("get object local:\n%w", err)
	}

	if data == nil {
		return nil, nil
	}

	return parseObject(data), nil
}

// Status returns the node's consensus status and operational counters over QUIC.
func (c *Client) Status() (*network.StatusResponse, error) {
	return c.transport.Status()
}

// GetTxStatus returns a transaction's status (state and failure reason) over QUIC.
func (c *Client) GetTxStatus(hash [32]byte) (*network.GetTxStatusResponse, error) {
	return c.transport.GetTxStatus(hash)
}

// Fingerprint returns the node's convergence fingerprint (last committed
// round, checksum, and supply terms). It requires the node to have been
// started with --test-hooks; otherwise it errors with the node's refusal.
func (c *Client) Fingerprint() (*network.FingerprintResponse, error) {
	return c.transport.Fingerprint()
}

// SetPartition replaces the node's blocklist with blocked, dropping mesh
// traffic to and from every one of them until cleared. It requires the node
// to have been started with --test-hooks; otherwise it errors with the
// node's refusal.
func (c *Client) SetPartition(blocked [][32]byte) error {
	return c.transport.TestControl(&network.TestControlRequest{
		Op:      network.TestControlOpSetPartition,
		Pubkeys: blocked,
	})
}

// ClearPartition empties the node's blocklist, restoring normal traffic to
// and from every peer. It requires the node to have been started with
// --test-hooks; otherwise it errors with the node's refusal.
func (c *Client) ClearPartition() error {
	return c.transport.TestControl(&network.TestControlRequest{Op: network.TestControlOpClearPartition})
}

// ParseObject parses an Object FlatBuffer into an ObjectInfo. It is exported for
// callers (such as the integration tests) that fetch raw object bytes over the
// QUIC transport and need to decode them.
func ParseObject(data []byte) *ObjectInfo {
	return parseObject(data)
}

// parseObject parses an Object FlatBuffer into an ObjectInfo.
func parseObject(data []byte) *ObjectInfo {
	obj := types.GetRootAsObject(data, 0)

	info := &ObjectInfo{
		Version:     obj.Version(),
		Replication: obj.Replication(),
	}
	copy(info.ID[:], obj.IdBytes())
	copy(info.Owner[:], obj.OwnerBytes())

	info.Content = make([]byte, len(obj.ContentBytes()))
	copy(info.Content, obj.ContentBytes())

	return info
}

// submit routes a signed raw transaction through the daemon. A singleton-only
// transaction is submitted raw and wrapped by the validator; a transaction that
// touches replicated objects has its attestations collected and is submitted as
// an ATX.
func (c *Client) submit(rawTx []byte) error {
	if _, err := c.daemon.SubmitTransaction(context.Background(), rawTx); err != nil {
		return fmt.Errorf("submit transaction:\n%w", err)
	}

	return nil
}

// RefreshCoin updates a coin's version and balance from the network.
// Creates the coin entry if it doesn't exist yet.
func (w *Wallet) RefreshCoin(c *Client, coinID [32]byte) error {
	obj, err := c.GetObject(coinID)
	if err != nil {
		return fmt.Errorf("refresh coin:\n%w", err)
	}

	if len(obj.Content) < 8 {
		return fmt.Errorf("coin content too short: %d bytes", len(obj.Content))
	}

	balance := binary.LittleEndian.Uint64(obj.Content[:8])

	w.coins[coinID] = &CoinInfo{
		ID:      coinID,
		Version: obj.Version,
		Balance: balance,
	}

	return nil
}
