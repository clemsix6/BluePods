package daemon

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"BluePods/internal/network"
	"BluePods/internal/validators"
)

const (
	// alpnProtocol matches the node's QUIC ALPN identifier.
	alpnProtocol = "bluepods/1"

	// dialTimeout bounds a QUIC dial to a node.
	dialTimeout = 5 * time.Second

	// requestTimeout bounds a single QUIC request/response round-trip.
	requestTimeout = 8 * time.Second

	// lengthPrefixSize is the framing prefix width, matching internal/network.
	lengthPrefixSize = 4

	// maxResponseSize caps a single response, matching internal/network.
	maxResponseSize = 16 << 20
)

// Daemon is the client-side library that synchronizes the validator set, computes
// object holders by rendezvous hashing, collects attestations, and submits
// attested transactions. It imports only the pure shared packages (attest,
// validators, network codecs) and quic-go, never the node's heavy subsystems.
type Daemon struct {
	// nodeAddrs are the QUIC addresses of nodes used for syncing and submission.
	nodeAddrs []string

	// tlsConfig is the QUIC client TLS config (certless, manual verification).
	tlsConfig *tls.Config

	// quicConfig is the shared QUIC configuration.
	quicConfig *quic.Config

	// mu protects the synced validator set and epoch.
	mu sync.RWMutex

	// validatorSet is the locally cached validator set used for rendezvous.
	validatorSet *validators.ValidatorSet

	// epoch is the network epoch the validator set was synced at.
	epoch uint64
}

// New creates a daemon over the given node addresses and synchronizes the
// validator set and epoch from one of them.
func New(nodeAddrs []string) (*Daemon, error) {
	if len(nodeAddrs) == 0 {
		return nil, fmt.Errorf("at least one node address is required")
	}

	d := &Daemon{
		nodeAddrs: nodeAddrs,
		tlsConfig: &tls.Config{
			InsecureSkipVerify: true, // node verifies the client; the client trusts by quorum
			NextProtos:         []string{alpnProtocol},
		},
		quicConfig: &quic.Config{
			MaxIdleTimeout:  30 * time.Second,
			KeepAlivePeriod: 10 * time.Second,
		},
	}

	if err := d.SyncValidators(); err != nil {
		return nil, fmt.Errorf("initial validator sync:\n%w", err)
	}

	return d, nil
}

// Epoch returns the epoch the cached validator set was synced at.
func (d *Daemon) Epoch() uint64 {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return d.epoch
}

// Validators returns the cached validator set used for rendezvous hashing.
func (d *Daemon) Validators() *validators.ValidatorSet {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return d.validatorSet
}

// SyncValidators fetches the validator set and epoch from a node and caches them.
// It also pulls the epoch from the status message so the daemon scopes its
// attestations to the live epoch.
func (d *Daemon) SyncValidators() error {
	resp, err := d.roundTrip(network.EncodeGetValidators())
	if err != nil {
		return fmt.Errorf("get validators:\n%w", err)
	}

	parsed, err := network.DecodeGetValidatorsResp(resp)
	if err != nil {
		return fmt.Errorf("decode validators:\n%w", err)
	}

	vs := validators.NewValidatorSet(nil)
	for i := range parsed.Validators {
		v := &parsed.Validators[i]
		vs.Add(validators.Hash(v.Pubkey), "", v.QUICAddr, v.BLSPubkey)
	}

	d.mu.Lock()
	d.validatorSet = vs
	d.epoch = parsed.Epoch
	d.mu.Unlock()

	return nil
}

// GetObject fetches an object FlatBuffer by ID from a node. It returns nil bytes
// and a nil error when the object is not found.
func (d *Daemon) GetObject(id [32]byte) ([]byte, error) {
	resp, err := d.roundTrip(network.EncodeGetObject(&network.GetObjectRequest{ObjectID: id}))
	if err != nil {
		return nil, fmt.Errorf("get object:\n%w", err)
	}

	parsed, err := network.DecodeGetObjectResp(resp)
	if err != nil {
		return nil, fmt.Errorf("decode object:\n%w", err)
	}

	if !parsed.Found {
		return nil, nil
	}

	return parsed.Data, nil
}

// roundTrip dials a node, sends one length-prefixed request, and returns the
// length-prefixed response. It tries each configured node until one answers.
func (d *Daemon) roundTrip(request []byte) ([]byte, error) {
	var lastErr error

	for _, addr := range d.nodeAddrs {
		resp, err := d.roundTripAddr(addr, request)
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}

	return nil, fmt.Errorf("all nodes failed: %w", lastErr)
}

// roundTripAddr performs a single request/response against one node address.
func (d *Daemon) roundTripAddr(addr string, request []byte) ([]byte, error) {
	dialCtx, dialCancel := context.WithTimeout(context.Background(), dialTimeout)
	defer dialCancel()

	conn, err := quic.DialAddr(dialCtx, addr, d.tlsConfig, d.quicConfig)
	if err != nil {
		return nil, fmt.Errorf("dial %s:\n%w", addr, err)
	}
	defer conn.CloseWithError(0, "")

	reqCtx, reqCancel := context.WithTimeout(context.Background(), requestTimeout)
	defer reqCancel()

	stream, err := conn.OpenStreamSync(reqCtx)
	if err != nil {
		return nil, fmt.Errorf("open stream:\n%w", err)
	}
	defer stream.Close()

	deadline, _ := reqCtx.Deadline()
	stream.SetDeadline(deadline)

	if err := writeFrame(stream, request); err != nil {
		return nil, fmt.Errorf("write request:\n%w", err)
	}

	resp, err := readFrame(stream)
	if err != nil {
		return nil, fmt.Errorf("read response:\n%w", err)
	}

	return resp, nil
}

// writeFrame writes a length-prefixed message, matching the node's framing.
func writeFrame(w io.Writer, data []byte) error {
	var prefix [lengthPrefixSize]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(data)))

	if _, err := w.Write(prefix[:]); err != nil {
		return err
	}

	_, err := w.Write(data)
	return err
}

// readFrame reads a length-prefixed message, matching the node's framing.
func readFrame(r io.Reader) ([]byte, error) {
	var prefix [lengthPrefixSize]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, err
	}

	length := binary.BigEndian.Uint32(prefix[:])
	if length > maxResponseSize {
		return nil, fmt.Errorf("response too large: %d", length)
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}

	return data, nil
}
