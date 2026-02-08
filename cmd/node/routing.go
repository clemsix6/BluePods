package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/aggregation"
	"BluePods/internal/api"
	"BluePods/internal/consensus"
	"BluePods/internal/types"
)

// holderRouter routes GetObject requests to remote holders.
type holderRouter struct {
	rendezvous *aggregation.Rendezvous // rendezvous computes holder mappings
	dag        *consensus.DAG          // dag provides validator info
	ownPubkey  consensus.Hash          // ownPubkey is this node's public key
}

// newHolderRouter creates a HolderRouter for routing GetObject requests.
// Returns nil if aggregation is not initialized.
func (n *Node) newHolderRouter() api.HolderRouter {
	if n.rendezvous == nil {
		return nil
	}

	return &holderRouter{
		rendezvous: n.rendezvous,
		dag:        n.dag,
		ownPubkey:  n.myPubkey(),
	}
}

// RouteGetObject tries to fetch an object from its holders.
func (hr *holderRouter) RouteGetObject(id [32]byte) ([]byte, error) {
	// Use replication=10 as minimum probe size
	holders := hr.rendezvous.ComputeHolders(id, 10)

	for _, h := range holders {
		// Skip self
		if h == hr.ownPubkey {
			continue
		}

		info := hr.dag.ValidatorSet().Get(h)
		if info == nil || info.HTTPAddr == "" {
			continue
		}

		data, err := fetchObjectFromHolder(info.HTTPAddr, id)
		if err == nil {
			return data, nil
		}
	}

	return nil, fmt.Errorf("object not found on any holder")
}

// holderClient is an HTTP client with timeout for holder-to-holder requests.
// Without a timeout, routing cascades can hang the HTTP server indefinitely.
var holderClient = &http.Client{Timeout: 5 * time.Second}

// fetchObjectFromHolder fetches an object from a remote holder via HTTP.
func fetchObjectFromHolder(httpAddr string, id [32]byte) ([]byte, error) {
	url := "http://" + httpAddr + "/object/" + hex.EncodeToString(id[:])

	resp, err := holderClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var objResp struct {
		ID          string `json:"id"`
		Version     uint64 `json:"version"`
		Owner       string `json:"owner"`
		Replication uint16 `json:"replication"`
		Content     string `json:"content"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&objResp); err != nil {
		return nil, err
	}

	return rebuildObjectFromJSON(objResp.ID, objResp.Owner, objResp.Content, objResp.Version, objResp.Replication)
}

// rebuildObjectFromJSON reconstructs a FlatBuffer Object from JSON API response fields.
func rebuildObjectFromJSON(idHex, ownerHex, contentHex string, version uint64, replication uint16) ([]byte, error) {
	idBytes, err := hex.DecodeString(idHex)
	if err != nil || len(idBytes) != 32 {
		return nil, fmt.Errorf("invalid id")
	}

	ownerBytes, err := hex.DecodeString(ownerHex)
	if err != nil || len(ownerBytes) != 32 {
		return nil, fmt.Errorf("invalid owner")
	}

	contentBytes, err := hex.DecodeString(contentHex)
	if err != nil {
		return nil, fmt.Errorf("invalid content")
	}

	builder := flatbuffers.NewBuilder(512)

	idVec := builder.CreateByteVector(idBytes)
	ownerVec := builder.CreateByteVector(ownerBytes)
	contentVec := builder.CreateByteVector(contentBytes)

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, version)
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, replication)
	types.ObjectAddContent(builder, contentVec)
	offset := types.ObjectEnd(builder)

	builder.Finish(offset)

	return builder.FinishedBytes(), nil
}
