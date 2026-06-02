package network

import (
	"bytes"
	"testing"
)

func TestSubmitTxRoundTrip(t *testing.T) {
	body := []byte{0x10, 0x20, 0x30, 0x40}
	enc := EncodeSubmitTx(&SubmitTxRequest{Body: body})

	if !IsClientMessage(enc) {
		t.Fatal("submit-tx not classified as a client message")
	}

	dec, err := DecodeSubmitTx(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !bytes.Equal(dec.Body, body) {
		t.Fatalf("body mismatch: %x", dec.Body)
	}
}

func TestSubmitTxRespRoundTrip(t *testing.T) {
	hash := bytes.Repeat([]byte{0xAB}, 32)

	encOK := EncodeSubmitTxResp(&SubmitTxResponse{Hash: hash})
	decOK, err := DecodeSubmitTxResp(encOK)
	if err != nil {
		t.Fatalf("decode ok: %v", err)
	}
	if !bytes.Equal(decOK.Hash, hash) || decOK.Err != "" {
		t.Fatalf("ok mismatch: hash=%x err=%q", decOK.Hash, decOK.Err)
	}

	encErr := EncodeSubmitTxResp(&SubmitTxResponse{Err: "not yet enabled"})
	decErr, err := DecodeSubmitTxResp(encErr)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if len(decErr.Hash) != 0 || decErr.Err != "not yet enabled" {
		t.Fatalf("err mismatch: hash=%x err=%q", decErr.Hash, decErr.Err)
	}
}

func TestGetObjectRoundTrip(t *testing.T) {
	var id [32]byte
	id[0], id[31] = 0x01, 0xFF

	enc := EncodeGetObject(&GetObjectRequest{ObjectID: id})
	dec, err := DecodeGetObject(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.ObjectID != id {
		t.Fatalf("id mismatch: %x", dec.ObjectID)
	}
}

func TestGetObjectRespRoundTrip(t *testing.T) {
	data := []byte("object-bytes")

	enc := EncodeGetObjectResp(&GetObjectResponse{Found: true, Data: data})
	dec, err := DecodeGetObjectResp(enc)
	if err != nil {
		t.Fatalf("decode found: %v", err)
	}
	if !dec.Found || !bytes.Equal(dec.Data, data) {
		t.Fatalf("found mismatch: %v %x", dec.Found, dec.Data)
	}

	encNF := EncodeGetObjectResp(&GetObjectResponse{Found: false})
	decNF, err := DecodeGetObjectResp(encNF)
	if err != nil {
		t.Fatalf("decode not-found: %v", err)
	}
	if decNF.Found || len(decNF.Data) != 0 {
		t.Fatalf("not-found mismatch: %v %x", decNF.Found, decNF.Data)
	}
}

func TestGetValidatorsRoundTrip(t *testing.T) {
	resp := &GetValidatorsResponse{
		Epoch: 7,
		Validators: []ValidatorEntry{
			{Pubkey: [32]byte{0x01}, BLSPubkey: [48]byte{0xAA}, QUICAddr: "10.0.0.1:9000"},
			{Pubkey: [32]byte{0x02}, BLSPubkey: [48]byte{0xBB}, QUICAddr: ""},
		},
	}

	enc := EncodeGetValidatorsResp(resp)
	dec, err := DecodeGetValidatorsResp(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if dec.Epoch != 7 || len(dec.Validators) != 2 {
		t.Fatalf("header mismatch: epoch=%d n=%d", dec.Epoch, len(dec.Validators))
	}

	if dec.Validators[0].Pubkey != resp.Validators[0].Pubkey ||
		dec.Validators[0].BLSPubkey != resp.Validators[0].BLSPubkey ||
		dec.Validators[0].QUICAddr != "10.0.0.1:9000" {
		t.Fatalf("entry 0 mismatch: %+v", dec.Validators[0])
	}

	if dec.Validators[1].QUICAddr != "" {
		t.Fatalf("entry 1 addr mismatch: %q", dec.Validators[1].QUICAddr)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	var pod [32]byte
	pod[0], pod[31] = 0xAB, 0xCD

	enc := EncodeStatusResp(&StatusResponse{
		Round:         123,
		EpochLength:   1000,
		Epoch:         2,
		LastCommitted: 120,
		Validators:    5,
		EpochHolders:  4,
		SystemPod:     pod,
	})
	dec, err := DecodeStatusResp(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if dec.Round != 123 || dec.EpochLength != 1000 || dec.Epoch != 2 {
		t.Fatalf("status mismatch: %+v", dec)
	}

	if dec.LastCommitted != 120 || dec.Validators != 5 || dec.EpochHolders != 4 || dec.SystemPod != pod {
		t.Fatalf("operational fields mismatch: %+v", dec)
	}
}

func TestGetObjectLocalFlagRoundTrip(t *testing.T) {
	var id [32]byte
	id[0] = 0x7F

	enc := EncodeGetObject(&GetObjectRequest{ObjectID: id, LocalOnly: true})
	dec, err := DecodeGetObject(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if dec.ObjectID != id || !dec.LocalOnly {
		t.Fatalf("local flag mismatch: %+v", dec)
	}

	// A legacy 33-byte request decodes with LocalOnly defaulting to false.
	legacy := []byte{MsgTagGetObject}
	legacy = append(legacy, id[:]...)
	decLegacy, err := DecodeGetObject(legacy)
	if err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if decLegacy.LocalOnly {
		t.Fatalf("legacy request should default LocalOnly to false")
	}
}

func TestHealthRoundTrip(t *testing.T) {
	ok, err := DecodeHealthResp(EncodeHealthResp(true))
	if err != nil || !ok {
		t.Fatalf("health true: ok=%v err=%v", ok, err)
	}
}

func TestFaucetRoundTrip(t *testing.T) {
	var pk [32]byte
	pk[0] = 0x09

	enc := EncodeFaucet(&FaucetRequest{Pubkey: pk, Amount: 500})
	dec, err := DecodeFaucet(enc)
	if err != nil {
		t.Fatalf("decode req: %v", err)
	}
	if dec.Pubkey != pk || dec.Amount != 500 {
		t.Fatalf("req mismatch: %+v", dec)
	}

	hash := bytes.Repeat([]byte{0x01}, 32)
	coin := bytes.Repeat([]byte{0x02}, 32)
	encResp := EncodeFaucetResp(&FaucetResponse{Hash: hash, CoinID: coin})
	decResp, err := DecodeFaucetResp(encResp)
	if err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if !bytes.Equal(decResp.Hash, hash) || !bytes.Equal(decResp.CoinID, coin) || decResp.Err != "" {
		t.Fatalf("resp mismatch: %+v", decResp)
	}
}

func TestDomainResolveRoundTrip(t *testing.T) {
	enc := EncodeDomainResolve(&DomainResolveRequest{Name: "example.bp"})
	dec, err := DecodeDomainResolve(enc)
	if err != nil {
		t.Fatalf("decode req: %v", err)
	}
	if dec.Name != "example.bp" {
		t.Fatalf("name mismatch: %q", dec.Name)
	}

	var id [32]byte
	id[5] = 0x42
	encResp := EncodeDomainResolveResp(&DomainResolveResponse{Found: true, ObjectID: id})
	decResp, err := DecodeDomainResolveResp(encResp)
	if err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if !decResp.Found || decResp.ObjectID != id {
		t.Fatalf("resp mismatch: %+v", decResp)
	}
}

func TestGetTxStatusRoundTrip(t *testing.T) {
	var hash [32]byte
	hash[0], hash[31] = 0x11, 0x22

	req, err := DecodeGetTxStatus(EncodeGetTxStatus(&GetTxStatusRequest{Hash: hash}))
	if err != nil || req.Hash != hash {
		t.Fatalf("request round-trip failed: %v %x", err, req.Hash)
	}

	resp, err := DecodeGetTxStatusResp(EncodeGetTxStatusResp(&GetTxStatusResponse{State: TxStateFailed, Reason: 1}))
	if err != nil || resp.State != TxStateFailed || resp.Reason != 1 {
		t.Fatalf("response round-trip failed: %v %+v", err, resp)
	}
}

func TestStatusRespRoundTripWithOperationalFields(t *testing.T) {
	in := &StatusResponse{
		Round: 1428, EpochLength: 1000, Epoch: 3, LastCommitted: 1426,
		Validators: 7, EpochHolders: 7, TotalTx: 18392, TPSMilli: 12400, ConnectedPeers: 6,
	}
	in.SystemPod[0] = 0xAB

	out, err := DecodeStatusResp(EncodeStatusResp(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.TotalTx != 18392 || out.TPSMilli != 12400 || out.ConnectedPeers != 6 {
		t.Fatalf("operational fields lost: %+v", out)
	}
	if out.Round != 1428 || out.Validators != 7 || out.SystemPod[0] != 0xAB {
		t.Fatalf("base fields lost: %+v", out)
	}
}

func TestClientTagsDoNotCollideWithAttestation(t *testing.T) {
	// Attestation tags are 0x01-0x03; all client tags must be >= 0x04.
	allTags := []byte{
		MsgTagSubmitTx, MsgTagGetObject, MsgTagGetObjectResp,
		MsgTagGetValidators, MsgTagGetValidatorsResp, MsgTagStatus,
		MsgTagStatusResp, MsgTagHealth, MsgTagHealthResp,
		MsgTagFaucet, MsgTagFaucetResp, MsgTagDomainResolve,
		MsgTagDomainResolveResp, MsgTagSubmitTxResp,
	}

	for _, tag := range allTags {
		if tag < 0x04 {
			t.Fatalf("client tag 0x%02x collides with attestation range", tag)
		}
	}

	// Only request tags are classified as inbound client messages; response tags
	// are produced by the node and never received as requests.
	requestTags := []byte{
		MsgTagSubmitTx, MsgTagGetObject, MsgTagGetValidators,
		MsgTagStatus, MsgTagHealth, MsgTagFaucet, MsgTagDomainResolve,
	}

	for _, tag := range requestTags {
		if !IsClientMessage([]byte{tag}) {
			t.Fatalf("request tag 0x%02x not classified as a client message", tag)
		}
	}

	// A snapshot request's FlatBuffer root offset (0x0c) must not be classified
	// as a client message, or it would be misrouted away from the snapshot path.
	if IsClientMessage([]byte{0x0c, 0x00, 0x00, 0x00}) {
		t.Fatal("snapshot root offset 0x0c misclassified as a client message")
	}
}

func TestGossipTxRoundTrip(t *testing.T) {
	body := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02}

	enc := EncodeGossipTx(body)
	if enc[0] != MsgTagGossipTx {
		t.Fatalf("gossip-tx tag missing: got 0x%02x", enc[0])
	}

	decoded, ok := DecodeGossipTx(enc)
	if !ok {
		t.Fatal("DecodeGossipTx should recognize a tagged gossip transaction")
	}

	if string(decoded) != string(body) {
		t.Fatalf("gossip-tx body mismatch: got %x, want %x", decoded, body)
	}

	// A vertex (untagged FlatBuffer, first byte is a small root offset) must not be
	// classified as a gossiped transaction.
	if _, ok := DecodeGossipTx([]byte{0x0c, 0x00, 0x00, 0x00}); ok {
		t.Fatal("an untagged vertex must not decode as a gossip transaction")
	}
}
