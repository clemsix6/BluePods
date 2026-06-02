package integration

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/consensus"
	"BluePods/internal/genesis"
	"BluePods/internal/network"
	"BluePods/pkg/client"
)

const (
	// txStatusPollInterval is how long to wait between GetTxStatus polls.
	txStatusPollInterval = 200 * time.Millisecond

	// txStatusPollAttempts is how many polls to attempt before failing.
	txStatusPollAttempts = 50

	// txStatusFaucetAmount is the amount of tokens to faucet for the tx status test.
	txStatusFaucetAmount = 1_000_000

	// txStatusMaxGas mirrors the client SDK's default gas budget.
	txStatusMaxGas uint64 = 1000
)

// TestSimTxStatus verifies the tx-status substrate end to end on a 3-node cluster:
// - a submitted singleton transfer reaches TxStateFinalized,
// - a second transfer reusing the now-stale coin version reaches TxStateFailed with FailVersion,
// - Status.TotalTx is non-zero after commits.
func TestSimTxStatus(t *testing.T) {
	cluster := NewCluster(t, 3, WithHTTPBase(19700), WithQUICBase(19700+920))
	cluster.WaitReady(60 * time.Second)

	nodeAddr := cluster.Bootstrap().Addr()

	// Create a wallet from a known private key so we can compute tx hashes ourselves.
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var pubKey32 [32]byte
	copy(pubKey32[:], pubKey)

	wallet := client.NewWalletFromKey(privKey)

	cli := cluster.Client(0)

	// Faucet a coin to the wallet.
	coinID, _, err := cli.Faucet(pubKey32, txStatusFaucetAmount)
	if err != nil {
		t.Fatalf("faucet: %v", err)
	}
	WaitForObject(t, cli, coinID, 30*time.Second)

	if err := wallet.RefreshCoin(cli, coinID); err != nil {
		t.Fatalf("refresh coin: %v", err)
	}

	coin := wallet.GetCoin(coinID)
	if coin == nil {
		t.Fatalf("coin not tracked after refresh")
	}

	systemPod := cluster.SystemPodID()
	staleVersion := coin.Version

	t.Run("finalized", func(t *testing.T) {
		runTxStatusFinalized(t, cli, wallet, privKey, nodeAddr, systemPod, coinID, coin.Version)
	})

	t.Run("failed-version", func(t *testing.T) {
		runTxStatusFailedVersion(t, cli, privKey, nodeAddr, systemPod, coinID, staleVersion)
	})

	t.Run("total-tx-nonzero", func(t *testing.T) {
		runStatusTotalTxNonZero(t, cli)
	})
}

// runTxStatusFinalized submits a valid transfer and polls until TxStateFinalized.
func runTxStatusFinalized(t *testing.T, cli *client.Client, w *client.Wallet, privKey ed25519.PrivateKey, nodeAddr string, systemPod [32]byte, coinID [32]byte, coinVersion uint64) {
	t.Helper()

	recipient := client.NewWallet()
	txBytes, txHash := buildStatusTransferTx(privKey, systemPod, coinID, coinVersion, recipient.Pubkey())

	_, err := transportFor(nodeAddr).SubmitTx(txBytes)
	if err != nil {
		t.Fatalf("submit transfer: %v", err)
	}
	t.Logf("submitted transfer hash %x", txHash[:4])

	pollUntilState(t, nodeAddr, txHash, network.TxStateFinalized, txStatusPollAttempts)

	t.Logf("transfer finalized: %x", txHash[:4])

	// Refresh so the wallet tracks the new owner after the transfer.
	_ = w.RefreshCoin(cli, coinID)
}

// runTxStatusFailedVersion submits a transfer with a stale version and polls
// until TxStateFailed with a FailVersion reason.
func runTxStatusFailedVersion(t *testing.T, _ *client.Client, privKey ed25519.PrivateKey, nodeAddr string, systemPod [32]byte, coinID [32]byte, staleVersion uint64) {
	t.Helper()

	// Re-submit a transfer at the stale version (the first transfer already
	// consumed it), triggering FailVersion at commit. The recipient differs
	// from the finalized tx, so the two hashes are provably distinct.
	recipient := client.NewWallet()
	txBytes, txHash := buildStatusTransferTx(privKey, systemPod, coinID, staleVersion, recipient.Pubkey())

	_, err := transportFor(nodeAddr).SubmitTx(txBytes)
	if err != nil {
		t.Fatalf("stale-version tx rejected at ingress: %v", err)
	}

	t.Logf("stale-version tx submitted: %x", txHash[:4])

	// Wait longer: the stale tx may sit behind the finalized commit.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := transportFor(nodeAddr).GetTxStatus(txHash)
		if err != nil {
			t.Fatalf("GetTxStatus: %v", err)
		}

		switch resp.State {
		case network.TxStateFailed:
			if resp.Reason != uint8(consensus.FailVersion) {
				t.Errorf("stale-version tx failed with reason %d, want %d (FailVersion)", resp.Reason, uint8(consensus.FailVersion))
			} else {
				t.Logf("stale-version tx correctly failed with FailVersion")
			}
			return
		case network.TxStateFinalized:
			t.Fatalf("stale-version tx reached TxStateFinalized; expected TxStateFailed with FailVersion")
		case network.TxStateUnknown, network.TxStatePending:
			time.Sleep(txStatusPollInterval)
		}
	}

	t.Fatalf("stale-version tx %x did not reach TxStateFailed within deadline", txHash[:4])
}

// runStatusTotalTxNonZero asserts that Status.TotalTx is non-zero after commits.
func runStatusTotalTxNonZero(t *testing.T, cli *client.Client) {
	t.Helper()

	status, err := cli.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if status.TotalTx == 0 {
		t.Fatalf("Status.TotalTx = 0, expected > 0 after faucet and transfers")
	}

	t.Logf("Status.TotalTx=%d TPSMilli=%d ConnectedPeers=%d",
		status.TotalTx, status.TPSMilli, status.ConnectedPeers)
}

// pollUntilState polls GetTxStatus until the tx reaches wantState.
func pollUntilState(t *testing.T, nodeAddr string, hash [32]byte, wantState uint8, attempts int) {
	t.Helper()

	for i := 0; i < attempts; i++ {
		resp, err := transportFor(nodeAddr).GetTxStatus(hash)
		if err != nil {
			t.Fatalf("GetTxStatus: %v", err)
		}

		if resp.State == wantState {
			return
		}

		time.Sleep(txStatusPollInterval)
	}

	resp, _ := transportFor(nodeAddr).GetTxStatus(hash)
	t.Fatalf("tx %x did not reach state %d after %d polls; last state=%d reason=%d",
		hash[:4], wantState, attempts, resp.State, resp.Reason)
}

// buildStatusTransferTx builds a signed raw transfer transaction for a singleton
// coin. The coin serves as both the mutable ref and the gas coin.
// Returns the serialized transaction bytes and the 32-byte transaction hash.
func buildStatusTransferTx(privKey ed25519.PrivateKey, systemPod [32]byte, coinID [32]byte, coinVersion uint64, recipient [32]byte) ([]byte, [32]byte) {
	pubKey := privKey.Public().(ed25519.PublicKey)

	args := make([]byte, 32)
	copy(args, recipient[:])

	mutableRefs := []genesis.ObjectRefData{{ID: coinID, Version: coinVersion}}

	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(
		pubKey, systemPod, "transfer", args, nil, 0, txStatusMaxGas, coinID[:], mutableRefs, nil,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(privKey, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOffset := genesis.BuildTxTableWithRefs(
		builder, pubKey, systemPod, "transfer", args, nil, 0, txStatusMaxGas, coinID[:], hash, sig, mutableRefs, nil,
	)
	builder.Finish(txOffset)

	return builder.FinishedBytes(), hash
}
