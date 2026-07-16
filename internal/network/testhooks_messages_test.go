package network

import "testing"

// TestStateFingerprintRoundTrip verifies the state-fingerprint request/response
// codec round-trips every field.
func TestStateFingerprintRoundTrip(t *testing.T) {
	if !IsClientMessage(EncodeStateFingerprint()) {
		t.Fatal("state-fingerprint request not classified as a client message")
	}

	var checksum [32]byte
	checksum[0], checksum[31] = 0xAB, 0xCD

	want := &FingerprintResponse{
		Round:        42,
		Checksum:     checksum,
		TotalSupply:  1000,
		CoinsTotal:   700,
		TotalBonded:  250,
		Deposits:     40,
		FeesInFlight: 10,
	}

	enc := EncodeStateFingerprintResp(want)
	got, err := DecodeStateFingerprintResp(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if *got != *want {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, want)
	}
}

// TestStateFingerprintRespCarriesRefusal verifies a refusal response decodes
// with its error message and zeroed fields.
func TestStateFingerprintRespCarriesRefusal(t *testing.T) {
	enc := EncodeStateFingerprintResp(&FingerprintResponse{Err: "test hooks disabled"})

	got, err := DecodeStateFingerprintResp(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.Err != "test hooks disabled" {
		t.Fatalf("err = %q, want %q", got.Err, "test hooks disabled")
	}

	if got.Round != 0 || got.TotalSupply != 0 || got.Checksum != ([32]byte{}) {
		t.Fatalf("refusal response should be zeroed besides Err: %+v", got)
	}
}

// TestTestControlRoundTrip verifies the test-control request/response codec
// round-trips the op and pubkey list.
func TestTestControlRoundTrip(t *testing.T) {
	if !IsClientMessage(EncodeTestControl(&TestControlRequest{Op: TestControlOpSetPartition})) {
		t.Fatal("test-control request not classified as a client message")
	}

	var a, b [32]byte
	a[0] = 0x11
	b[0] = 0x22

	want := &TestControlRequest{Op: TestControlOpSetPartition, Pubkeys: [][32]byte{a, b}}
	enc := EncodeTestControl(want)

	got, err := DecodeTestControl(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.Op != want.Op || len(got.Pubkeys) != 2 || got.Pubkeys[0] != a || got.Pubkeys[1] != b {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, want)
	}
}

// TestTestControlClearRoundTrip verifies the clear operation round-trips with
// an empty pubkey list.
func TestTestControlClearRoundTrip(t *testing.T) {
	enc := EncodeTestControl(&TestControlRequest{Op: TestControlOpClearPartition})

	got, err := DecodeTestControl(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.Op != TestControlOpClearPartition || len(got.Pubkeys) != 0 {
		t.Fatalf("got %+v, want clear op with no pubkeys", got)
	}
}

// TestTestControlRespRoundTrip verifies the response codec round-trips both
// the success (empty Err) and failure cases.
func TestTestControlRespRoundTrip(t *testing.T) {
	encOK := EncodeTestControlResp(&TestControlResponse{})
	gotOK, err := DecodeTestControlResp(encOK)
	if err != nil {
		t.Fatalf("decode ok: %v", err)
	}
	if gotOK.Err != "" {
		t.Fatalf("ok response err = %q, want empty", gotOK.Err)
	}

	encErr := EncodeTestControlResp(&TestControlResponse{Err: "test hooks disabled"})
	gotErr, err := DecodeTestControlResp(encErr)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if gotErr.Err != "test hooks disabled" {
		t.Fatalf("err response = %q, want %q", gotErr.Err, "test hooks disabled")
	}
}

// TestDecodeTestControlTruncated verifies a truncated pubkey vector errors
// instead of reading past the buffer.
func TestDecodeTestControlTruncated(t *testing.T) {
	enc := EncodeTestControl(&TestControlRequest{Op: TestControlOpSetPartition, Pubkeys: [][32]byte{{}}})
	truncated := enc[:len(enc)-10]

	if _, err := DecodeTestControl(truncated); err == nil {
		t.Fatal("expected error decoding a truncated test-control message")
	}
}
