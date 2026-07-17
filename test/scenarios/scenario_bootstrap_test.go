package scenarios

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
	"BluePods/pkg/client"
	"BluePods/test/harness"
)

// TestScenarioBootstrap drives a single-node cluster through the basic
// client operations (faucet, split, transfer, create/get object) and proves
// that malformed raw submissions are rejected at ingress, before ever
// reaching consensus.
func TestScenarioBootstrap(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, 1)
	node := c.Node(0)
	cli := c.Client(0)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	w, coinID := fundedWallet(ctx, t, cli, node, 1_000_000)

	t.Run("split", func(t *testing.T) {
		recipient := client.NewWallet()

		newCoinID, hash, err := w.Split(cli, coinID, 100_000, recipient.Pubkey())
		requireNoErr(t, err)
		requireCommittedSuccess(ctx, t, node, hash)
		requireTxStatusMatchesCommit(t, cli, hash, true)

		obj, err := cli.GetObject(newCoinID)
		requireNoErr(t, err)
		if coinBalance(obj) != 100_000 {
			t.Fatalf("split coin balance: got %d, want 100000", coinBalance(obj))
		}
		if obj.Owner != recipient.Pubkey() {
			t.Fatalf("split coin owner mismatch")
		}

		requireNoErr(t, w.RefreshCoin(cli, coinID))
	})

	t.Run("create_object", func(t *testing.T) {
		content := []byte("bootstrap scenario object")

		objectID, hash, err := w.CreateObject(cli, 0, content, coinID)
		requireNoErr(t, err)
		requireCommittedSuccess(ctx, t, node, hash)

		obj, err := cli.GetObject(objectID)
		requireNoErr(t, err)
		if obj.Owner != w.Pubkey() {
			t.Fatalf("created object owner mismatch")
		}
		// The system pod stores create_object's metadata as a u32-length-prefixed
		// blob (mirroring the argument encoding), not the raw bytes verbatim.
		if !bytes.HasSuffix(obj.Content, content) {
			t.Fatalf("created object content: got %q, want a suffix of %q", obj.Content, content)
		}

		requireNoErr(t, w.RefreshCoin(cli, coinID))
	})

	t.Run("transfer", func(t *testing.T) {
		recipient := client.NewWallet()

		hash, err := w.Transfer(cli, coinID, recipient.Pubkey())
		requireNoErr(t, err)
		requireCommittedSuccess(ctx, t, node, hash)

		obj, err := cli.GetObject(coinID)
		requireNoErr(t, err)
		if obj.Owner != recipient.Pubkey() {
			t.Fatalf("transfer owner mismatch")
		}
	})

	t.Run("ingress_rejects", func(t *testing.T) {
		testIngressRejects(ctx, t, node, cli.SystemPod())
	})
}

// testIngressRejects submits structurally malformed raw transactions
// directly (bypassing the wallet) and asserts each is rejected before
// reaching consensus: an ingress.tx.rejected event with the typed reason,
// and no tx.committed ever recorded for it (there is no valid hash to wait
// on, so the proof is the absence of consensus involvement plus the
// synchronous submission error).
func testIngressRejects(ctx context.Context, t *testing.T, node *harness.Node, systemPod [32]byte) {
	t.Helper()

	transport := client.NewQUICTransport(node.QUICAddr)

	cases := map[string][]byte{
		"bad_hash":  buildMalformedHashTx(systemPod),
		"bad_sig":   buildMalformedSigTx(systemPod),
		"garbage":   garbageBytes(64),
		"empty":     {},
		"too_short": {0x01, 0x02, 0x03},
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			before := len(node.Journal().Events("ingress.tx.rejected", harness.Attr("reason", "invalid_submission")))

			if _, err := transport.SubmitTx(body); err == nil {
				t.Fatalf("expected %s submission to be rejected", name)
			}

			rejectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			waitEventCount(rejectCtx, t, node, "ingress.tx.rejected", before+1, harness.Attr("reason", "invalid_submission"))
		})
	}
}

// buildMalformedHashTx returns a structurally well-formed, correctly signed
// raw Transaction whose declared hash has been corrupted after signing: the
// signature verifies against the ORIGINAL hash, not the declared one, so
// ingress-level hash verification (validation.ValidateTx) rejects it.
func buildMalformedHashTx(systemPod [32]byte) []byte {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	pub := priv.Public().(ed25519.PublicKey)

	unsigned := genesis.BuildUnsignedTxBytesWithRefs(pub, systemPod, "noop", nil, nil, 0, 0, nil, nil, nil)
	hash := blake3.Sum256(unsigned)
	sig := ed25519.Sign(priv, hash[:])

	bad := hash
	bad[0] ^= 0xFF

	builder := flatbuffers.NewBuilder(256)
	txOff := genesis.BuildTxTableWithRefs(builder, pub, systemPod, "noop", nil, nil, 0, 0, nil, bad, sig, nil, nil)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// buildMalformedSigTx returns a structurally well-formed raw Transaction with
// a correct hash but a corrupted signature.
func buildMalformedSigTx(systemPod [32]byte) []byte {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	pub := priv.Public().(ed25519.PublicKey)

	unsigned := genesis.BuildUnsignedTxBytesWithRefs(pub, systemPod, "noop", nil, nil, 0, 0, nil, nil, nil)
	hash := blake3.Sum256(unsigned)
	sig := ed25519.Sign(priv, hash[:])
	sig[0] ^= 0xFF

	builder := flatbuffers.NewBuilder(256)
	txOff := genesis.BuildTxTableWithRefs(builder, pub, systemPod, "noop", nil, nil, 0, 0, nil, hash, sig, nil, nil)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// garbageBytes returns n cryptographically random bytes: not a valid
// FlatBuffer under any schema.
func garbageBytes(n int) []byte {
	buf := make([]byte, n)
	rand.Read(buf) //nolint:errcheck // crypto/rand.Read never errors on a plain []byte

	return buf
}
