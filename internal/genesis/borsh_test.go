package genesis

import "testing"

// TestEncodeDecodeRegisterValidatorRewardCoin round-trips the optional
// reward-coin designation through EncodeRegisterValidatorArgs and
// DecodeRegisterValidatorRewardCoin, alongside the quic address and BLS key
// decoding unchanged.
func TestEncodeDecodeRegisterValidatorRewardCoin(t *testing.T) {
	quicAddr := []byte("quic://x:1")
	bls := []byte{0xAA, 0xBB, 0xCC}
	rewardCoin := [32]byte{0x01, 0x02, 0x03}

	data := EncodeRegisterValidatorArgs(quicAddr, bls, rewardCoin)

	got, ok := DecodeRegisterValidatorRewardCoin(data)
	if !ok {
		t.Fatal("expected a reward coin designation")
	}
	if got != rewardCoin {
		t.Errorf("reward coin = %x, want %x", got, rewardCoin)
	}

	gotQUIC, gotBLS := DecodeRegisterValidatorArgs(data)
	if gotQUIC != string(quicAddr) {
		t.Errorf("quic addr = %q, want %q", gotQUIC, quicAddr)
	}
	if string(gotBLS) != string(bls) {
		t.Errorf("bls pubkey = %x, want %x", gotBLS, bls)
	}
}

// TestEncodeRegisterValidatorArgs_ZeroRewardCoinOmitsField verifies that a zero
// rewardCoin omits the trailing field entirely rather than encoding 32 zero
// bytes, so DecodeRegisterValidatorRewardCoin reports ok=false — distinguishing
// "no designation" from "designated the zero coin" — and the encoding stays
// byte-identical to the older two-field format.
func TestEncodeRegisterValidatorArgs_ZeroRewardCoinOmitsField(t *testing.T) {
	quicAddr := []byte("quic://x:1")
	bls := []byte{0xAA}

	data := EncodeRegisterValidatorArgs(quicAddr, bls, [32]byte{})

	if _, ok := DecodeRegisterValidatorRewardCoin(data); ok {
		t.Fatal("expected ok=false: a zero reward coin must omit the field")
	}

	want := encodeRegisterValidatorArgs(quicAddr, bls)
	if string(data) != string(want) {
		t.Error("zero-reward-coin encoding diverges from the two-field (older) encoding")
	}
}
