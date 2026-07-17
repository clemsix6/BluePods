package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/zeebo/blake3"

	"BluePods/internal/consensus"
	"BluePods/internal/events"
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
	case network.MsgTagGetVertexRange:
		return n.handleGetVertexRange(data)
	case network.MsgTagStateFingerprint:
		return n.handleStateFingerprint()
	case network.MsgTagTestControl:
		return n.handleTestControl(data)
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

	atx, hash, kind, err := n.ingestSubmission(req.Body)
	if err != nil {
		events.IngressTxRejected("invalid_submission", err.Error())
		return submitErr(err.Error()), nil
	}

	var pendingHash [32]byte
	copy(pendingHash[:], hash)
	n.txIndex.markPending(pendingHash, n.dag.Round())

	n.dag.SubmitTx(atx)
	n.GossipTx(atx)

	var hashArr [32]byte
	copy(hashArr[:], hash)
	events.IngressTxReceived(hashArr, kind)

	return network.EncodeSubmitTxResp(&network.SubmitTxResponse{Hash: hash}), nil
}

// ingestSubmission turns a submission body into an ATX ready for a vertex. It
// distinguishes a raw transaction from a full ATX by structure: a body that
// validates as a raw Transaction (hash and signature check out) is a raw
// singleton-only intent and is wrapped; otherwise it is treated as a full ATX.
// kind reports which path was taken ("raw" or "attested"), for the ingress
// event the caller emits.
func (n *Node) ingestSubmission(body []byte) (atx []byte, hash []byte, kind string, err error) {
	if validation.ValidateTx(body) == nil {
		atx, hash, err = n.ingestRawTx(body)
		return atx, hash, "raw", err
	}

	atx, hash, err = ingestATX(body)
	return atx, hash, "attested", err
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

// handleGetObject answers an object read. A local-only probe replies strictly
// from local state. A routed read trusts the local copy only when this node
// currently holds the object; otherwise it probes the holders, which carry the
// object's current version, and never serves a possibly-stale local copy.
func (n *Node) handleGetObject(data []byte) ([]byte, error) {
	req, err := network.DecodeGetObject(data)
	if err != nil {
		return nil, err
	}

	local := n.localObject(req.ObjectID)

	// A local-only probe answers strictly from local state: a holder returns its
	// stored copy, a miss is a definitive not-found, and it never re-routes.
	if req.LocalOnly {
		return encodeObjectResp(local), nil
	}

	// A routed read trusts the local copy only when this node currently holds the
	// object (or the object is a singleton every validator holds). A node that has
	// lost holdership keeps its copy lazily past epoch transitions, so that copy
	// can lag the holders after an ownership change. In that case the holders carry
	// the truth and are probed for the current version.
	if local != nil && n.holdsObjectLocally(req.ObjectID, local) {
		return encodeObjectResp(local), nil
	}

	if fetched := n.fetchObjectFromHolder(req.ObjectID); fetched != nil {
		return encodeObjectResp(fetched), nil
	}

	return encodeObjectResp(nil), nil
}

// localObject returns this node's locally stored copy of an object, or nil when
// state is unavailable or the object is not stored locally.
func (n *Node) localObject(id [32]byte) []byte {
	if n.state == nil {
		return nil
	}

	return n.state.GetObject(id)
}

// holdsObjectLocally reports whether this node is a current holder of the object
// and can treat its local copy as authoritative. Singletons (replication 0) are
// held by every validator. A replicated object is held only when this node is in
// the current rendezvous holder set; a node that merely retained a copy from a
// past epoch is not a current holder and must defer to the holders.
func (n *Node) holdsObjectLocally(id [32]byte, data []byte) bool {
	replication := types.GetRootAsObject(data, 0).Replication()
	if replication == 0 {
		return true
	}

	if n.isHolder == nil {
		return false
	}

	return n.isHolder(id, replication)
}

// encodeObjectResp encodes a GetObject response: found with the data when the
// object is present, not-found when data is nil.
func encodeObjectResp(data []byte) []byte {
	if data == nil {
		return network.EncodeGetObjectResp(&network.GetObjectResponse{Found: false})
	}

	return network.EncodeGetObjectResp(&network.GetObjectResponse{Found: true, Data: data})
}

// fetchObjectFromHolder fetches an object from one of its computed holders over
// the persistent QUIC mesh. It asks each holder for the object locally (the
// remote handler returns not-found rather than re-routing), preventing cascades.
func (n *Node) fetchObjectFromHolder(id [32]byte) []byte {
	if n.rendezvous == nil || n.dag == nil {
		return nil
	}

	own := n.myPubkey()
	req := buildHolderObjectRequest(id)
	reqBytes := network.EncodeGetObject(&req)

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

// buildHolderObjectRequest builds the inter-node GetObject request sent to a
// computed holder. The request always sets LocalOnly: a remote holder answers
// from its own local state only, and a miss is a definitive not-found for that
// holder, never a re-route to the rest of the mesh. Without LocalOnly, a holder
// that also lacked the object would re-enter the non-local path itself and probe
// every other validator (including the original requester), so an object no
// node holds would cascade across the whole mesh instead of one bounded
// round-trip per holder.
func buildHolderObjectRequest(id [32]byte) network.GetObjectRequest {
	return network.GetObjectRequest{ObjectID: id, LocalOnly: true}
}

// requestObjectFrom sends a LocalOnly GetObject request to one holder over the
// mesh. It returns the object bytes, or nil if the holder is unreachable or
// its local state lacks the object.
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

// handleGetValidators returns the epoch-frozen holder snapshot plus the current
// epoch. The daemon computes object holders and assembles attestation quorums
// from this response, and the chain verifies those quorums against the same
// frozen snapshot (HoldersForEpoch), so serving the epoch holders here — not the
// live validator set — keeps the daemon's holder set identical to the verifier's.
func (n *Node) handleGetValidators() ([]byte, error) {
	if n.dag == nil {
		return nil, fmt.Errorf("consensus not available")
	}

	infos := n.dag.EpochHolders().All()
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

	var hashArr [32]byte
	copy(hashArr[:], txHash)
	events.IngressTxReceived(hashArr, "faucet")

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
