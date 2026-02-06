package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

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
}

// Server is the HTTP API server.
type Server struct {
	addr      string         // addr is the HTTP listen address
	submitter TxSubmitter    // submitter accepts transactions for consensus
	gossiper  TxGossiper    // gossiper forwards transactions to peers
	status    StatusProvider // status provides consensus state for monitoring
	server    *http.Server  // server is the underlying HTTP server
}

// New creates a new HTTP API server.
func New(addr string, submitter TxSubmitter, gossiper TxGossiper, status StatusProvider) *Server {
	return &Server{
		addr:      addr,
		submitter: submitter,
		gossiper:  gossiper,
		status:    status,
	}
}

// Start starts the HTTP server in a goroutine.
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tx", s.handleSubmitTx)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /status", s.handleStatus)

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
func (s *Server) handleSubmitTx(w http.ResponseWriter, r *http.Request) {
	// Read transaction bytes
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTxSize))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}

	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "empty transaction")
		return
	}

	// Parse to extract hash for response
	hash, err := extractTxHash(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid transaction: %v", err))
		return
	}

	// Submit to local consensus
	s.submitter.SubmitTx(body)

	// Gossip to peers so transaction reaches a producer
	// This enables validators to forward transactions they can't produce themselves
	if s.gossiper != nil {
		s.gossiper.GossipTx(body)
	}

	logger.Debug("tx submitted", "hash", hex.EncodeToString(hash[:8]))

	// Return hash
	writeJSON(w, http.StatusAccepted, map[string]string{
		"hash": hex.EncodeToString(hash),
	})
}

// handleHealth handles GET /health requests.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
	})
}

// handleStatus handles GET /status requests.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if s.status == nil {
		writeError(w, http.StatusServiceUnavailable, "status not available")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"round":              s.status.Round(),
		"lastCommitted":      s.status.LastCommittedRound(),
		"validators":         s.status.ValidatorCount(),
		"fullQuorumAchieved": s.status.FullQuorumAchieved(),
	})
}

// extractTxHash extracts the transaction hash from AttestedTransaction bytes.
func extractTxHash(data []byte) ([]byte, error) {
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
