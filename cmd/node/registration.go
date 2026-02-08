package main

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"BluePods/internal/consensus"
	"BluePods/internal/genesis"
	"BluePods/internal/logger"
)

// registerAsValidator sends a register_validator raw transaction to the bootstrap node.
func (n *Node) registerAsValidator() error {
	var blsPubkeyBytes []byte
	if n.blsKey != nil {
		blsPubkeyBytes = n.blsKey.PublicKeyBytes()
	}

	tx := genesis.BuildRegisterValidatorRawTx(
		n.cfg.PrivateKey,
		n.systemPod,
		n.cfg.HTTPAddress,
		n.cfg.QUICAddress,
		blsPubkeyBytes,
	)

	registrationHTTP := n.getRegistrationHTTPAddr()
	logger.Info("registering as validator", "target", registrationHTTP)

	resp, err := http.Post(
		"http://"+registrationHTTP+"/tx",
		"application/octet-stream",
		bytes.NewReader(tx),
	)
	if err != nil {
		return fmt.Errorf("send registration tx:\n%w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("registration failed: status %d", resp.StatusCode)
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

	n.dag.AddValidator(pubHash, n.cfg.HTTPAddress, n.cfg.QUICAddress, blsPub)
}

// getRegistrationHTTPAddr derives the HTTP address for validator registration.
// Uses RegistrationAddr if set, otherwise falls back to BootstrapAddr.
// Convention: HTTP port = QUIC port - 920 (e.g., QUIC 9000 -> HTTP 8080).
func (n *Node) getRegistrationHTTPAddr() string {
	quicAddr := n.cfg.RegistrationAddr
	if quicAddr == "" {
		quicAddr = n.cfg.BootstrapAddr
	}

	host := quicAddr
	portStr := ""

	if idx := strings.LastIndex(host, ":"); idx != -1 {
		portStr = host[idx+1:]
		host = host[:idx]
	}

	if portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil {
			httpPort := port - 920
			if httpPort > 0 {
				return fmt.Sprintf("%s:%d", host, httpPort)
			}
		}
	}

	return host + ":8080"
}
