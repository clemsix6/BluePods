package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// Client connects to a BluePods node via HTTP.
type Client struct {
	nodeAddr  string   // nodeAddr is the HTTP address (e.g. "127.0.0.1:8080")
	systemPod [32]byte // systemPod is the system pod ID
}

// Wallet holds a keypair and tracks owned coins.
type Wallet struct {
	privKey ed25519.PrivateKey     // privKey is the Ed25519 private key
	pubKey  ed25519.PublicKey      // pubKey is the Ed25519 public key
	coins   map[[32]byte]*CoinInfo // coins tracks owned coins by ID
}

// CoinInfo holds metadata about a coin.
type CoinInfo struct {
	ID      [32]byte // ID is the unique object identifier
	Version uint64   // Version is the current object version
	Balance uint64   // Balance is the coin amount
}

// ObjectInfo holds parsed object data from the API.
type ObjectInfo struct {
	ID          [32]byte // ID is the object identifier
	Version     uint64   // Version is the current version
	Owner       [32]byte // Owner is the owner's public key
	Replication uint16   // Replication is the replication factor
	Content     []byte   // Content is the raw content bytes
}

// ValidatorInfo holds validator network information.
type ValidatorInfo struct {
	Pubkey [32]byte // Pubkey is the validator's public key
	HTTP   string   // HTTP is the HTTP API endpoint
}

// NewClient creates a client connected to a node.
// It fetches the systemPod ID from the node's /status endpoint.
func NewClient(nodeAddr string) (*Client, error) {
	var status struct {
		SystemPod string `json:"systemPod"`
	}

	if err := httpGet("http://"+nodeAddr+"/status", &status); err != nil {
		return nil, fmt.Errorf("get status:\n%w", err)
	}

	podBytes, err := hex.DecodeString(status.SystemPod)
	if err != nil || len(podBytes) != 32 {
		return nil, fmt.Errorf("invalid systemPod: %q", status.SystemPod)
	}

	c := &Client{nodeAddr: nodeAddr}
	copy(c.systemPod[:], podBytes)

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
	}
}

// NewWalletFromKey creates a wallet from an existing Ed25519 private key.
// Used for integration testing where a node's private key is loaded from file.
func NewWalletFromKey(privKey ed25519.PrivateKey) *Wallet {
	return &Wallet{
		privKey: privKey,
		pubKey:  privKey.Public().(ed25519.PublicKey),
		coins:   make(map[[32]byte]*CoinInfo),
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

// Faucet requests tokens from the faucet endpoint.
// Returns the predicted coinID of the minted coin.
func (c *Client) Faucet(pubkey [32]byte, amount uint64) ([32]byte, error) {
	body := map[string]any{
		"pubkey": hex.EncodeToString(pubkey[:]),
		"amount": amount,
	}

	var resp struct {
		CoinID string `json:"coinID"`
	}

	if err := httpPostJSON("http://"+c.nodeAddr+"/faucet", body, &resp); err != nil {
		return [32]byte{}, fmt.Errorf("faucet:\n%w", err)
	}

	return decodeHexID(resp.CoinID)
}

// Validators returns the list of active validators.
func (c *Client) Validators() ([]ValidatorInfo, error) {
	var resp []struct {
		Pubkey string `json:"pubkey"`
		HTTP   string `json:"http"`
	}

	if err := httpGet("http://"+c.nodeAddr+"/validators", &resp); err != nil {
		return nil, fmt.Errorf("get validators:\n%w", err)
	}

	return parseValidators(resp)
}

// parseValidators converts raw validator responses to ValidatorInfo.
func parseValidators(raw []struct {
	Pubkey string `json:"pubkey"`
	HTTP   string `json:"http"`
}) ([]ValidatorInfo, error) {
	result := make([]ValidatorInfo, len(raw))

	for i, v := range raw {
		pkBytes, err := hex.DecodeString(v.Pubkey)
		if err != nil || len(pkBytes) != 32 {
			return nil, fmt.Errorf("invalid validator pubkey: %q", v.Pubkey)
		}

		copy(result[i].Pubkey[:], pkBytes)
		result[i].HTTP = v.HTTP
	}

	return result, nil
}

// GetObject retrieves an object by ID from the node.
func (c *Client) GetObject(id [32]byte) (*ObjectInfo, error) {
	var resp struct {
		ID          string `json:"id"`
		Version     uint64 `json:"version"`
		Owner       string `json:"owner"`
		Replication uint16 `json:"replication"`
		Content     string `json:"content"`
	}

	url := "http://" + c.nodeAddr + "/object/" + hex.EncodeToString(id[:])
	if err := httpGet(url, &resp); err != nil {
		return nil, fmt.Errorf("get object:\n%w", err)
	}

	return parseObjectResp(resp.ID, resp.Owner, resp.Content, resp.Version, resp.Replication)
}

// parseObjectResp parses hex fields from the object API response.
func parseObjectResp(idHex, ownerHex, contentHex string, version uint64, replication uint16) (*ObjectInfo, error) {
	info := &ObjectInfo{Version: version, Replication: replication}

	idBytes, err := hex.DecodeString(idHex)
	if err != nil || len(idBytes) != 32 {
		return nil, fmt.Errorf("invalid object ID: %q", idHex)
	}
	copy(info.ID[:], idBytes)

	ownerBytes, err := hex.DecodeString(ownerHex)
	if err != nil || len(ownerBytes) != 32 {
		return nil, fmt.Errorf("invalid owner: %q", ownerHex)
	}
	copy(info.Owner[:], ownerBytes)

	info.Content, err = hex.DecodeString(contentHex)
	if err != nil {
		return nil, fmt.Errorf("invalid content hex:\n%w", err)
	}

	return info, nil
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

// decodeHexID decodes a 64-char hex string to a [32]byte.
func decodeHexID(hexStr string) ([32]byte, error) {
	b, err := hex.DecodeString(hexStr)
	if err != nil || len(b) != 32 {
		return [32]byte{}, fmt.Errorf("invalid hex ID: %q", hexStr)
	}

	var id [32]byte
	copy(id[:], b)

	return id, nil
}
