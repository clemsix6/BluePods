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

	// minClientTag is the lowest tag value reserved for client messages.
	minClientTag = MsgTagSubmitTx
)

// IsClientMessage reports whether data carries a client message tag.
// It is used to classify a request before the snapshot heuristic.
func IsClientMessage(data []byte) bool {
	return len(data) > 0 && data[0] >= minClientTag
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

// GetObjectRequest requests an object by ID.
type GetObjectRequest struct {
	ObjectID [32]byte // ObjectID is the object to fetch
}

// EncodeGetObject encodes a GetObject request.
// Format: [1B tag] [32B objectID].
func EncodeGetObject(req *GetObjectRequest) []byte {
	buf := make([]byte, 33)
	buf[0] = MsgTagGetObject
	copy(buf[1:], req.ObjectID[:])

	return buf
}

// DecodeGetObject decodes a GetObject request.
func DecodeGetObject(data []byte) (*GetObjectRequest, error) {
	if len(data) < 33 || data[0] != MsgTagGetObject {
		return nil, fmt.Errorf("not a get-object message")
	}

	req := &GetObjectRequest{}
	copy(req.ObjectID[:], data[1:33])

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

// StatusResponse carries the node's consensus status.
type StatusResponse struct {
	Round       uint64 // Round is the current consensus round
	EpochLength uint64 // EpochLength is the number of rounds per epoch
	Epoch       uint64 // Epoch is the current epoch
}

// EncodeStatusResp encodes a status response.
// Format: [1B tag] [8B round] [8B epochLength] [8B epoch].
func EncodeStatusResp(resp *StatusResponse) []byte {
	buf := make([]byte, 25)
	buf[0] = MsgTagStatusResp
	binary.BigEndian.PutUint64(buf[1:9], resp.Round)
	binary.BigEndian.PutUint64(buf[9:17], resp.EpochLength)
	binary.BigEndian.PutUint64(buf[17:25], resp.Epoch)

	return buf
}

// DecodeStatusResp decodes a status response.
func DecodeStatusResp(data []byte) (*StatusResponse, error) {
	if len(data) < 25 || data[0] != MsgTagStatusResp {
		return nil, fmt.Errorf("not a status response")
	}

	return &StatusResponse{
		Round:       binary.BigEndian.Uint64(data[1:9]),
		EpochLength: binary.BigEndian.Uint64(data[9:17]),
		Epoch:       binary.BigEndian.Uint64(data[17:25]),
	}, nil
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
