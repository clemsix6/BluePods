package client

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/quic-go/quic-go"

	"BluePods/internal/network"
)

const (
	// quicALPN matches the node's QUIC ALPN identifier.
	quicALPN = "bluepods/1"

	// quicDialTimeout bounds a QUIC dial to a node.
	quicDialTimeout = 5 * time.Second

	// quicRequestTimeout bounds a single request/response round-trip.
	quicRequestTimeout = 8 * time.Second

	// quicLengthPrefix is the framing prefix width, matching internal/network.
	quicLengthPrefix = 4

	// quicMaxResponse caps a single response, matching internal/network.
	quicMaxResponse = 16 << 20
)

// QUICTransport is the SDK's QUIC client transport. It performs length-prefixed
// request/response round-trips against a node using the node's length-prefixed QUIC client codecs.
type QUICTransport struct {
	// nodeAddr is the node's QUIC address.
	nodeAddr string

	// tlsConfig is the certless QUIC client TLS config.
	tlsConfig *tls.Config

	// quicConfig is the shared QUIC configuration.
	quicConfig *quic.Config
}

// NewQUICTransport creates an SDK QUIC transport to a node address.
func NewQUICTransport(nodeAddr string) *QUICTransport {
	return &QUICTransport{
		nodeAddr: nodeAddr,
		tlsConfig: &tls.Config{
			InsecureSkipVerify: true, // node authenticates the client; SDK trusts by quorum
			NextProtos:         []string{quicALPN},
		},
		quicConfig: &quic.Config{
			MaxIdleTimeout:  30 * time.Second,
			KeepAlivePeriod: 10 * time.Second,
		},
	}
}

// Status fetches the node's consensus status (round, epoch length, epoch).
func (t *QUICTransport) Status() (*network.StatusResponse, error) {
	resp, err := t.roundTrip(network.EncodeStatus())
	if err != nil {
		return nil, fmt.Errorf("status:\n%w", err)
	}

	return network.DecodeStatusResp(resp)
}

// Validators fetches the validator set and current epoch.
func (t *QUICTransport) Validators() (*network.GetValidatorsResponse, error) {
	resp, err := t.roundTrip(network.EncodeGetValidators())
	if err != nil {
		return nil, fmt.Errorf("validators:\n%w", err)
	}

	return network.DecodeGetValidatorsResp(resp)
}

// GetObject fetches an object FlatBuffer by ID. It returns nil bytes when the
// object is not found.
func (t *QUICTransport) GetObject(id [32]byte) ([]byte, error) {
	return t.getObject(id, false)
}

// GetObjectLocal fetches an object only from the node's local state, never
// routing to a remote holder. It returns nil bytes when the node does not hold
// the object, which lets a caller probe local holdership.
func (t *QUICTransport) GetObjectLocal(id [32]byte) ([]byte, error) {
	return t.getObject(id, true)
}

// getObject performs a GetObject round-trip with the given local-only flag.
func (t *QUICTransport) getObject(id [32]byte, localOnly bool) ([]byte, error) {
	req := &network.GetObjectRequest{ObjectID: id, LocalOnly: localOnly}

	resp, err := t.roundTrip(network.EncodeGetObject(req))
	if err != nil {
		return nil, fmt.Errorf("get object:\n%w", err)
	}

	parsed, err := network.DecodeGetObjectResp(resp)
	if err != nil {
		return nil, err
	}

	if !parsed.Found {
		return nil, nil
	}

	return parsed.Data, nil
}

// SubmitTx submits a raw transaction or ATX body and returns the tx hash.
func (t *QUICTransport) SubmitTx(body []byte) ([]byte, error) {
	resp, err := t.roundTrip(network.EncodeSubmitTx(&network.SubmitTxRequest{Body: body}))
	if err != nil {
		return nil, fmt.Errorf("submit:\n%w", err)
	}

	parsed, err := network.DecodeSubmitTxResp(resp)
	if err != nil {
		return nil, err
	}

	if parsed.Err != "" {
		return nil, fmt.Errorf("submission rejected: %s", parsed.Err)
	}

	return parsed.Hash, nil
}

// Health probes the node's liveness over QUIC.
func (t *QUICTransport) Health() (bool, error) {
	resp, err := t.roundTrip(network.EncodeHealth())
	if err != nil {
		return false, fmt.Errorf("health:\n%w", err)
	}

	return network.DecodeHealthResp(resp)
}

// DomainResolve resolves a domain name to an object ID over QUIC. It returns the
// object ID and a found flag.
func (t *QUICTransport) DomainResolve(name string) ([32]byte, bool, error) {
	resp, err := t.roundTrip(network.EncodeDomainResolve(&network.DomainResolveRequest{Name: name}))
	if err != nil {
		return [32]byte{}, false, fmt.Errorf("domain resolve:\n%w", err)
	}

	parsed, err := network.DecodeDomainResolveResp(resp)
	if err != nil {
		return [32]byte{}, false, err
	}

	return parsed.ObjectID, parsed.Found, nil
}

// Faucet requests a faucet mint and returns the minted coin ID and transaction hash.
func (t *QUICTransport) Faucet(pubkey [32]byte, amount uint64) ([32]byte, [32]byte, error) {
	resp, err := t.roundTrip(network.EncodeFaucet(&network.FaucetRequest{Pubkey: pubkey, Amount: amount}))
	if err != nil {
		return [32]byte{}, [32]byte{}, fmt.Errorf("faucet:\n%w", err)
	}

	parsed, err := network.DecodeFaucetResp(resp)
	if err != nil {
		return [32]byte{}, [32]byte{}, err
	}

	if parsed.Err != "" {
		return [32]byte{}, [32]byte{}, fmt.Errorf("faucet rejected: %s", parsed.Err)
	}

	var coinID [32]byte
	copy(coinID[:], parsed.CoinID)

	var txHash [32]byte
	copy(txHash[:], parsed.Hash)

	return coinID, txHash, nil
}

// GetTxStatus fetches a transaction's status by hash over QUIC.
func (t *QUICTransport) GetTxStatus(hash [32]byte) (*network.GetTxStatusResponse, error) {
	resp, err := t.roundTrip(network.EncodeGetTxStatus(&network.GetTxStatusRequest{Hash: hash}))
	if err != nil {
		return nil, fmt.Errorf("get tx status:\n%w", err)
	}

	return network.DecodeGetTxStatusResp(resp)
}

// roundTrip dials the node, sends one length-prefixed request, and returns the
// length-prefixed response.
func (t *QUICTransport) roundTrip(request []byte) ([]byte, error) {
	dialCtx, dialCancel := context.WithTimeout(context.Background(), quicDialTimeout)
	defer dialCancel()

	conn, err := quic.DialAddr(dialCtx, t.nodeAddr, t.tlsConfig, t.quicConfig)
	if err != nil {
		return nil, fmt.Errorf("dial %s:\n%w", t.nodeAddr, err)
	}
	defer conn.CloseWithError(0, "")

	reqCtx, reqCancel := context.WithTimeout(context.Background(), quicRequestTimeout)
	defer reqCancel()

	stream, err := conn.OpenStreamSync(reqCtx)
	if err != nil {
		return nil, fmt.Errorf("open stream:\n%w", err)
	}
	defer stream.Close()

	if deadline, ok := reqCtx.Deadline(); ok {
		stream.SetDeadline(deadline)
	}

	if err := writeQUICFrame(stream, request); err != nil {
		return nil, fmt.Errorf("write request:\n%w", err)
	}

	return readQUICFrame(stream)
}

// writeQUICFrame writes a length-prefixed message, matching the node's framing.
func writeQUICFrame(w io.Writer, data []byte) error {
	var prefix [quicLengthPrefix]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(data)))

	if _, err := w.Write(prefix[:]); err != nil {
		return err
	}

	_, err := w.Write(data)
	return err
}

// readQUICFrame reads a length-prefixed message, matching the node's framing.
func readQUICFrame(r io.Reader) ([]byte, error) {
	var prefix [quicLengthPrefix]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, err
	}

	length := binary.BigEndian.Uint32(prefix[:])
	if length > quicMaxResponse {
		return nil, fmt.Errorf("response too large: %d", length)
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}

	return data, nil
}
