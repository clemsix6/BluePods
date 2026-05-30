package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/zeebo/blake3"

	"BluePods/internal/consensus"
	"BluePods/internal/genesis"
	"BluePods/internal/logger"
	"BluePods/internal/network"
	"BluePods/internal/types"
)

const (
	// interNodeFetchTimeout bounds an inter-node GetObject round-trip.
	interNodeFetchTimeout = 5 * time.Second

	// objectFetchProbeHolders is the replication used to probe holders for an
	// object whose true replication is not known to the routing node.
	objectFetchProbeHolders = 10
)

// handleClientMessage routes a tagged client message to its handler.
// It is invoked for any request whose first byte is a client tag (>= 0x04),
// from both ephemeral client connections and mesh peers (inter-node GetObject).
func (n *Node) handleClientMessage(data []byte) ([]byte, error) {
	tag, err := network.MessageTag(data)
	if err != nil {
		return nil, err
	}

	switch tag {
	case network.MsgTagSubmitTx:
		return n.handleSubmitTx(data)
	case network.MsgTagGetObject:
		return n.handleGetObject(data)
	case network.MsgTagGetValidators:
		return n.handleGetValidators()
	case network.MsgTagStatus:
		return n.handleStatus()
	case network.MsgTagHealth:
		return network.EncodeHealthResp(true), nil
	case network.MsgTagFaucet:
		return n.handleFaucet(data)
	case network.MsgTagDomainResolve:
		return n.handleDomainResolve(data)
	default:
		return nil, fmt.Errorf("unhandled client message tag: 0x%02x", tag)
	}
}

// handleSubmitTx answers a submission request. Submission over QUIC is wired in
// a later batch; for now it returns a typed "not yet enabled" error so the
// client surface is reachable without enabling the new ingestion path.
func (n *Node) handleSubmitTx(_ []byte) ([]byte, error) {
	return network.EncodeSubmitTxResp(&network.SubmitTxResponse{
		Err: "submission over QUIC not yet enabled",
	}), nil
}

// handleGetObject returns a held object, or fetches it from a computed holder
// over the QUIC mesh when this node does not hold it.
func (n *Node) handleGetObject(data []byte) ([]byte, error) {
	req, err := network.DecodeGetObject(data)
	if err != nil {
		return nil, err
	}

	if n.state != nil {
		if obj := n.state.GetObject(req.ObjectID); obj != nil {
			return network.EncodeGetObjectResp(&network.GetObjectResponse{Found: true, Data: obj}), nil
		}
	}

	if fetched := n.fetchObjectFromHolder(req.ObjectID); fetched != nil {
		return network.EncodeGetObjectResp(&network.GetObjectResponse{Found: true, Data: fetched}), nil
	}

	return network.EncodeGetObjectResp(&network.GetObjectResponse{Found: false}), nil
}

// fetchObjectFromHolder fetches an object from one of its computed holders over
// the persistent QUIC mesh. It asks each holder for the object locally (the
// remote handler returns not-found rather than re-routing), preventing cascades.
func (n *Node) fetchObjectFromHolder(id [32]byte) []byte {
	if n.rendezvous == nil || n.dag == nil {
		return nil
	}

	own := n.myPubkey()
	reqBytes := network.EncodeGetObject(&network.GetObjectRequest{ObjectID: id})

	for _, holder := range n.rendezvous.ComputeHolders(id, objectFetchProbeHolders) {
		if holder == own {
			continue
		}

		if obj := n.requestObjectFrom(holder, reqBytes); obj != nil {
			return obj
		}
	}

	return nil
}

// requestObjectFrom sends a local GetObject request to one holder over the mesh.
// It returns the object bytes, or nil if the holder is unreachable or lacks it.
func (n *Node) requestObjectFrom(holder consensus.Hash, reqBytes []byte) []byte {
	peer := n.network.GetPeer(holder[:])
	if peer == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), interNodeFetchTimeout)
	defer cancel()

	respBytes, err := peer.Request(ctx, reqBytes)
	if err != nil {
		logger.Debug("inter-node object fetch failed", "id_prefix", reqBytes[1:5], "error", err)
		return nil
	}

	resp, err := network.DecodeGetObjectResp(respBytes)
	if err != nil || !resp.Found {
		return nil
	}

	return resp.Data
}

// handleGetValidators returns the current validator set plus the current epoch.
func (n *Node) handleGetValidators() ([]byte, error) {
	if n.dag == nil {
		return nil, fmt.Errorf("consensus not available")
	}

	infos := n.dag.ValidatorsInfo()
	entries := make([]network.ValidatorEntry, len(infos))

	for i, info := range infos {
		entries[i] = network.ValidatorEntry{
			Pubkey:    info.Pubkey,
			BLSPubkey: info.BLSPubkey,
			QUICAddr:  info.QUICAddr,
		}
	}

	return network.EncodeGetValidatorsResp(&network.GetValidatorsResponse{
		Epoch:      n.dag.Epoch(),
		Validators: entries,
	}), nil
}

// handleStatus returns the current round, epoch length, and epoch.
func (n *Node) handleStatus() ([]byte, error) {
	if n.dag == nil {
		return nil, fmt.Errorf("consensus not available")
	}

	return network.EncodeStatusResp(&network.StatusResponse{
		Round:       n.dag.Round(),
		EpochLength: n.dag.EpochLength(),
		Epoch:       n.dag.Epoch(),
	}), nil
}

// handleFaucet mints tokens to a public key, mirroring the HTTP faucet handler.
func (n *Node) handleFaucet(data []byte) ([]byte, error) {
	req, err := network.DecodeFaucet(data)
	if err != nil {
		return nil, err
	}

	if req.Amount == 0 {
		return network.EncodeFaucetResp(&network.FaucetResponse{Err: "amount must be > 0"}), nil
	}

	txBytes := n.buildFaucetTx(req)

	txHash, err := faucetTxHash(txBytes)
	if err != nil {
		return network.EncodeFaucetResp(&network.FaucetResponse{Err: "failed to build mint tx"}), nil
	}

	coinID := firstCreatedObjectID(txHash)

	n.dag.SubmitTx(txBytes)
	n.GossipTx(txBytes)

	logger.Info("faucet mint", "amount", req.Amount)

	return network.EncodeFaucetResp(&network.FaucetResponse{
		Hash:   txHash,
		CoinID: coinID[:],
	}), nil
}

// buildFaucetTx builds a signed mint ATX granting tokens to the requester.
func (n *Node) buildFaucetTx(req *network.FaucetRequest) []byte {
	return genesis.BuildMintTx(n.cfg.PrivateKey, n.systemPod, req.Amount, req.Pubkey)
}

// handleDomainResolve resolves a domain name to an object ID via state.
func (n *Node) handleDomainResolve(data []byte) ([]byte, error) {
	req, err := network.DecodeDomainResolve(data)
	if err != nil {
		return nil, err
	}

	if n.state == nil {
		return nil, fmt.Errorf("domain resolution not available")
	}

	objectID, found := n.state.ResolveDomain(req.Name)

	return network.EncodeDomainResolveResp(&network.DomainResolveResponse{
		Found:    found,
		ObjectID: objectID,
	}), nil
}

// faucetTxHash extracts the embedded transaction hash from a mint ATX.
func faucetTxHash(atxData []byte) ([]byte, error) {
	atx := types.GetRootAsAttestedTransaction(atxData, 0)

	tx := atx.Transaction(nil)
	if tx == nil {
		return nil, fmt.Errorf("missing transaction")
	}

	hash := tx.HashBytes()
	if len(hash) != 32 {
		return nil, fmt.Errorf("invalid hash length: %d", len(hash))
	}

	return hash, nil
}

// firstCreatedObjectID computes blake3(txHash || 0_u32_LE), the ID assigned to
// the first object a transaction creates (the minted coin for the faucet).
func firstCreatedObjectID(txHash []byte) [32]byte {
	var buf [36]byte
	copy(buf[:32], txHash)
	binary.LittleEndian.PutUint32(buf[32:], 0)

	return blake3.Sum256(buf[:])
}
