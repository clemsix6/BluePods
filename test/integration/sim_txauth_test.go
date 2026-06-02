package integration

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"BluePods/client"
)

const (
	// txauthFaucetAmount is the faucet amount for the forged-tx security test.
	txauthFaucetAmount = 1_000_000

	// txauthTxWait is how long to wait for a forged tx to (fail to) commit.
	txauthTxWait = 10 * time.Second
)

// TestSimForgedTxAuth runs a 5-node simulation that submits a FORGED transfer
// transaction naming the victim as sender, with the victim's coin as gas_coin
// and mutable_ref, signed by an attacker key (an invalid signature). It asserts
// the transaction does NOT commit and the victim's coin is NOT stolen.
//
// This is the regression test for the commit-time authenticity enforcement bug:
// before the fix, the inner-tx signature check was wired through a nil-able field
// that production never set, so a forged tx reaching commit via gossip could
// reassign the victim's coin. With authenticity enforced unconditionally at
// commit on every node, the forged tx is rejected and the owner is unchanged.
func TestSimForgedTxAuth(t *testing.T) {
	cluster := NewCluster(t, 5, WithHTTPBase(18900), WithQUICBase(18900+920))
	cluster.WaitReady(60 * time.Second)

	cli := cluster.Client(0)
	addr := cluster.Bootstrap().Addr()

	t.Run("forged sender tx does not steal victim coin", func(t *testing.T) {
		// Victim funds a real, signed coin via the faucet.
		victim := client.NewWallet()
		coinID := FaucetAndWait(t, cli, victim, txauthFaucetAmount, 30*time.Second)

		obj, err := cli.GetObject(coinID)
		if err != nil {
			t.Fatalf("get victim coin: %v", err)
		}

		victimPK := victim.Pubkey()
		if obj.Owner != victimPK {
			t.Fatalf("setup: victim should own coin, got %s",
				hex.EncodeToString(obj.Owner[:8]))
		}

		// Attacker forges a transfer: sender=victim, gas_coin/mutable_ref=victim's
		// coin, signed with the attacker's key (invalid signature for the victim).
		_, attacker, _ := ed25519.GenerateKey(rand.Reader)
		thief := client.NewWallet()
		thiefPK := thief.Pubkey()

		forged := BuildForgedSenderTx(
			cli.SystemPod(), victimPK, attacker, coinID, obj.Version, thiefPK,
		)

		// Submit the forged ATX. Ingress may reject it (400) or accept it (202);
		// either way the committing node must never reassign the coin.
		code, msg := SubmitRawBytes(addr, forged)
		t.Logf("forged tx submission: code=%d msg=%s", code, msg)

		time.Sleep(txauthTxWait)

		// The victim must still own the coin: the forged tx is not committed.
		after, err := cli.GetObject(coinID)
		if err != nil {
			t.Fatalf("get victim coin after attack: %v", err)
		}

		if after.Owner != victimPK {
			t.Fatalf("THEFT: forged tx changed owner to %s, want victim %s",
				hex.EncodeToString(after.Owner[:8]), hex.EncodeToString(victimPK[:8]))
		}

		if after.Owner == thiefPK {
			t.Fatalf("THEFT: forged tx transferred coin to attacker %s",
				hex.EncodeToString(thiefPK[:8]))
		}
	})
}
