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
	"BluePods/internal/validation"
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
	case network.MsgTagGetTxStatus:
		return n.handleGetTxStatus(data)
	case network.MsgTagGetVertex:
		return n.handleGetVertex(data)
	default:
		return nil, fmt.Errorf("unhandled client message tag: 0x%02x", tag)
	}
}

// handleSubmitTx answers a submission request. The body is either a raw
// Transaction (a singleton-only intent) or a full AttestedTransaction. A raw
// transaction is validated structurally, checked to reference only singletons,
// and wrapped into a trivial ATX. A full ATX is validated structurally and
// included as-is. In both cases the resulting ATX enters the next vertex and is
// gossiped. The lifecycle checks (version, owner, gas, replay) and the
// epoch-aware ATX quorum verification both run later, in the commit path.
func (n *Node) handleSubmitTx(data []byte) ([]byte, error) {
	req, err := network.DecodeSubmitTx(data)
	if err != nil {
		return nil, err
	}

	if n.dag == nil {
		return submitErr("consensus not available"), nil
	}

	atx, hash, err := n.ingestSubmission(req.Body)
	if err != nil {
		return submitErr(err.Error()), nil
	}

	var pendingHash [32]byte
	copy(pendingHash[:], hash)
	n.txIndex.markPending(pendingHash, n.dag.Round())

	n.dag.SubmitTx(atx)
	n.GossipTx(atx)

	return network.EncodeSubmitTxResp(&network.SubmitTxResponse{Hash: hash}), nil
}

// ingestSubmission turns a submission body into an ATX ready for a vertex. It
// distinguishes a raw transaction from a full ATX by structure: a body that
// validates as a raw Transaction (hash and signature check out) is a raw
// singleton-only intent and is wrapped; otherwise it is treated as a full ATX.
func (n *Node) ingestSubmission(body []byte) (atx []byte, hash []byte, err error) {
	if validation.ValidateTx(body) == nil {
		return n.ingestRawTx(body)
	}

	return ingestATX(body)
}

// ingestRawTx wraps a validated raw transaction into a trivial ATX after
// confirming it references only singletons (replicated objects must arrive as a
// full ATX with quorum proofs).
func (n *Node) ingestRawTx(body []byte) (atx []byte, hash []byte, err error) {
	tx := types.GetRootAsTransaction(body, 0)

	if err := n.referencesOnlySingletons(tx); err != nil {
		return nil, nil, err
	}

	return genesis.WrapInATX(body), copyHash(tx.HashBytes()), nil
}

// ingestATX validates the structure of a full ATX and returns it unchanged. The
// quorum proofs are reverified by the epoch-aware verifier at commit time, so
// ingress only checks that the nested transaction is structurally sound.
func ingestATX(body []byte) (atx []byte, hash []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("malformed attested transaction")
		}
	}()

	parsed := types.GetRootAsAttestedTransaction(body, 0)

	tx := parsed.Transaction(nil)
	if tx == nil {
		return nil, nil, fmt.Errorf("attested transaction missing transaction")
	}

	if len(tx.HashBytes()) != 32 {
		return nil, nil, fmt.Errorf("attested transaction has invalid hash")
	}

	return body, copyHash(tx.HashBytes()), nil
}

// referencesOnlySingletons reports an error if the transaction references any
// replicated object. A singleton (replication 0) is held by every validator, so
// it is present in local state; a referenced object that is unknown locally or
// replicated must be submitted as a full ATX instead.
func (n *Node) referencesOnlySingletons(tx *types.Transaction) error {
	if n.state == nil {
		return fmt.Errorf("state not available")
	}

	var ref types.ObjectRef

	for _, mutable := range []bool{true, false} {
		count := tx.ReadRefsLength()
		if mutable {
			count = tx.MutableRefsLength()
		}

		for i := 0; i < count; i++ {
			if mutable {
				tx.MutableRefs(&ref, i)
			} else {
				tx.ReadRefs(&ref, i)
			}

			if err := n.requireSingletonRef(&ref); err != nil {
				return err
			}
		}
	}

	return nil
}

// requireSingletonRef checks that a single object reference resolves to a
// singleton. Domain-only references are resolved through state first.
func (n *Node) requireSingletonRef(ref *types.ObjectRef) error {
	id, ok := n.resolveRefID(ref)
	if !ok {
		// A domain-only reference that does not resolve is left for the
		// lifecycle checks; it carries no replicated object here.
		return nil
	}

	objectData := n.state.GetObject(id)
	if objectData == nil {
		return fmt.Errorf("raw transaction references unknown object %x", id[:4])
	}

	if obj := types.GetRootAsObject(objectData, 0); obj.Replication() != 0 {
		return fmt.Errorf("raw transaction references replicated object %x; submit a full ATX", id[:4])
	}

	return nil
}

// resolveRefID returns the 32-byte object ID of a reference, resolving a
// domain-only reference through state. It returns ok=false when no ID is found.
func (n *Node) resolveRefID(ref *types.ObjectRef) ([32]byte, bool) {
	if idBytes := ref.IdBytes(); len(idBytes) == 32 {
		var id [32]byte
		copy(id[:], idBytes)
		return id, true
	}

	if domain := ref.Domain(); len(domain) > 0 {
		return n.state.ResolveDomain(string(domain))
	}

	return [32]byte{}, false
}

// submitErr builds a submission response carrying an error message.
func submitErr(msg string) []byte {
	return network.EncodeSubmitTxResp(&network.SubmitTxResponse{Err: msg})
}

// copyHash returns a fresh copy of a transaction hash.
func copyHash(hash []byte) []byte {
	out := make([]byte, len(hash))
	copy(out, hash)
	return out
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

	// A local-only request never routes to a remote holder, so a non-holder
	// answers not-found and a caller can probe local holdership.
	if req.LocalOnly {
		return network.EncodeGetObjectResp(&network.GetObjectResponse{Found: false}), nil
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

// handleStatus returns the node's consensus status and operational counters.
func (n *Node) handleStatus() ([]byte, error) {
	if n.dag == nil {
		return nil, fmt.Errorf("consensus not available")
	}

	return network.EncodeStatusResp(&network.StatusResponse{
		Round:          n.dag.Round(),
		EpochLength:    n.dag.EpochLength(),
		Epoch:          n.dag.Epoch(),
		LastCommitted:  n.dag.LastCommittedRound(),
		Validators:     uint32(len(n.dag.ValidatorsInfo())),
		EpochHolders:   uint32(n.dag.EpochHoldersCount()),
		SystemPod:      n.systemPod,
		TotalTx:        n.stats.TotalTx(),
		TPSMilli:       uint32(n.stats.TPS() * 1000),
		ConnectedPeers: uint32(n.network.ConnectedPeers()),
	}), nil
}

// handleGetTxStatus returns a transaction's last-known status from the index.
func (n *Node) handleGetTxStatus(data []byte) ([]byte, error) {
	req, err := network.DecodeGetTxStatus(data)
	if err != nil {
		return nil, err
	}

	state, reason := n.txIndex.get(req.Hash)

	return network.EncodeGetTxStatusResp(&network.GetTxStatusResponse{State: state, Reason: reason}), nil
}

// handleFaucet grants tokens to a public key by splitting from the genesis
// reserve coin. Handling is serialized so rapid sequential requests each
// reference a distinct reserve-coin version and none lose the version race at
// commit.
func (n *Node) handleFaucet(data []byte) ([]byte, error) {
	req, err := network.DecodeFaucet(data)
	if err != nil {
		return nil, err
	}

	if req.Amount == 0 {
		return network.EncodeFaucetResp(&network.FaucetResponse{Err: "amount must be > 0"}), nil
	}

	n.faucetMu.Lock()
	defer n.faucetMu.Unlock()

	txBytes := n.buildFaucetTx(req)

	txHash, err := faucetTxHash(txBytes)
	if err != nil {
		return network.EncodeFaucetResp(&network.FaucetResponse{Err: "failed to build faucet tx"}), nil
	}

	coinID := firstCreatedObjectID(txHash)

	var pendingHash [32]byte
	copy(pendingHash[:], txHash)
	n.txIndex.markPending(pendingHash, n.dag.Round())

	n.dag.SubmitTx(txBytes)
	n.GossipTx(txBytes)

	logger.Info("faucet split", "amount", req.Amount)

	return network.EncodeFaucetResp(&network.FaucetResponse{
		Hash:   txHash,
		CoinID: coinID[:],
	}), nil
}

// buildFaucetTx builds a signed split ATX moving tokens from the genesis reserve
// coin to the requester. There is no user-callable mint; the faucet splits the
// bootstrap node's reserve, paying its own gas from that same coin. Requests
// fail naturally once the reserve is exhausted (insufficient balance). The caller
// holds faucetMu, so the version it reserves is unique to this in-flight split.
func (n *Node) buildFaucetTx(req *network.FaucetRequest) []byte {
	owner := deriveOwner(n.cfg.PrivateKey)
	reserveCoinID := genesis.GenesisCoinID(owner)
	version := n.pickReserveVersion(n.reserveCoinVersion(reserveCoinID))

	return genesis.BuildSplitTx(n.cfg.PrivateKey, n.systemPod, reserveCoinID, version, req.Pubkey, req.Amount)
}

// pickReserveVersion returns the reserve-coin version the next faucet split must
// reference and advances the in-memory counter past it. It takes the larger of
// the committed version (committedVersion) and the in-memory counter, so the
// counter stays ahead of in-flight splits yet self-heals to committed state once
// splits actually commit (or after a restart). The caller must hold faucetMu.
func (n *Node) pickReserveVersion(committedVersion uint64) uint64 {
	chosen := committedVersion
	if n.faucetNextVersion > chosen {
		chosen = n.faucetNextVersion
	}

	n.faucetNextVersion = chosen + 1

	return chosen
}

// reserveCoinVersion reads the current version of the genesis reserve coin from
// local state. Returns 0 when the coin is not yet stored locally.
func (n *Node) reserveCoinVersion(reserveCoinID [32]byte) uint64 {
	data := n.state.GetObject(reserveCoinID)
	if data == nil {
		return 0
	}

	return types.GetRootAsObject(data, 0).Version()
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

// faucetTxHash extracts the embedded transaction hash from a faucet split ATX.
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
// the first object a transaction creates (the split-out coin for the faucet).
func firstCreatedObjectID(txHash []byte) [32]byte {
	var buf [36]byte
	copy(buf[:32], txHash)
	binary.LittleEndian.PutUint32(buf[32:], 0)

	return blake3.Sum256(buf[:])
}
