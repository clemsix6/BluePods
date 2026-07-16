package network

import (
	"encoding/binary"
	"fmt"
)

// Client message-type tags. Tags 0x01-0x03 are reserved for the attestation
// protocol and the FlatBuffers-based snapshot messages have no tag (they are
// matched heuristically). Client messages all use explicit tags >= 0x04 so the
// request router can classify them before falling back to the snapshot
// heuristic, which keys on RequestId() > 0.
const (
	// MsgTagSubmitTx carries a raw transaction or a full ATX for submission.
	MsgTagSubmitTx = 0x04

	// MsgTagGetObject requests an object by ID.
	MsgTagGetObject = 0x05

	// MsgTagGetObjectResp is the response to a GetObject request.
	MsgTagGetObjectResp = 0x06

	// MsgTagGetValidators requests the current validator set and epoch.
	MsgTagGetValidators = 0x07

	// MsgTagGetValidatorsResp carries the validator set and current epoch.
	MsgTagGetValidatorsResp = 0x08

	// MsgTagStatus requests the node's consensus status.
	MsgTagStatus = 0x09

	// MsgTagStatusResp carries the node's consensus status.
	MsgTagStatusResp = 0x0A

	// MsgTagHealth is a liveness probe.
	MsgTagHealth = 0x0B

	// MsgTagHealthResp is the response to a health probe.
	MsgTagHealthResp = 0x0C

	// MsgTagFaucet requests a faucet mint.
	MsgTagFaucet = 0x0D

	// MsgTagFaucetResp is the response to a faucet request.
	MsgTagFaucetResp = 0x0E

	// MsgTagDomainResolve requests resolution of a domain name to an object ID.
	MsgTagDomainResolve = 0x0F

	// MsgTagDomainResolveResp is the response to a domain-resolution request.
	MsgTagDomainResolveResp = 0x10

	// MsgTagSubmitTxResp is the response to a transaction submission.
	MsgTagSubmitTxResp = 0x11

	// MsgTagGossipTx carries a transaction (raw or ATX) gossiped between mesh peers
	// over the one-way message stream. It is distinct from MsgSubmitTx (a
	// request/response submission) and from a vertex (an untagged FlatBuffer on the
	// same stream): the tag lets a receiver tell a forwarded transaction apart from
	// a vertex so the transaction enters the producer's pending set instead of being
	// misparsed as a vertex and dropped.
	MsgTagGossipTx = 0x12

	// MsgTagGetTxStatus requests the status of a transaction by hash.
	MsgTagGetTxStatus = 0x13

	// MsgTagGetTxStatusResp carries a transaction's status.
	MsgTagGetTxStatusResp = 0x14

	// MsgTagGetVertex requests a single DAG vertex by hash. It is a mesh-tier
	// request the commit loop uses to recover a decided anchor's missing ancestry
	// from peers when gossip and the pending buffer did not deliver it.
	MsgTagGetVertex = 0x15

	// MsgTagGetVertexResp carries the requested vertex bytes, or a not-found flag.
	MsgTagGetVertexResp = 0x16

	// MsgTagStateFingerprint requests the node's convergence fingerprint (test
	// hooks only; see internal/sync.ComputeFingerprint). Refused with a typed
	// error when the node was not started with --test-hooks.
	MsgTagStateFingerprint = 0x17

	// MsgTagStateFingerprintResp carries the fingerprint response.
	MsgTagStateFingerprintResp = 0x18

	// MsgTagTestControl carries a test-only network-control operation (partition
	// set/clear). Refused with a typed error when the node was not started with
	// --test-hooks.
	MsgTagTestControl = 0x19

	// MsgTagTestControlResp is the response to a test-control operation.
	MsgTagTestControlResp = 0x1A

	// minClientTag is the lowest tag value reserved for client messages.
	minClientTag = MsgTagSubmitTx
)

// EncodeGossipTx wraps a transaction body for gossip on the one-way message
// stream. Format: [1B tag] [body].
func EncodeGossipTx(body []byte) []byte {
	buf := make([]byte, 1+len(body))
	buf[0] = MsgTagGossipTx
	copy(buf[1:], body)

	return buf
}

// DecodeGossipTx returns the transaction body of a gossiped-transaction message,
// or ok=false when data does not carry the gossip-tx tag.
func DecodeGossipTx(data []byte) (body []byte, ok bool) {
	if len(data) < 1 || data[0] != MsgTagGossipTx {
		return nil, false
	}

	return data[1:], true
}

// clientRequestTags is the exact set of first-byte tags a node serves as client
// requests. Classification matches this set rather than a broad ">= 0x04" range:
// a snapshot request is an untagged FlatBuffer whose root offset (0x0c) falls in
// that range, so a range test would misroute it. Only request tags appear here;
// response tags are never received as inbound requests.
var clientRequestTags = map[byte]struct{}{
	MsgTagSubmitTx:         {},
	MsgTagGetObject:        {},
	MsgTagGetValidators:    {},
	MsgTagStatus:           {},
	MsgTagHealth:           {},
	MsgTagFaucet:           {},
	MsgTagDomainResolve:    {},
	MsgTagGetTxStatus:      {},
	MsgTagGetVertex:        {},
	MsgTagStateFingerprint: {},
	MsgTagTestControl:      {},
}

// IsClientMessage reports whether data carries a known client request tag. It is
// used to classify a request before the snapshot heuristic. Matching the exact
// request-tag set (not a range) keeps a snapshot request, whose FlatBuffer root
// offset happens to land in the client-tag range, from being misrouted here.
func IsClientMessage(data []byte) bool {
	if len(data) == 0 {
		return false
	}

	_, ok := clientRequestTags[data[0]]
	return ok
}

// MessageTag returns the first-byte tag of a message, or an error if empty.
func MessageTag(data []byte) (byte, error) {
	if len(data) == 0 {
		return 0, fmt.Errorf("empty message")
	}

	return data[0], nil
}

// SubmitTxRequest carries a transaction body to submit. The body is either a
// raw Transaction FlatBuffer or a full AttestedTransaction; the receiver
// distinguishes them by structure.
type SubmitTxRequest struct {
	Body []byte // Body is the raw transaction or ATX FlatBuffer
}

// EncodeSubmitTx encodes a transaction submission request.
// Format: [1B tag] [body].
func EncodeSubmitTx(req *SubmitTxRequest) []byte {
	buf := make([]byte, 1+len(req.Body))
	buf[0] = MsgTagSubmitTx
	copy(buf[1:], req.Body)

	return buf
}

// DecodeSubmitTx decodes a transaction submission request.
func DecodeSubmitTx(data []byte) (*SubmitTxRequest, error) {
	if len(data) < 1 || data[0] != MsgTagSubmitTx {
		return nil, fmt.Errorf("not a submit-tx message")
	}

	body := make([]byte, len(data)-1)
	copy(body, data[1:])

	return &SubmitTxRequest{Body: body}, nil
}

// SubmitTxResponse is the response to a transaction submission.
type SubmitTxResponse struct {
	Hash []byte // Hash is the transaction hash (32 bytes), empty on error
	Err  string // Err is a non-empty error message on failure
}

// EncodeSubmitTxResp encodes a transaction submission response.
// Format: [1B tag] [2B hashLen] [hash] [errString].
func EncodeSubmitTxResp(resp *SubmitTxResponse) []byte {
	buf := make([]byte, 1+2+len(resp.Hash)+len(resp.Err))
	buf[0] = MsgTagSubmitTxResp
	binary.BigEndian.PutUint16(buf[1:3], uint16(len(resp.Hash)))
	copy(buf[3:], resp.Hash)
	copy(buf[3+len(resp.Hash):], resp.Err)

	return buf
}

// DecodeSubmitTxResp decodes a transaction submission response.
func DecodeSubmitTxResp(data []byte) (*SubmitTxResponse, error) {
	if len(data) < 3 || data[0] != MsgTagSubmitTxResp {
		return nil, fmt.Errorf("not a submit-tx response")
	}

	hashLen := int(binary.BigEndian.Uint16(data[1:3]))
	if len(data) < 3+hashLen {
		return nil, fmt.Errorf("submit-tx response truncated")
	}

	resp := &SubmitTxResponse{
		Hash: make([]byte, hashLen),
		Err:  string(data[3+hashLen:]),
	}
	copy(resp.Hash, data[3:3+hashLen])

	return resp, nil
}

// GetObjectRequest requests an object by ID. When LocalOnly is set, the node
// answers only from its local state and never routes to a remote holder, which
// lets a caller probe whether a specific node holds an object.
type GetObjectRequest struct {
	ObjectID  [32]byte // ObjectID is the object to fetch
	LocalOnly bool     // LocalOnly suppresses inter-node routing
}

// EncodeGetObject encodes a GetObject request.
// Format: [1B tag] [32B objectID] [1B localOnly].
func EncodeGetObject(req *GetObjectRequest) []byte {
	buf := make([]byte, 34)
	buf[0] = MsgTagGetObject
	copy(buf[1:33], req.ObjectID[:])

	if req.LocalOnly {
		buf[33] = 1
	}

	return buf
}

// DecodeGetObject decodes a GetObject request. The local-only flag is optional
// so older 33-byte requests still decode (LocalOnly defaults to false).
func DecodeGetObject(data []byte) (*GetObjectRequest, error) {
	if len(data) < 33 || data[0] != MsgTagGetObject {
		return nil, fmt.Errorf("not a get-object message")
	}

	req := &GetObjectRequest{}
	copy(req.ObjectID[:], data[1:33])

	if len(data) >= 34 && data[33] == 1 {
		req.LocalOnly = true
	}

	return req, nil
}

// GetObjectResponse carries an object payload or a not-found flag.
type GetObjectResponse struct {
	Found bool   // Found reports whether the object was located
	Data  []byte // Data is the Object FlatBuffer when Found
}

// EncodeGetObjectResp encodes a GetObject response.
// Format: [1B tag] [1B found] [data].
func EncodeGetObjectResp(resp *GetObjectResponse) []byte {
	buf := make([]byte, 2+len(resp.Data))
	buf[0] = MsgTagGetObjectResp

	if resp.Found {
		buf[1] = 1
	}

	copy(buf[2:], resp.Data)

	return buf
}

// DecodeGetObjectResp decodes a GetObject response.
func DecodeGetObjectResp(data []byte) (*GetObjectResponse, error) {
	if len(data) < 2 || data[0] != MsgTagGetObjectResp {
		return nil, fmt.Errorf("not a get-object response")
	}

	resp := &GetObjectResponse{Found: data[1] == 1}

	if len(data) > 2 {
		resp.Data = make([]byte, len(data)-2)
		copy(resp.Data, data[2:])
	}

	return resp, nil
}

// EncodeGetValidators encodes a validator-set request.
// Format: [1B tag].
func EncodeGetValidators() []byte {
	return []byte{MsgTagGetValidators}
}

// ValidatorEntry describes one validator in a GetValidators response.
type ValidatorEntry struct {
	Pubkey    [32]byte // Pubkey is the validator's Ed25519 public key
	BLSPubkey [48]byte // BLSPubkey is the validator's BLS public key
	QUICAddr  string   // QUICAddr is the validator's QUIC endpoint
}

// GetValidatorsResponse carries the validator set and the current epoch.
type GetValidatorsResponse struct {
	Epoch      uint64           // Epoch is the node's current epoch
	Validators []ValidatorEntry // Validators is the active set
}

// EncodeGetValidatorsResp encodes a validator-set response.
// Format: [1B tag] [8B epoch] [4B count] then per entry:
// [32B pubkey] [48B blsPubkey] [2B addrLen] [addr].
func EncodeGetValidatorsResp(resp *GetValidatorsResponse) []byte {
	size := 1 + 8 + 4
	for i := range resp.Validators {
		size += 32 + 48 + 2 + len(resp.Validators[i].QUICAddr)
	}

	buf := make([]byte, size)
	buf[0] = MsgTagGetValidatorsResp
	binary.BigEndian.PutUint64(buf[1:9], resp.Epoch)
	binary.BigEndian.PutUint32(buf[9:13], uint32(len(resp.Validators)))

	off := 13
	for i := range resp.Validators {
		v := &resp.Validators[i]
		copy(buf[off:off+32], v.Pubkey[:])
		off += 32
		copy(buf[off:off+48], v.BLSPubkey[:])
		off += 48
		binary.BigEndian.PutUint16(buf[off:off+2], uint16(len(v.QUICAddr)))
		off += 2
		copy(buf[off:off+len(v.QUICAddr)], v.QUICAddr)
		off += len(v.QUICAddr)
	}

	return buf
}

// DecodeGetValidatorsResp decodes a validator-set response.
func DecodeGetValidatorsResp(data []byte) (*GetValidatorsResponse, error) {
	if len(data) < 13 || data[0] != MsgTagGetValidatorsResp {
		return nil, fmt.Errorf("not a get-validators response")
	}

	resp := &GetValidatorsResponse{Epoch: binary.BigEndian.Uint64(data[1:9])}
	count := int(binary.BigEndian.Uint32(data[9:13]))
	resp.Validators = make([]ValidatorEntry, 0, count)

	off := 13
	for i := 0; i < count; i++ {
		if len(data) < off+82 {
			return nil, fmt.Errorf("get-validators response truncated")
		}

		var entry ValidatorEntry
		copy(entry.Pubkey[:], data[off:off+32])
		off += 32
		copy(entry.BLSPubkey[:], data[off:off+48])
		off += 48
		addrLen := int(binary.BigEndian.Uint16(data[off : off+2]))
		off += 2

		if len(data) < off+addrLen {
			return nil, fmt.Errorf("get-validators response truncated")
		}

		entry.QUICAddr = string(data[off : off+addrLen])
		off += addrLen

		resp.Validators = append(resp.Validators, entry)
	}

	return resp, nil
}

// EncodeStatus encodes a status request.
// Format: [1B tag].
func EncodeStatus() []byte {
	return []byte{MsgTagStatus}
}

// StatusResponse carries the node's consensus status and a few operational
// fields the daemon and operators read over QUIC (the former HTTP /status).
type StatusResponse struct {
	Round          uint64   // Round is the current consensus round
	EpochLength    uint64   // EpochLength is the number of rounds per epoch
	Epoch          uint64   // Epoch is the current epoch
	LastCommitted  uint64   // LastCommitted is the last committed round
	Validators     uint32   // Validators is the active validator count
	EpochHolders   uint32   // EpochHolders is the frozen holder-snapshot count
	SystemPod      [32]byte // SystemPod is the system pod ID
	TotalTx        uint64   // TotalTx is the total committed transactions seen
	TPSMilli       uint32   // TPSMilli is the current transactions-per-second times 1000
	ConnectedPeers uint32   // ConnectedPeers is the count of connected mesh peers
}

// EncodeStatusResp encodes a status response.
// Format: [1B tag] [8B round] [8B epochLength] [8B epoch] [8B lastCommitted]
// [4B validators] [4B epochHolders] [32B systemPod] [8B totalTx] [4B tpsMilli]
// [4B connectedPeers].
func EncodeStatusResp(resp *StatusResponse) []byte {
	buf := make([]byte, 89)
	buf[0] = MsgTagStatusResp
	binary.BigEndian.PutUint64(buf[1:9], resp.Round)
	binary.BigEndian.PutUint64(buf[9:17], resp.EpochLength)
	binary.BigEndian.PutUint64(buf[17:25], resp.Epoch)
	binary.BigEndian.PutUint64(buf[25:33], resp.LastCommitted)
	binary.BigEndian.PutUint32(buf[33:37], resp.Validators)
	binary.BigEndian.PutUint32(buf[37:41], resp.EpochHolders)
	copy(buf[41:73], resp.SystemPod[:])
	binary.BigEndian.PutUint64(buf[73:81], resp.TotalTx)
	binary.BigEndian.PutUint32(buf[81:85], resp.TPSMilli)
	binary.BigEndian.PutUint32(buf[85:89], resp.ConnectedPeers)

	return buf
}

// DecodeStatusResp decodes a status response. The operational fields past the
// first 25 bytes are optional so a peer that only sets round/epoch still decodes.
func DecodeStatusResp(data []byte) (*StatusResponse, error) {
	if len(data) < 25 || data[0] != MsgTagStatusResp {
		return nil, fmt.Errorf("not a status response")
	}

	resp := &StatusResponse{
		Round:       binary.BigEndian.Uint64(data[1:9]),
		EpochLength: binary.BigEndian.Uint64(data[9:17]),
		Epoch:       binary.BigEndian.Uint64(data[17:25]),
	}

	if len(data) >= 73 {
		resp.LastCommitted = binary.BigEndian.Uint64(data[25:33])
		resp.Validators = binary.BigEndian.Uint32(data[33:37])
		resp.EpochHolders = binary.BigEndian.Uint32(data[37:41])
		copy(resp.SystemPod[:], data[41:73])
	}

	if len(data) >= 89 {
		resp.TotalTx = binary.BigEndian.Uint64(data[73:81])
		resp.TPSMilli = binary.BigEndian.Uint32(data[81:85])
		resp.ConnectedPeers = binary.BigEndian.Uint32(data[85:89])
	}

	return resp, nil
}

// EncodeHealth encodes a health probe.
// Format: [1B tag].
func EncodeHealth() []byte {
	return []byte{MsgTagHealth}
}

// EncodeHealthResp encodes a health response.
// Format: [1B tag] [1B ok].
func EncodeHealthResp(ok bool) []byte {
	buf := []byte{MsgTagHealthResp, 0}
	if ok {
		buf[1] = 1
	}

	return buf
}

// DecodeHealthResp decodes a health response, returning whether the node is ok.
func DecodeHealthResp(data []byte) (bool, error) {
	if len(data) < 2 || data[0] != MsgTagHealthResp {
		return false, fmt.Errorf("not a health response")
	}

	return data[1] == 1, nil
}

// FaucetRequest requests a faucet mint to a public key.
type FaucetRequest struct {
	Pubkey [32]byte // Pubkey is the recipient public key
	Amount uint64   // Amount is the number of tokens to mint
}

// EncodeFaucet encodes a faucet request.
// Format: [1B tag] [32B pubkey] [8B amount].
func EncodeFaucet(req *FaucetRequest) []byte {
	buf := make([]byte, 41)
	buf[0] = MsgTagFaucet
	copy(buf[1:33], req.Pubkey[:])
	binary.BigEndian.PutUint64(buf[33:41], req.Amount)

	return buf
}

// DecodeFaucet decodes a faucet request.
func DecodeFaucet(data []byte) (*FaucetRequest, error) {
	if len(data) < 41 || data[0] != MsgTagFaucet {
		return nil, fmt.Errorf("not a faucet message")
	}

	req := &FaucetRequest{Amount: binary.BigEndian.Uint64(data[33:41])}
	copy(req.Pubkey[:], data[1:33])

	return req, nil
}

// FaucetResponse is the response to a faucet request.
type FaucetResponse struct {
	Hash   []byte // Hash is the mint transaction hash (32 bytes), empty on error
	CoinID []byte // CoinID is the minted coin's object ID (32 bytes), empty on error
	Err    string // Err is a non-empty error message on failure
}

// EncodeFaucetResp encodes a faucet response.
// Format: [1B tag] [1B hashLen] [hash] [1B coinLen] [coinID] [errString].
func EncodeFaucetResp(resp *FaucetResponse) []byte {
	buf := make([]byte, 1+1+len(resp.Hash)+1+len(resp.CoinID)+len(resp.Err))
	buf[0] = MsgTagFaucetResp
	buf[1] = byte(len(resp.Hash))
	off := 2
	copy(buf[off:], resp.Hash)
	off += len(resp.Hash)
	buf[off] = byte(len(resp.CoinID))
	off++
	copy(buf[off:], resp.CoinID)
	off += len(resp.CoinID)
	copy(buf[off:], resp.Err)

	return buf
}

// DecodeFaucetResp decodes a faucet response.
func DecodeFaucetResp(data []byte) (*FaucetResponse, error) {
	if len(data) < 2 || data[0] != MsgTagFaucetResp {
		return nil, fmt.Errorf("not a faucet response")
	}

	hashLen := int(data[1])
	off := 2

	if len(data) < off+hashLen+1 {
		return nil, fmt.Errorf("faucet response truncated")
	}

	resp := &FaucetResponse{Hash: make([]byte, hashLen)}
	copy(resp.Hash, data[off:off+hashLen])
	off += hashLen

	coinLen := int(data[off])
	off++

	if len(data) < off+coinLen {
		return nil, fmt.Errorf("faucet response truncated")
	}

	resp.CoinID = make([]byte, coinLen)
	copy(resp.CoinID, data[off:off+coinLen])
	off += coinLen

	resp.Err = string(data[off:])

	return resp, nil
}

// DomainResolveRequest requests resolution of a domain name.
type DomainResolveRequest struct {
	Name string // Name is the domain name to resolve
}

// EncodeDomainResolve encodes a domain-resolution request.
// Format: [1B tag] [name].
func EncodeDomainResolve(req *DomainResolveRequest) []byte {
	buf := make([]byte, 1+len(req.Name))
	buf[0] = MsgTagDomainResolve
	copy(buf[1:], req.Name)

	return buf
}

// DecodeDomainResolve decodes a domain-resolution request.
func DecodeDomainResolve(data []byte) (*DomainResolveRequest, error) {
	if len(data) < 1 || data[0] != MsgTagDomainResolve {
		return nil, fmt.Errorf("not a domain-resolve message")
	}

	return &DomainResolveRequest{Name: string(data[1:])}, nil
}

// DomainResolveResponse is the response to a domain-resolution request.
type DomainResolveResponse struct {
	Found    bool     // Found reports whether the domain resolved
	ObjectID [32]byte // ObjectID is the resolved object ID when Found
}

// EncodeDomainResolveResp encodes a domain-resolution response.
// Format: [1B tag] [1B found] [32B objectID].
func EncodeDomainResolveResp(resp *DomainResolveResponse) []byte {
	buf := make([]byte, 34)
	buf[0] = MsgTagDomainResolveResp

	if resp.Found {
		buf[1] = 1
	}

	copy(buf[2:34], resp.ObjectID[:])

	return buf
}

// DecodeDomainResolveResp decodes a domain-resolution response.
func DecodeDomainResolveResp(data []byte) (*DomainResolveResponse, error) {
	if len(data) < 34 || data[0] != MsgTagDomainResolveResp {
		return nil, fmt.Errorf("not a domain-resolve response")
	}

	resp := &DomainResolveResponse{Found: data[1] == 1}
	copy(resp.ObjectID[:], data[2:34])

	return resp, nil
}

// Transaction status states reported by GetTxStatus.
const (
	TxStateUnknown   uint8 = 0 // TxStateUnknown means the node has no record of the hash
	TxStatePending   uint8 = 1 // TxStatePending means the node ingested it but it is not committed
	TxStateFinalized uint8 = 2 // TxStateFinalized means it committed and applied
	TxStateFailed    uint8 = 3 // TxStateFailed means it committed but did not apply
)

// GetTxStatusRequest requests a transaction's status by hash.
type GetTxStatusRequest struct {
	Hash [32]byte // Hash is the transaction hash
}

// EncodeGetTxStatus encodes a tx-status request. Format: [1B tag] [32B hash].
func EncodeGetTxStatus(req *GetTxStatusRequest) []byte {
	buf := make([]byte, 33)
	buf[0] = MsgTagGetTxStatus
	copy(buf[1:33], req.Hash[:])

	return buf
}

// DecodeGetTxStatus decodes a tx-status request.
func DecodeGetTxStatus(data []byte) (*GetTxStatusRequest, error) {
	if len(data) < 33 || data[0] != MsgTagGetTxStatus {
		return nil, fmt.Errorf("not a get-tx-status message")
	}

	req := &GetTxStatusRequest{}
	copy(req.Hash[:], data[1:33])

	return req, nil
}

// GetTxStatusResponse carries a transaction's state and, on failure, its reason.
type GetTxStatusResponse struct {
	State  uint8 // State is one of the TxState constants
	Reason uint8 // Reason is the consensus.FailReason code when State is TxStateFailed
}

// EncodeGetTxStatusResp encodes a tx-status response.
// Format: [1B tag] [1B state] [1B reason].
func EncodeGetTxStatusResp(resp *GetTxStatusResponse) []byte {
	return []byte{MsgTagGetTxStatusResp, resp.State, resp.Reason}
}

// DecodeGetTxStatusResp decodes a tx-status response.
func DecodeGetTxStatusResp(data []byte) (*GetTxStatusResponse, error) {
	if len(data) < 3 || data[0] != MsgTagGetTxStatusResp {
		return nil, fmt.Errorf("not a get-tx-status response")
	}

	return &GetTxStatusResponse{State: data[1], Reason: data[2]}, nil
}

// GetVertexRequest requests a single DAG vertex by hash.
type GetVertexRequest struct {
	Hash [32]byte // Hash is the requested vertex's hash
}

// EncodeGetVertex encodes a vertex-fetch request.
// Format: [1B tag] [32B hash].
func EncodeGetVertex(req *GetVertexRequest) []byte {
	buf := make([]byte, 33)
	buf[0] = MsgTagGetVertex
	copy(buf[1:33], req.Hash[:])

	return buf
}

// DecodeGetVertex decodes a vertex-fetch request.
func DecodeGetVertex(data []byte) (*GetVertexRequest, error) {
	if len(data) < 33 || data[0] != MsgTagGetVertex {
		return nil, fmt.Errorf("not a get-vertex message")
	}

	req := &GetVertexRequest{}
	copy(req.Hash[:], data[1:33])

	return req, nil
}

// GetVertexResponse carries a vertex payload or a not-found flag.
type GetVertexResponse struct {
	Found bool   // Found reports whether the vertex was located
	Data  []byte // Data is the serialized Vertex FlatBuffer when Found
}

// EncodeGetVertexResp encodes a vertex-fetch response.
// Format: [1B tag] [1B found] [data].
func EncodeGetVertexResp(resp *GetVertexResponse) []byte {
	buf := make([]byte, 2+len(resp.Data))
	buf[0] = MsgTagGetVertexResp

	if resp.Found {
		buf[1] = 1
	}

	copy(buf[2:], resp.Data)

	return buf
}

// DecodeGetVertexResp decodes a vertex-fetch response.
func DecodeGetVertexResp(data []byte) (*GetVertexResponse, error) {
	if len(data) < 2 || data[0] != MsgTagGetVertexResp {
		return nil, fmt.Errorf("not a get-vertex response")
	}

	resp := &GetVertexResponse{Found: data[1] == 1}

	if len(data) > 2 {
		resp.Data = make([]byte, len(data)-2)
		copy(resp.Data, data[2:])
	}

	return resp, nil
}
