package api

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zeebo/blake3"

	"BluePods/internal/consensus"
	"BluePods/internal/genesis"
	"BluePods/internal/logger"
	"BluePods/internal/types"
)

const (
	// maxTxSize is the maximum transaction size in bytes.
	maxTxSize = 1 << 20 // 1 MB
)

// TxSubmitter accepts transactions for inclusion in the next vertex.
type TxSubmitter interface {
	SubmitTx(tx []byte)
}

// TxGossiper gossips transactions to network peers.
type TxGossiper interface {
	GossipTx(tx []byte)
}

// StatusProvider exposes consensus state for monitoring.
type StatusProvider interface {
	Round() uint64
	LastCommittedRound() uint64
	ValidatorCount() int
	FullQuorumAchieved() bool
	ValidatorsInfo() []*consensus.ValidatorInfo
}

// ObjectQuerier reads objects from state.
type ObjectQuerier interface {
	GetObject(id [32]byte) []byte
}

// TxAggregator aggregates a raw Transaction into an AttestedTransaction.
type TxAggregator interface {
	Aggregate(ctx context.Context, txData []byte) ([]byte, error)
}

// HolderRouter routes GetObject requests to holders when object is not local.
type HolderRouter interface {
	RouteGetObject(id [32]byte) ([]byte, error)
}

// FaucetConfig holds configuration for the faucet endpoint.
type FaucetConfig struct {
	PrivKey   ed25519.PrivateKey // PrivKey is the node's private key for signing mint txs
	SystemPod [32]byte           // SystemPod is the system pod ID
}

// Server is the HTTP API server.
type Server struct {
	addr       string         // addr is the HTTP listen address
	submitter  TxSubmitter    // submitter accepts transactions for consensus
	gossiper   TxGossiper     // gossiper forwards transactions to peers
	status     StatusProvider // status provides consensus state for monitoring
	objects    ObjectQuerier  // objects provides object lookup
	faucet     *FaucetConfig  // faucet config for mint endpoint (nil = disabled)
	aggregator TxAggregator   // aggregator transforms raw tx into attested tx (nil = wrap only)
	router     HolderRouter   // router routes GetObject to holders (nil = local only)
	server     *http.Server   // server is the underlying HTTP server
}

// New creates a new HTTP API server.
func New(
	addr string,
	submitter TxSubmitter,
	gossiper TxGossiper,
	status StatusProvider,
	objects ObjectQuerier,
	faucet *FaucetConfig,
	aggregator TxAggregator,
	router HolderRouter,
) *Server {
	return &Server{
		addr:       addr,
		submitter:  submitter,
		gossiper:   gossiper,
		status:     status,
		objects:    objects,
		faucet:     faucet,
		aggregator: aggregator,
		router:     router,
	}
}

// Start starts the HTTP server in a goroutine.
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tx", s.handleSubmitTx)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /faucet", s.handleFaucet)
	mux.HandleFunc("GET /validators", s.handleValidators)
	mux.HandleFunc("GET /object/", s.handleGetObject)

	s.server = &http.Server{
		Addr:         s.addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("http api started", "addr", s.addr)

		if err := s.server.ListenAndServe(); err != http.ErrServerClosed {
			logger.Error("http server error", "error", err)
		}
	}()

	return nil
}

// Stop gracefully shuts down the HTTP server.
func (s *Server) Stop() error {
	if s.server == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return s.server.Shutdown(ctx)
}

// handleSubmitTx handles POST /tx requests.
// Accepts a raw Transaction, aggregates it into an ATX, then submits.
func (s *Server) handleSubmitTx(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTxSize))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}

	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "empty transaction")
		return
	}

	// Validate transaction structure, hash, and signature
	if err := validateTx(body); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid transaction: %v", err))
		return
	}

	// Extract hash from raw Transaction
	hash, err := extractRawTxHash(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid transaction: %v", err))
		return
	}

	// Aggregate: raw tx → ATX
	atxBytes, err := s.aggregateTx(r.Context(), body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("aggregation failed: %v", err))
		return
	}

	s.submitter.SubmitTx(atxBytes)

	if s.gossiper != nil {
		s.gossiper.GossipTx(atxBytes)
	}

	logger.Debug("tx submitted", "hash", hex.EncodeToString(hash[:8]))

	writeJSON(w, http.StatusAccepted, map[string]string{
		"hash": hex.EncodeToString(hash),
	})
}

// aggregateTx converts a raw Transaction into an AttestedTransaction.
// If aggregator is available, uses it. Otherwise wraps with empty objects/proofs.
func (s *Server) aggregateTx(ctx context.Context, txData []byte) ([]byte, error) {
	if s.aggregator != nil {
		return s.aggregator.Aggregate(ctx, txData)
	}

	// No aggregator (bootstrap/early init) — wrap in empty ATX
	return genesis.WrapInATX(txData), nil
}

// handleHealth handles GET /health requests.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
	})
}

// handleStatus handles GET /status requests.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	if s.status == nil {
		writeError(w, http.StatusServiceUnavailable, "status not available")
		return
	}

	resp := map[string]any{
		"round":              s.status.Round(),
		"lastCommitted":      s.status.LastCommittedRound(),
		"validators":         s.status.ValidatorCount(),
		"fullQuorumAchieved": s.status.FullQuorumAchieved(),
	}

	if s.faucet != nil {
		resp["systemPod"] = hex.EncodeToString(s.faucet.SystemPod[:])
	}

	writeJSON(w, http.StatusOK, resp)
}

// faucetRequest is the JSON body for POST /faucet.
type faucetRequest struct {
	Pubkey string `json:"pubkey"` // Pubkey is hex-encoded 32-byte public key
	Amount uint64 `json:"amount"` // Amount of tokens to mint
}

// handleFaucet handles POST /faucet requests.
// Faucet builds ATX internally (mint has no objects to aggregate).
func (s *Server) handleFaucet(w http.ResponseWriter, r *http.Request) {
	if s.faucet == nil {
		writeError(w, http.StatusServiceUnavailable, "faucet not available")
		return
	}

	var req faucetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	pubkeyBytes, err := hex.DecodeString(req.Pubkey)
	if err != nil || len(pubkeyBytes) != 32 {
		writeError(w, http.StatusBadRequest, "pubkey must be 64 hex chars (32 bytes)")
		return
	}

	if req.Amount == 0 {
		writeError(w, http.StatusBadRequest, "amount must be > 0")
		return
	}

	var owner [32]byte
	copy(owner[:], pubkeyBytes)

	txBytes := genesis.BuildMintTx(s.faucet.PrivKey, s.faucet.SystemPod, req.Amount, owner)

	txHash, err := extractAtxHash(txBytes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build mint tx")
		return
	}

	// Predict the coinID: blake3(txHash || 0_u32_LE)
	coinID := computeObjectID(txHash)

	s.submitter.SubmitTx(txBytes)

	if s.gossiper != nil {
		s.gossiper.GossipTx(txBytes)
	}

	logger.Info("faucet mint", "to", req.Pubkey[:16], "amount", req.Amount)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"hash":   hex.EncodeToString(txHash),
		"coinID": hex.EncodeToString(coinID[:]),
	})
}

// computeObjectID computes blake3(txHash || 0_u32_LE) for the first created object.
func computeObjectID(txHash []byte) [32]byte {
	var buf [36]byte
	copy(buf[:32], txHash)
	binary.LittleEndian.PutUint32(buf[32:], 0)

	return blake3.Sum256(buf[:])
}

// validatorResponse is the JSON response for a single validator.
type validatorResponse struct {
	Pubkey string `json:"pubkey"` // Pubkey is the hex-encoded public key
	HTTP   string `json:"http"`   // HTTP is the HTTP API endpoint
}

// handleValidators handles GET /validators requests.
func (s *Server) handleValidators(w http.ResponseWriter, _ *http.Request) {
	if s.status == nil {
		writeError(w, http.StatusServiceUnavailable, "status not available")
		return
	}

	infos := s.status.ValidatorsInfo()
	resp := make([]validatorResponse, len(infos))

	for i, info := range infos {
		resp[i] = validatorResponse{
			Pubkey: hex.EncodeToString(info.Pubkey[:]),
			HTTP:   info.HTTPAddr,
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleGetObject handles GET /object/{id} requests.
// If the object is not local and a router is available, routes to holders.
func (s *Server) handleGetObject(w http.ResponseWriter, r *http.Request) {
	if s.objects == nil {
		writeError(w, http.StatusServiceUnavailable, "object store not available")
		return
	}

	// Extract ID from path: /object/{id}
	idHex := strings.TrimPrefix(r.URL.Path, "/object/")
	if idHex == "" {
		writeError(w, http.StatusBadRequest, "missing object ID")
		return
	}

	idBytes, err := hex.DecodeString(idHex)
	if err != nil || len(idBytes) != 32 {
		writeError(w, http.StatusBadRequest, "invalid object ID (expected 64 hex chars)")
		return
	}

	var id [32]byte
	copy(id[:], idBytes)

	data := s.objects.GetObject(id)

	// If not local, try routing to holders
	if data == nil && s.router != nil {
		routed, err := s.router.RouteGetObject(id)
		if err == nil && routed != nil {
			data = routed
		}
	}

	if data == nil {
		writeError(w, http.StatusNotFound, "object not found")
		return
	}

	obj := types.GetRootAsObject(data, 0)

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          hex.EncodeToString(obj.IdBytes()),
		"version":     obj.Version(),
		"owner":       hex.EncodeToString(obj.OwnerBytes()),
		"replication": obj.Replication(),
		"content":     hex.EncodeToString(obj.ContentBytes()),
	})
}

// extractRawTxHash extracts the transaction hash from a raw Transaction FlatBuffer.
func extractRawTxHash(data []byte) ([]byte, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("data too short")
	}

	tx := types.GetRootAsTransaction(data, 0)

	hash := tx.HashBytes()
	if len(hash) != 32 {
		return nil, fmt.Errorf("invalid hash length: %d", len(hash))
	}

	return hash, nil
}

// extractAtxHash extracts the transaction hash from AttestedTransaction bytes.
func extractAtxHash(data []byte) ([]byte, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("data too short")
	}

	atx := types.GetRootAsAttestedTransaction(data, 0)

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

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// writeError writes an error response.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{
		"error": message,
	})
}
