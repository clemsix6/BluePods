package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/quic-go/quic-go"

	"BluePods/internal/consensus"
	"BluePods/internal/genesis"
	"BluePods/internal/logger"
	"BluePods/internal/network"
)

const (
	// registrationALPN matches the node's QUIC ALPN identifier.
	registrationALPN = "bluepods/1"

	// registrationDialTimeout bounds the dial to the registration target.
	registrationDialTimeout = 5 * time.Second

	// registrationRequestTimeout bounds the registration round-trip.
	registrationRequestTimeout = 8 * time.Second

	// registrationLengthPrefix is the framing prefix width, matching internal/network.
	registrationLengthPrefix = 4

	// registrationMaxResponse caps a registration response.
	registrationMaxResponse = 1 << 20
)

// registerAsValidator submits a register_validator raw transaction over QUIC to
// the registration target. The receiving validator wraps it into a trivial ATX.
func (n *Node) registerAsValidator() error {
	var blsPubkeyBytes []byte
	if n.blsKey != nil {
		blsPubkeyBytes = n.blsKey.PublicKeyBytes()
	}

	// A joining validator has no coin yet at registration time, so it designates no
	// reward coin and declares no gas coin here. setRewardCoinFromArgs then leaves
	// the reward coin zero, and the validator's reward compounds deterministically
	// into its self-stake until a later registration designates a coin explicitly.
	tx := genesis.BuildRegisterValidatorRawTx(
		n.cfg.PrivateKey,
		n.systemPod,
		n.cfg.QUICAddress,
		blsPubkeyBytes,
		[32]byte{},
	)

	target := n.registrationAddr()
	logger.Info("registering as validator", "target", target)

	if err := submitTxOverQUIC(target, tx); err != nil {
		return fmt.Errorf("submit registration tx:\n%w", err)
	}

	// Optimistic self-add: add ourselves to the local validator set immediately
	// so we can start producing vertices without waiting for the registration
	// to commit through the full DAG chain.
	n.selfAddToValidatorSet()

	logger.Info("registration submitted, self-added to validator set")
	return nil
}

// selfAddToValidatorSet adds this node to the local validator set immediately.
func (n *Node) selfAddToValidatorSet() {
	pubKey := n.cfg.PrivateKey.Public().(ed25519.PublicKey)
	var pubHash consensus.Hash
	copy(pubHash[:], pubKey)

	var blsPub [48]byte
	if n.blsKey != nil {
		copy(blsPub[:], n.blsKey.PublicKeyBytes())
	}

	n.dag.AddValidator(pubHash, n.cfg.QUICAddress, blsPub)
}

// registrationAddr returns the QUIC address used for validator registration.
// It uses RegistrationAddr if set, otherwise falls back to BootstrapAddr.
func (n *Node) registrationAddr() string {
	if n.cfg.RegistrationAddr != "" {
		return n.cfg.RegistrationAddr
	}

	return n.cfg.BootstrapAddr
}

// submitTxOverQUIC dials the target node and submits a raw transaction or ATX
// body via a single MsgSubmitTx round-trip.
func submitTxOverQUIC(addr string, body []byte) error {
	dialCtx, dialCancel := context.WithTimeout(context.Background(), registrationDialTimeout)
	defer dialCancel()

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true, // the node authenticates the client; we trust by quorum
		NextProtos:         []string{registrationALPN},
	}

	conn, err := quic.DialAddr(dialCtx, addr, tlsConfig, &quic.Config{})
	if err != nil {
		return fmt.Errorf("dial %s:\n%w", addr, err)
	}
	defer conn.CloseWithError(0, "")

	resp, err := registrationRoundTrip(conn, body)
	if err != nil {
		return err
	}

	parsed, err := network.DecodeSubmitTxResp(resp)
	if err != nil {
		return fmt.Errorf("decode registration response:\n%w", err)
	}

	if parsed.Err != "" {
		return fmt.Errorf("registration rejected: %s", parsed.Err)
	}

	return nil
}

// registrationRoundTrip opens a stream, writes the submit request, and reads the
// length-prefixed response.
func registrationRoundTrip(conn *quic.Conn, body []byte) ([]byte, error) {
	reqCtx, reqCancel := context.WithTimeout(context.Background(), registrationRequestTimeout)
	defer reqCancel()

	stream, err := conn.OpenStreamSync(reqCtx)
	if err != nil {
		return nil, fmt.Errorf("open stream:\n%w", err)
	}
	defer stream.Close()

	if deadline, ok := reqCtx.Deadline(); ok {
		stream.SetDeadline(deadline)
	}

	request := network.EncodeSubmitTx(&network.SubmitTxRequest{Body: body})
	if err := writeRegistrationFrame(stream, request); err != nil {
		return nil, fmt.Errorf("write request:\n%w", err)
	}

	return readRegistrationFrame(stream)
}

// writeRegistrationFrame writes a length-prefixed message, matching node framing.
func writeRegistrationFrame(w io.Writer, data []byte) error {
	var prefix [registrationLengthPrefix]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(data)))

	if _, err := w.Write(prefix[:]); err != nil {
		return err
	}

	_, err := w.Write(data)
	return err
}

// readRegistrationFrame reads a length-prefixed message, matching node framing.
func readRegistrationFrame(r io.Reader) ([]byte, error) {
	var prefix [registrationLengthPrefix]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, err
	}

	length := binary.BigEndian.Uint32(prefix[:])
	if length > registrationMaxResponse {
		return nil, fmt.Errorf("registration response too large: %d", length)
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}

	return data, nil
}
