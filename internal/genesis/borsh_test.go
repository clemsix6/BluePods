package genesis

import "testing"

// TestEncodeDecodeRegisterValidatorRewardCoin round-trips the optional
// reward-coin designation through EncodeRegisterValidatorArgs and
// DecodeRegisterValidatorRewardCoin, alongside the quic address, the BLS key and
// its proof of possession.
func TestEncodeDecodeRegisterValidatorRewardCoin(t *testing.T) {
	quicAddr := []byte("quic://x:1")
	bls := []byte{0xAA, 0xBB, 0xCC}
	pop := []byte{0xDD, 0xEE}
	rewardCoin := [32]byte{0x01, 0x02, 0x03}

	data := EncodeRegisterValidatorArgs(quicAddr, bls, pop, rewardCoin)

	got, ok := DecodeRegisterValidatorRewardCoin(data)
	if !ok {
		t.Fatal("expected a reward coin designation")
	}
	if got != rewardCoin {
		t.Errorf("reward coin = %x, want %x", got, rewardCoin)
	}

	gotQUIC, gotBLS, gotPoP := DecodeRegisterValidatorArgs(data)
	if gotQUIC != string(quicAddr) {
		t.Errorf("quic addr = %q, want %q", gotQUIC, quicAddr)
	}
	if string(gotBLS) != string(bls) {
		t.Errorf("bls pubkey = %x, want %x", gotBLS, bls)
	}
	if string(gotPoP) != string(pop) {
		t.Errorf("bls pop = %x, want %x", gotPoP, pop)
	}
}

// TestEncodeRegisterValidatorArgs_ZeroRewardCoinOmitsField verifies that a zero
// rewardCoin omits the trailing field entirely rather than encoding 32 zero
// bytes, so DecodeRegisterValidatorRewardCoin reports ok=false, distinguishing
// "no designation" from "designated the zero coin".
func TestEncodeRegisterValidatorArgs_ZeroRewardCoinOmitsField(t *testing.T) {
	quicAddr := []byte("quic://x:1")
	bls := []byte{0xAA}
	pop := []byte{0xBB}

	data := EncodeRegisterValidatorArgs(quicAddr, bls, pop, [32]byte{})

	if _, ok := DecodeRegisterValidatorRewardCoin(data); ok {
		t.Fatal("expected ok=false: a zero reward coin must omit the field")
	}

	want := encodeRegisterValidatorArgs(quicAddr, bls, pop)
	if string(data) != string(want) {
		t.Error("zero-reward-coin encoding diverges from the proof-carrying encoding without a reward coin")
	}
}

// TestDecodeRegisterValidatorArgs_AbsentProof checks that args stopping after the
// BLS key decode with a nil proof rather than borrowing bytes from another field:
// the commit path refuses such a registration, so the absence must be visible.
func TestDecodeRegisterValidatorArgs_AbsentProof(t *testing.T) {
	quicAddr := []byte("quic://x:1")
	bls := []byte{0xAA, 0xBB}

	data := EncodeRegisterValidatorArgs(quicAddr, bls, nil, [32]byte{})

	gotQUIC, gotBLS, gotPoP := DecodeRegisterValidatorArgs(data)
	if gotQUIC != string(quicAddr) || string(gotBLS) != string(bls) {
		t.Fatalf("address/key decoding broke: addr=%q key=%x", gotQUIC, gotBLS)
	}
	if len(gotPoP) != 0 {
		t.Errorf("bls pop = %x, want none", gotPoP)
	}
}
