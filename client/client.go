package client

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
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

// Split splits a coin, sending `amount` to `recipient`.
// Returns the new coinID created for the recipient.
func (w *Wallet) Split(c *Client, coinID [32]byte, amount uint64, recipient [32]byte) ([32]byte, error) {
	coin := w.coins[coinID]
	if coin == nil {
		return [32]byte{}, fmt.Errorf("coin not tracked: %x", coinID[:8])
	}

	obj, err := c.GetObject(coinID)
	if err != nil {
		return [32]byte{}, fmt.Errorf("get coin object:\n%w", err)
	}

	args := encodeSplitArgs(amount, recipient)
	mutableRefs := buildMutableRef(coinID, coin.Version)

	txBytes, txHash := buildSignedAttestedTx(w.privKey, c.systemPod, "split", args, true, mutableRefs, obj)
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

	obj, err := c.GetObject(coinID)
	if err != nil {
		return fmt.Errorf("get coin object:\n%w", err)
	}

	args := encodeTransferArgs(recipient)
	mutableRefs := buildMutableRef(coinID, coin.Version)

	txBytes, _ := buildSignedAttestedTx(w.privKey, c.systemPod, "transfer", args, false, mutableRefs, obj)

	if err := submitTx(c.nodeAddr, txBytes); err != nil {
		return fmt.Errorf("submit transfer tx:\n%w", err)
	}

	return nil
}

// buildMutableRef creates a 40-byte mutable object reference.
// Format: 32-byte objectID + 8-byte version (little-endian).
func buildMutableRef(id [32]byte, version uint64) []byte {
	ref := make([]byte, 40)
	copy(ref[:32], id[:])
	binary.LittleEndian.PutUint64(ref[32:], version)

	return ref
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

// computeNewObjectID computes the deterministic ID for the first created object.
// objectID = blake3(txHash || 0_u32_LE).
func computeNewObjectID(txHash [32]byte) [32]byte {
	var buf [36]byte
	copy(buf[:32], txHash[:])
	binary.LittleEndian.PutUint32(buf[32:], 0)

	return blake3.Sum256(buf[:])
}

// buildSignedAttestedTx builds a signed AttestedTransaction with embedded objects.
// Returns the serialized bytes and the transaction hash.
func buildSignedAttestedTx(
	privKey ed25519.PrivateKey,
	pod [32]byte,
	funcName string,
	args []byte,
	createsObjects bool,
	mutableRefs []byte,
	obj *ObjectInfo,
) ([]byte, [32]byte) {
	pubKey := privKey.Public().(ed25519.PublicKey)

	unsignedBytes := genesis.BuildUnsignedTxBytesWithMutables(
		pubKey, pod, funcName, args, createsObjects, mutableRefs,
	)

	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(privKey, hash[:])

	atxBytes := assembleAttestedTx(pubKey, pod, funcName, args, createsObjects, mutableRefs, hash, sig, obj)

	return atxBytes, hash
}

// assembleAttestedTx builds the final AttestedTransaction FlatBuffers.
func assembleAttestedTx(
	sender []byte,
	pod [32]byte,
	funcName string,
	args []byte,
	createsObjects bool,
	mutableRefs []byte,
	hash [32]byte,
	sig []byte,
	obj *ObjectInfo,
) []byte {
	builder := flatbuffers.NewBuilder(2048)

	// Singletons (replication=0) are not included in the ATX body.
	// The node already has them locally and resolves them from state.
	var objectsVec flatbuffers.UOffsetT
	if obj.Replication == 0 {
		types.AttestedTransactionStartObjectsVector(builder, 0)
		objectsVec = builder.EndVector(0)
	} else {
		objOffset := buildObjectFB(builder, obj)
		types.AttestedTransactionStartObjectsVector(builder, 1)
		builder.PrependUOffsetT(objOffset)
		objectsVec = builder.EndVector(1)
	}

	// Empty proofs vector (singletons don't need proofs)
	types.AttestedTransactionStartProofsVector(builder, 0)
	proofsVec := builder.EndVector(0)

	// Build Transaction table
	txOffset := genesis.BuildTxTableWithMutables(
		builder, sender, pod, funcName, args, createsObjects, hash, sig, mutableRefs,
	)

	// Build AttestedTransaction
	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOffset)
	types.AttestedTransactionAddObjects(builder, objectsVec)
	types.AttestedTransactionAddProofs(builder, proofsVec)
	atxOffset := types.AttestedTransactionEnd(builder)

	builder.Finish(atxOffset)

	return builder.FinishedBytes()
}

// buildObjectFB builds an Object FlatBuffers table from ObjectInfo.
func buildObjectFB(builder *flatbuffers.Builder, info *ObjectInfo) flatbuffers.UOffsetT {
	idVec := builder.CreateByteVector(info.ID[:])
	ownerVec := builder.CreateByteVector(info.Owner[:])
	contentVec := builder.CreateByteVector(info.Content)

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, info.Version)
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, info.Replication)
	types.ObjectAddContent(builder, contentVec)

	return types.ObjectEnd(builder)
}

// submitTx sends transaction bytes to a node via POST /tx.
func submitTx(nodeAddr string, txBytes []byte) error {
	resp, err := http.Post(
		"http://"+nodeAddr+"/tx",
		"application/octet-stream",
		bytes.NewReader(txBytes),
	)
	if err != nil {
		return fmt.Errorf("post tx:\n%w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("tx rejected: status %d", resp.StatusCode)
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

// httpGet performs a GET request and decodes the JSON response.
func httpGet(url string, result any) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("GET %s:\n%w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(result)
}

// httpPostJSON performs a POST request with JSON body and decodes the JSON response.
func httpPostJSON(url string, body any, result any) error {
	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body:\n%w", err)
	}

	resp, err := http.Post(url, "application/json", bytes.NewReader(jsonBytes))
	if err != nil {
		return fmt.Errorf("POST %s:\n%w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("POST %s: status %d", url, resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(result)
}
