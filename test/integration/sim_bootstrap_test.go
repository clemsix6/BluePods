package integration

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"BluePods/pkg/client"
)

// TestSimBootstrap runs a single-node bootstrap simulation.
// Tests transaction validation, API endpoints, and basic pod operations.
func TestSimBootstrap(t *testing.T) {
	cluster := NewCluster(t, 1, WithHTTPBase(18100), WithQUICBase(18100+920))
	cluster.WaitReady(30 * time.Second)

	addr := cluster.Bootstrap().Addr()
	cli := cluster.Client(0)
	systemPod := cli.SystemPod()

	t.Run("tx-validation", func(t *testing.T) {
		runTxFieldSizeTests(t, addr, systemPod)
		runTxContentTests(t, addr, systemPod)
		runTxRefTests(t, addr, systemPod)
		runTxEdgeCaseTests(t, addr, systemPod)
	})

	t.Run("api-endpoints", func(t *testing.T) {
		runAPITests(t, addr, cli, systemPod)
	})

	t.Run("faucet-endpoints", func(t *testing.T) {
		runFaucetTests(t, addr, cli)
	})

	t.Run("bootstrap-dag", func(t *testing.T) {
		runBootstrapDAGTests(t, cluster)
	})

	t.Run("pod-vm", func(t *testing.T) {
		runPodVMTests(t, addr, cli)
	})
}

// runTxFieldSizeTests tests that fields with wrong sizes are rejected.
func runTxFieldSizeTests(t *testing.T, addr string, systemPod [32]byte) {
	t.Helper()

	t.Run("ATP-1.1: hash must be 32 bytes", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithFieldSize(systemPod, "hash", 16))
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.2: sender must be 32 bytes", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithFieldSize(systemPod, "sender", 16))
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.3: signature must be 64 bytes", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithFieldSize(systemPod, "signature", 32))
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.4: pod must be 32 bytes", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithFieldSize(systemPod, "pod", 16))
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.5: function name must be non-empty", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithEmptyFuncName(systemPod))
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.6: gas_coin must be 0 or 32 bytes", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithFieldSize(systemPod, "gas_coin", 16))
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})
}

// runTxContentTests tests that content-level validation works.
func runTxContentTests(t *testing.T, addr string, systemPod [32]byte) {
	t.Helper()

	t.Run("ATP-1.7: hash mismatch rejected", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithBadHash(systemPod))
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.9: invalid signature rejected", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithBadSig(systemPod))
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.10: wrong sender key rejected", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithWrongSender(systemPod))
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.22: valid tx returns 202", func(t *testing.T) {
		w := client.NewWallet()
		pk := w.Pubkey()

		// The faucet is the canonical valid-tx path: it splits balance from the
		// genesis reserve coin (there is no longer a user-callable mint).
		code, _ := SubmitFaucet(addr, hex.EncodeToString(pk[:]), 100)
		if code != statusAccepted {
			t.Errorf("expected 202, got %d", code)
		}
	})
}

// runTxRefTests tests object reference validation.
func runTxRefTests(t *testing.T, addr string, systemPod [32]byte) {
	t.Helper()

	t.Run("ATP-1.11: too many refs rejected", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithTooManyRefs(systemPod))
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.12: duplicate read_refs rejected", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithDuplicateRefs(systemPod, false, true))
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.13: duplicate mutable_refs rejected", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithDuplicateRefs(systemPod, true, false))
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.14: cross-list duplicate rejected", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithDuplicateRefs(systemPod, true, true))
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.15: duplicate domain refs rejected", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithDuplicateDomainRefs(systemPod, "test.bp"))
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.16: invalid ref ID size rejected", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithBadRefIDSize(systemPod, 16))
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.17: domain ref with no ID accepted", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithDomainRef(systemPod, "test.bp"))
		// Should not be rejected at validation layer (400)
		// May fail at aggregation (500) or succeed (202)
		if code == statusBadRequest {
			t.Errorf("domain ref should not be rejected by validation, got 400")
		}
	})
}

// runTxEdgeCaseTests tests edge cases in transaction submission.
func runTxEdgeCaseTests(t *testing.T, addr string, systemPod [32]byte) {
	t.Helper()

	t.Run("ATP-1.18: FlatBuffers panic recovery", func(t *testing.T) {
		garbage := make([]byte, 100)
		rand.Read(garbage)

		code, _ := SubmitRawBytes(addr, garbage)
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.19: data too short rejected", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, []byte{0x01, 0x02, 0x03, 0x04})
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.20: body over 1MB rejected", func(t *testing.T) {
		bigData := make([]byte, 1_100_000)
		rand.Read(bigData)

		code, _ := SubmitRawBytes(addr, bigData)
		// Server truncates to 1MB via LimitReader, validates truncated data → fails
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.21: empty body rejected", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, []byte{})
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})
}

// runAPITests tests the QUIC operational and query messages.
func runAPITests(t *testing.T, addr string, cli *client.Client, systemPod [32]byte) {
	t.Helper()

	t.Run("ATP-1.23: health probe", func(t *testing.T) {
		result := QueryHealth(t, addr)
		if result["status"] != "ok" {
			t.Errorf("expected status=ok, got %q", result["status"])
		}
	})

	t.Run("ATP-1.24: status fields", func(t *testing.T) {
		status := QueryStatus(t, addr)

		if status.Validators < 1 {
			t.Errorf("expected >= 1 validator, got %d", status.Validators)
		}

		if status.SystemPod == "" {
			t.Error("systemPod field is empty")
		}
	})

	t.Run("ATP-1.25: validators message", func(t *testing.T) {
		validators := QueryValidators(t, addr)
		if len(validators) != 1 {
			t.Errorf("expected 1 validator, got %d", len(validators))
		}
	})

	t.Run("ATP-1.26: get object found", func(t *testing.T) {
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, 1000, 15*time.Second)

		obj := QueryObject(t, addr, coinID)
		if obj == nil {
			t.Fatal("object not found")
		}

		if obj.ID == "" || obj.Owner == "" {
			t.Error("missing required fields in object response")
		}
	})

	t.Run("ATP-1.28: get object not found", func(t *testing.T) {
		var randomID [32]byte
		rand.Read(randomID[:])

		obj := QueryObject(t, addr, randomID)
		if obj != nil {
			t.Error("expected nil for non-existent object")
		}
	})

	t.Run("ATP-1.29: get object local-only", func(t *testing.T) {
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, 1000, 15*time.Second)

		// Singletons (rep=0) are stored by all validators, so local-only succeeds.
		obj := QueryObjectLocal(t, addr, coinID)
		if obj == nil {
			t.Fatal("object not found with local-only")
		}
	})

	t.Run("ATP-1.31: domain resolve not found", func(t *testing.T) {
		_, found := QueryDomain(t, addr, "nonexistent.bp")
		if found {
			t.Error("expected not-found for non-existent domain")
		}
	})

	t.Run("ATP-1.36: systemPod in status", func(t *testing.T) {
		status := QueryStatus(t, addr)

		if len(status.SystemPod) != 64 {
			t.Errorf("systemPod should be 64 hex chars, got %d", len(status.SystemPod))
		}

		expectedHex := hex.EncodeToString(systemPod[:])
		if status.SystemPod != expectedHex {
			t.Errorf("systemPod mismatch: got %s, want %s", status.SystemPod[:16], expectedHex[:16])
		}
	})
}

// runFaucetTests tests the QUIC faucet message.
func runFaucetTests(t *testing.T, addr string, cli *client.Client) {
	t.Helper()

	t.Run("ATP-1.32: faucet valid", func(t *testing.T) {
		w := client.NewWallet()
		pk := w.Pubkey()

		code, body := SubmitFaucet(addr, hex.EncodeToString(pk[:]), 1000)
		if code != statusAccepted {
			t.Errorf("expected 202, got %d: %s", code, body)
		}
	})

	t.Run("ATP-1.33: faucet amount=0", func(t *testing.T) {
		w := client.NewWallet()
		pk := w.Pubkey()

		code, _ := SubmitFaucet(addr, hex.EncodeToString(pk[:]), 0)
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-1.34: faucet invalid pubkey", func(t *testing.T) {
		code, _ := SubmitFaucet(addr, "abcd", 1000)
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})
}

// runBootstrapDAGTests tests DAG behavior on a single bootstrap node.
func runBootstrapDAGTests(t *testing.T, cluster *Cluster) {
	t.Helper()

	t.Run("ATP-2.12: bootstrap produces alone", func(t *testing.T) {
		// Wait a bit for vertices to accumulate
		time.Sleep(10 * time.Second)

		count := cluster.Bootstrap().LogCount("produced vertex")
		if count == 0 {
			t.Error("bootstrap should produce vertices when alone")
		}

		t.Logf("Bootstrap produced %d vertices", count)
	})
}

// runPodVMTests tests system pod operations.
func runPodVMTests(t *testing.T, addr string, cli *client.Client) {
	t.Helper()

	t.Run("ATP-16.1: mint success", func(t *testing.T) {
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, 50_000, 15*time.Second)

		obj, err := cli.GetObject(coinID)
		if err != nil {
			t.Fatalf("get minted coin: %v", err)
		}

		balance := ReadBalance(obj.Content)
		if balance != 50_000 {
			t.Errorf("balance: got %d, want 50000", balance)
		}
	})

	t.Run("ATP-16.2: mint zero amount", func(t *testing.T) {
		w := client.NewWallet()
		pk := w.Pubkey()

		// Faucet with amount=0 should be rejected at the ingress layer.
		code, _ := SubmitFaucet(addr, hex.EncodeToString(pk[:]), 0)
		if code != statusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-16.3: faucet large amount", func(t *testing.T) {
		// A large faucet split must stay within the genesis reserve
		// (InitialMint - GenesisStake = 900,000,000 at the default mint). The old
		// 1e12 amount exceeded the reserve and could never commit; cap it to a
		// large amount the reserve can fund.
		const largeAmount uint64 = 500_000_000

		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, largeAmount, 15*time.Second)

		obj, err := cli.GetObject(coinID)
		if err != nil {
			t.Fatalf("get large coin: %v", err)
		}

		balance := ReadBalance(obj.Content)
		if balance != largeAmount {
			t.Errorf("balance: got %d, want %d", balance, largeAmount)
		}
	})
}
