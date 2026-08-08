package scenarios

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
	"BluePods/internal/network"
	"BluePods/internal/types"
	"BluePods/pkg/client"
	"BluePods/test/harness"
)

const (
	// consensusScenarioSize is the validator count for TestScenarioConsensusBasics.
	consensusScenarioSize = 5

	// stepTimeout bounds one scenario step (a submit plus its cluster-wide
	// commit wait). Each step carves its own context so a slow or red step
	// cannot starve the steps after it.
	stepTimeout = 90 * time.Second
)

const (
	// reparentOpKind mirrors internal/consensus's reparentOp and pkg/client's
	// reparentOpKind: DeclaredOp.kind=0 rebinds an object's parent edge. A
	// transfer is a reparent to a KeyRoot.
	reparentOpKind byte = 0

	// keyRootKind mirrors internal/consensus's keyRootKind: the reparent
	// target is an Ed25519 public key (that IS a transfer).
	keyRootKind byte = 0
)

// stepCtx returns a fresh bounded context for one scenario step.
func stepCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	t.Cleanup(cancel)

	return ctx
}

// requireVerdictAll waits until every alive node records a tx.committed
// verdict for hash (any outcome), then asserts each node's FIRST verdict
// matches wantSuccess and wantReason. On any mismatch it reports the whole
// per-node verdict map, so a red run carries the cross-node evidence (for
// example one node accepting while another rejects) instead of a bare
// timeout. It matches each node's first verdict on purpose: later verdicts
// for the same hash are commit-once duplicate skips.
func requireVerdictAll(ctx context.Context, t *testing.T, c *harness.Cluster, hash [32]byte, wantSuccess bool, wantReason string) {
	t.Helper()

	verdicts := collectVerdicts(ctx, t, c, hash)

	allMatch := true
	for _, v := range verdicts {
		if v != verdictString(wantSuccess, wantReason) {
			allMatch = false
		}
	}

	if !allMatch {
		t.Fatalf("tx %x: divergent or unexpected verdicts (want %s): %v",
			hash[:4], verdictString(wantSuccess, wantReason), verdicts)
	}
}

// collectVerdicts waits for every alive node's first tx.committed verdict for
// hash and returns them keyed by node index, failing the test if any node
// never records one within ctx.
func collectVerdicts(ctx context.Context, t *testing.T, c *harness.Cluster, hash [32]byte) map[int]string {
	t.Helper()

	txPred := harness.Attr("tx", hex.EncodeToString(hash[:]))
	verdicts := make(map[int]string)

	for _, n := range c.Alive() {
		ev, err := n.WaitEvent(ctx, "tx.committed", txPred)
		if err != nil {
			c.Dump(t)
			t.Fatalf("node %d: no tx.committed verdict for %x: %v", n.Index, hash[:4], err)
		}

		success, _ := ev.Attrs["success"].(bool)
		reason, _ := ev.Attrs["reason"].(string)
		verdicts[n.Index] = verdictString(success, reason)
	}

	return verdicts
}

// verdictString renders one commit verdict for comparison and reporting.
func verdictString(success bool, reason string) string {
	if success {
		return "success"
	}

	return "failed:" + reason
}

// TestScenarioConsensusBasics drives a 5-node cluster through client
// operations (split, transfer, replicated object create/transfer via the
// daemon's attestation path), confirms every commit lands identically on
// every node, and exercises the three commit-path security rejections:
// replay, a tampered hash, and a non-owner mutation attempt.
func TestScenarioConsensusBasics(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, consensusScenarioSize)
	node0 := c.Node(0)
	cli := c.Client(0)

	w, coinID := fundedWallet(stepCtx(t), t, cli, node0, 2_000_000)

	t.Run("split", func(t *testing.T) {
		recipient := client.NewWallet()

		newCoinID, hash, err := w.Split(cli, coinID, 200_000, recipient.Pubkey())
		requireNoErr(t, err)
		requireVerdictAll(stepCtx(t), t, c, hash, true, "")

		obj, err := cli.GetObject(newCoinID)
		requireNoErr(t, err)
		if coinBalance(obj) != 200_000 {
			t.Fatalf("split coin balance: got %d, want 200000", coinBalance(obj))
		}

		requireNoErr(t, w.RefreshCoin(cli, coinID))
	})

	t.Run("transfer", func(t *testing.T) {
		recipient := client.NewWallet()

		hash, err := w.Transfer(cli, coinID, recipient.Pubkey())
		requireNoErr(t, err)
		requireVerdictAll(stepCtx(t), t, c, hash, true, "")

		// The transfer rewrites the coin's stored body owner, so a GetObject
		// read serves the recipient, not the stale sender.
		obj, err := cli.GetObject(coinID)
		requireNoErr(t, err)
		if obj.Owner != recipient.Pubkey() {
			t.Fatalf("transferred coin owner mismatch: body owner is not the recipient")
		}
	})

	t.Run("object_create_transfer", func(t *testing.T) {
		w2, gasCoin := fundedWallet(stepCtx(t), t, cli, node0, 2_000_000)

		objID, createHash, err := w2.CreateObject(cli, 3, []byte("consensus scenario object"), gasCoin)
		requireNoErr(t, err)
		requireVerdictAll(stepCtx(t), t, c, createHash, true, "")

		recipient := client.NewWallet()

		xferHash, err := w2.TransferObject(cli, objID, recipient.Pubkey(), gasCoin)
		requireNoErr(t, err)
		requireVerdictAll(stepCtx(t), t, c, xferHash, true, "")

		obj, err := cli.GetObject(objID)
		requireNoErr(t, err)
		if obj.Owner != recipient.Pubkey() {
			t.Fatalf("transferred object owner mismatch")
		}
	})

	t.Run("merge", func(t *testing.T) {
		testMergeCombinesCoins(t, c, cli, node0)
	})

	t.Run("security_rejects", func(t *testing.T) {
		t.Run("replay_is_duplicate", func(t *testing.T) {
			testReplayRejected(t, c)
		})
		t.Run("tampered_hash_is_authenticity_failed", func(t *testing.T) {
			testTamperedHashRejected(t, c)
		})
		t.Run("wrong_owner_is_declared_ops", func(t *testing.T) {
			testWrongOwnerRejected(t, c)
		})
		t.Run("sharded_mutation_without_valid_proof_is_proof_failed", func(t *testing.T) {
			testProofFailedRejected(t, c)
		})
	})

	// Anchored last: it needs committed traffic to already exist (every
	// earlier subtest supplies that), and it queries every node's own
	// GetIndexAnchor view rather than driving new consensus behavior.
	t.Run("index_anchor_quorum", func(t *testing.T) {
		testIndexAnchorQuorum(t, c, node0)
	})
}

// testReplayRejected submits a valid, hand-built transfer once (it commits
// successfully on every node), then resubmits the identical bytes: the
// commit-once guard must mark the second occurrence a duplicate rather than
// re-applying it.
func testReplayRejected(t *testing.T, c *harness.Cluster) {
	t.Helper()

	node0 := c.Node(0)
	cli := c.Client(0)

	priv, sender := generateRawKey(t)

	coinID, faucetHash, err := cli.Faucet(sender, 1_000_000)
	requireNoErr(t, err)
	requireCommittedSuccess(stepCtx(t), t, node0, faucetHash)

	obj, err := cli.GetObject(coinID)
	requireNoErr(t, err)

	recipient := randomID(t)

	txBytes, hash := buildSignedTransferTx(priv, coinID, obj.Version, recipient)

	transport := client.NewQUICTransport(node0.QUICAddr)

	firstHash, err := transport.SubmitTx(txBytes)
	requireNoErr(t, err)
	assertHash(t, firstHash, hash)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	_, err = transport.SubmitTx(txBytes)
	requireNoErr(t, err) // ingress accepts the resubmission; commit marks it a duplicate

	// The resubmission's verdict is a SECOND tx.committed for the same hash,
	// so this wait targets the duplicate reason specifically: the first
	// (successful) verdict cannot satisfy it, and any later occurrence of the
	// same bytes must be a duplicate skip.
	dupCtx := stepCtx(t)
	dupPreds := []harness.Pred{
		harness.Attr("tx", hex.EncodeToString(hash[:])),
		harness.Attr("success", false),
		harness.Attr("reason", "duplicate"),
	}
	if err := c.WaitAll(dupCtx, "tx.committed", dupPreds...); err != nil {
		c.Dump(t)
		t.Fatalf("wait duplicate verdict for %x on every node: %v", hash[:4], err)
	}
}

// testTamperedHashRejected wraps a hash-tampered transfer in a bare ATX
// (zero proofs, as a singleton-only transaction needs none): the ATX shape
// bypasses ingress's raw-transaction hash/signature check, so the tamper
// only surfaces at commit-time authenticity verification.
func testTamperedHashRejected(t *testing.T, c *harness.Cluster) {
	t.Helper()

	node0 := c.Node(0)
	cli := c.Client(0)

	priv, sender := generateRawKey(t)

	coinID, faucetHash, err := cli.Faucet(sender, 1_000_000)
	requireNoErr(t, err)
	requireCommittedSuccess(stepCtx(t), t, node0, faucetHash)

	obj, err := cli.GetObject(coinID)
	requireNoErr(t, err)

	recipient := randomID(t)

	rawTx, tamperedHash := buildTamperedHashTransferTx(priv, coinID, obj.Version, recipient)
	atxBytes := genesis.WrapInATX(rawTx)

	transport := client.NewQUICTransport(node0.QUICAddr)

	returnedHash, err := transport.SubmitTx(atxBytes)
	requireNoErr(t, err) // accepted at ingress: the ATX shape is not re-verified there
	assertHash(t, returnedHash, tamperedHash)

	requireVerdictAll(stepCtx(t), t, c, tamperedHash, false, "authenticity_failed")
}

// testWrongOwnerRejected has an attacker submit a transfer (a declared
// reparent op) of a victim's coin, paying gas from the attacker's own
// (validly owned) coin. The transaction is fully self-consistent (the
// attacker's own valid signature), so it passes authenticity and
// fee-deduction; a declared op never reaches the generic mutable-ref
// ownership check (internal/consensus/commit.go routes declared operations
// before it) — instead staged reparent validation's own controller check
// (internal/consensus/staged.go's stagedView.controls) rejects it, which
// surfaces as the "declared_ops" commit reason.
func testWrongOwnerRejected(t *testing.T, c *harness.Cluster) {
	t.Helper()

	node0 := c.Node(0)
	cli := c.Client(0)

	_, victim := generateRawKey(t)
	attackerPriv, attacker := generateRawKey(t)

	victimCoin, victimFaucetHash, err := cli.Faucet(victim, 1_000_000)
	requireNoErr(t, err)
	requireCommittedSuccess(stepCtx(t), t, node0, victimFaucetHash)

	attackerCoin, attackerFaucetHash, err := cli.Faucet(attacker, 1_000_000)
	requireNoErr(t, err)
	requireCommittedSuccess(stepCtx(t), t, node0, attackerFaucetHash)

	victimObj, err := cli.GetObject(victimCoin)
	requireNoErr(t, err)

	newOwner := randomID(t)

	txBytes, hash := buildSignedTransferTxWithGasCoin(attackerPriv, victimCoin, victimObj.Version, attackerCoin, newOwner)

	transport := client.NewQUICTransport(node0.QUICAddr)

	returnedHash, err := transport.SubmitTx(txBytes)
	requireNoErr(t, err) // structurally valid and self-consistent; accepted at ingress
	assertHash(t, returnedHash, hash)

	requireVerdictAll(stepCtx(t), t, c, hash, false, "declared_ops")

	after, err := cli.GetObject(victimCoin)
	requireNoErr(t, err)
	if after.Owner != victim {
		t.Fatalf("non-owner transfer changed the coin's owner: got %x, want victim %x", after.Owner[:8], victim[:8])
	}
}

// generateRawKey generates a fresh Ed25519 keypair not wrapped in a
// client.Wallet, so its private key is available for hand-building raw
// transactions.
func generateRawKey(t *testing.T) (ed25519.PrivateKey, [32]byte) {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	requireNoErr(t, err)

	var pub [32]byte
	copy(pub[:], priv.Public().(ed25519.PublicKey))

	return priv, pub
}

// randomID returns 32 cryptographically random bytes, used as a throwaway
// recipient/owner identity.
func randomID(t *testing.T) [32]byte {
	t.Helper()

	var id [32]byte
	_, err := rand.Read(id[:])
	requireNoErr(t, err)

	return id
}

// assertHash fails the test if got and want differ.
func assertHash(t *testing.T, got []byte, want [32]byte) {
	t.Helper()

	var gotArr [32]byte
	copy(gotArr[:], got)

	if gotArr != want {
		t.Fatalf("hash mismatch: got %x, want %x", gotArr, want)
	}
}

// buildSignedTransferTx builds a signed transfer (a declared reparent op to a
// KeyRoot) spending coinID as its own gas coin (mirroring the wallet's
// self-funding transfer shape), signed by priv. Returns the tx bytes and hash.
func buildSignedTransferTx(priv ed25519.PrivateKey, coinID [32]byte, version uint64, newOwner [32]byte) ([]byte, [32]byte) {
	return buildSignedTransferTxWithGasCoin(priv, coinID, version, coinID, newOwner)
}

// buildSignedTransferTxWithGasCoin builds a signed transfer (a declared
// reparent op to a KeyRoot) moving coinID (at version) to newOwner, paying
// gas from gasCoin (which may be a different coin than the one transferred).
// The commit path is what enforces control of each; construction does not.
// Returns the tx bytes and hash.
func buildSignedTransferTxWithGasCoin(priv ed25519.PrivateKey, coinID [32]byte, version uint64, gasCoin, newOwner [32]byte) ([]byte, [32]byte) {
	pub := priv.Public().(ed25519.PublicKey)
	var zeroPod [32]byte
	refs := []genesis.ObjectRefData{{ID: coinID, Version: version}}
	ops := []genesis.DeclaredOp{{Kind: reparentOpKind, ObjectID: coinID[:], TargetKind: keyRootKind, Target: newOwner[:]}}

	unsigned := genesis.BuildUnsignedTxBytesSponsored(pub, zeroPod, "", nil, nil, 1000, gasCoin[:], refs, nil, genesis.Sponsorship{}, nil, ops)
	hash := blake3.Sum256(unsigned)
	sig := ed25519.Sign(priv, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableSponsored(builder, pub, zeroPod, "", nil, nil, 1000, gasCoin[:], hash, sig, refs, nil, genesis.Sponsorship{}, nil, nil, ops)
	builder.Finish(txOff)

	return builder.FinishedBytes(), hash
}

// buildTamperedHashTransferTx builds a transfer (a declared reparent op)
// identical to buildSignedTransferTx, but with its declared hash corrupted
// after signing: the signature verifies against the ORIGINAL hash, not the
// declared, corrupted one. Returns the tx bytes and the corrupted (declared)
// hash, which is what tx.committed reports for it.
func buildTamperedHashTransferTx(priv ed25519.PrivateKey, coinID [32]byte, version uint64, newOwner [32]byte) ([]byte, [32]byte) {
	pub := priv.Public().(ed25519.PublicKey)
	var zeroPod [32]byte
	refs := []genesis.ObjectRefData{{ID: coinID, Version: version}}
	ops := []genesis.DeclaredOp{{Kind: reparentOpKind, ObjectID: coinID[:], TargetKind: keyRootKind, Target: newOwner[:]}}

	unsigned := genesis.BuildUnsignedTxBytesSponsored(pub, zeroPod, "", nil, nil, 1000, coinID[:], refs, nil, genesis.Sponsorship{}, nil, ops)
	hash := blake3.Sum256(unsigned)
	sig := ed25519.Sign(priv, hash[:])

	bad := hash
	bad[0] ^= 0xFF

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableSponsored(builder, pub, zeroPod, "", nil, nil, 1000, coinID[:], bad, sig, refs, nil, genesis.Sponsorship{}, nil, nil, ops)
	builder.Finish(txOff)

	return builder.FinishedBytes(), bad
}

// testMergeCombinesCoins splits a funded coin into two coins the same wallet
// owns, then merges the split-off source back into the original destination:
// every node must agree on success, the destination balance must equal the
// pre-merge sum of both coins minus the merge's OWN fee (the destination coin
// also pays the merge's gas, exactly like testFullDeduction's split), and the
// source coin must read back at a zero balance everywhere rather than being
// deleted (pods/pod-system/src/functions/merge/execute.rs empties every
// source coin via with_updated, never a deletion) — checked with
// GetObjectLocal on every node rather than a routed GetObject: a routed
// GetObject can cascade to a client timeout for an object no node holds,
// which is not this case (the coin is a singleton every node still holds
// after merge), but the local-only read is both cheaper and immune to that
// failure mode regardless.
func testMergeCombinesCoins(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node) {
	t.Helper()

	const funding, splitAmount = uint64(1_000_000), uint64(400_000)

	w, destCoin := fundedWallet(stepCtx(t), t, cli, node0, funding)

	sourceCoin, splitHash, err := w.Split(cli, destCoin, splitAmount, w.Pubkey())
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, splitHash, true, "")

	requireNoErr(t, w.RefreshCoin(cli, destCoin))
	requireNoErr(t, w.RefreshCoin(cli, sourceCoin))

	destBefore := w.GetCoin(destCoin).Balance
	sourceBefore := w.GetCoin(sourceCoin).Balance

	mergeHash, err := w.Merge(cli, destCoin, w.GetCoin(destCoin).Version, []*client.CoinInfo{w.GetCoin(sourceCoin)})
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, mergeHash, true, "")

	ev, err := node0.WaitEvent(stepCtx(t), "fees.deducted",
		harness.Attr("tx", hex.EncodeToString(mergeHash[:])), harness.Attr("covered", true))
	requireNoErr(t, err)

	fee, ok := ev.Attrs["amount"].(float64)
	if !ok || fee <= 0 {
		t.Fatalf("fees.deducted carries no positive amount: %v", ev.Attrs)
	}

	requireNoErr(t, w.RefreshCoin(cli, destCoin))
	if got, want := w.GetCoin(destCoin).Balance, destBefore+sourceBefore-uint64(fee); got != want {
		t.Fatalf("merged destination balance: got %d, want %d (dest %d + source %d - fee %d)",
			got, want, destBefore, sourceBefore, uint64(fee))
	}

	requireCoinBalanceEverywhere(t, c, sourceCoin, 0)
}

// requireCoinBalanceEverywhere asserts every alive node's LOCAL copy of a
// singleton coin carries exactly want as its balance.
func requireCoinBalanceEverywhere(t *testing.T, c *harness.Cluster, coinID [32]byte, want uint64) {
	t.Helper()

	for _, n := range c.Alive() {
		data, err := client.NewQUICTransport(n.QUICAddr).GetObjectLocal(coinID)
		requireNoErr(t, err)
		if data == nil {
			t.Fatalf("node %d: coin %x missing locally", n.Index, coinID[:4])
		}

		if got := coinBalance(client.ParseObject(data)); got != want {
			t.Fatalf("node %d: coin %x balance: got %d, want %d", n.Index, coinID[:4], got, want)
		}
	}
}

// testProofFailedRejected mutates a real sharded (replication-3) object,
// created through the ordinary CreateObject path, via a hand-built ATX that
// carries exactly one QuorumProof but an EMPTY Objects vector. The
// commit-time proof gate (internal/aggregation's ATXVerifier.prepareSingleProof,
// invoked through internal/consensus/commit.go's proofVerdict) looks the
// proof's referenced object up in the ATX's OWN Objects vector before it ever
// touches BLS cryptography or any node-local state; with that vector empty
// the lookup fails identically everywhere ("object not found in ATX"), so
// every alive node derives the SAME proof_failed verdict from the gossiped
// bytes alone. This proof gate reads only the ATX's own bytes and runs BEFORE
// the mutable-ref ownership check, so its verdict is uniform by construction —
// there is no node-local state to consult and no holder/non-holder split.
//
// This is the deterministic external analogue of "no valid attestation
// proof": a legitimate BLS quorum signature cannot be forged from outside the
// validator set, but an ATX that claims a proof for an object it never
// attaches is exactly what proofVerdict must reject, and it is reachable
// without any cooperation from consensus internals — the reason this is
// written as a scenario rather than left undone.
func testProofFailedRejected(t *testing.T, c *harness.Cluster) {
	t.Helper()

	node0 := c.Node(0)
	cli := c.Client(0)

	priv, sender := generateRawKey(t)
	w := client.NewWalletFromKey(priv)

	gasCoin, faucetHash, err := cli.Faucet(sender, 1_000_000)
	requireNoErr(t, err)
	requireCommittedSuccess(stepCtx(t), t, node0, faucetHash)
	requireNoErr(t, w.RefreshCoin(cli, gasCoin))

	objID, createHash, err := w.CreateObject(cli, 3, []byte("proof-failed-target"), gasCoin)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, createHash, true, "")
	waitHolders(stepCtx(t), t, c, objID, 3)

	obj, err := cli.GetObject(objID)
	requireNoErr(t, err)

	status, err := cli.Status()
	requireNoErr(t, err)

	rawTx, hash := buildSignedTransferObjectTx(priv, objID, obj.Version, gasCoin, randomID(t))
	atxBytes := buildATXWithBogusProof(rawTx, objID, status.Epoch)

	transport := client.NewQUICTransport(node0.QUICAddr)
	returnedHash, err := transport.SubmitTx(atxBytes)
	requireNoErr(t, err) // structurally valid ATX; the proof is only checked at commit
	assertHash(t, returnedHash, hash)

	requireVerdictAll(stepCtx(t), t, c, hash, false, "proof_failed")
}

// buildSignedTransferObjectTx builds a signed transfer_object (a declared
// reparent op to a KeyRoot) moving objID (at version) to newOwner, paying gas
// from a separately owned gasCoin (mirroring TransferObject's shape: the
// mutated object is not itself a coin, so gas comes from a distinct owned
// singleton). Returns the tx bytes and hash.
func buildSignedTransferObjectTx(priv ed25519.PrivateKey, objID [32]byte, version uint64, gasCoin, newOwner [32]byte) ([]byte, [32]byte) {
	pub := priv.Public().(ed25519.PublicKey)
	var zeroPod [32]byte
	refs := []genesis.ObjectRefData{{ID: objID, Version: version}}
	ops := []genesis.DeclaredOp{{Kind: reparentOpKind, ObjectID: objID[:], TargetKind: keyRootKind, Target: newOwner[:]}}

	unsigned := genesis.BuildUnsignedTxBytesSponsored(pub, zeroPod, "", nil, nil, 1000, gasCoin[:], refs, nil, genesis.Sponsorship{}, nil, ops)
	hash := blake3.Sum256(unsigned)
	sig := ed25519.Sign(priv, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableSponsored(builder, pub, zeroPod, "", nil, nil, 1000, gasCoin[:], hash, sig, refs, nil, genesis.Sponsorship{}, nil, nil, ops)
	builder.Finish(txOff)

	return builder.FinishedBytes(), hash
}

// buildATXWithBogusProof wraps rawTx in a hand-built AttestedTransaction that
// carries exactly one QuorumProof naming objID but an EMPTY Objects vector,
// so the proof's object lookup fails deterministically for every node
// ("object not found in ATX") without any BLS material. attestationEpoch is
// stamped from the caller's live epoch read so the epoch-boundary check
// (ATXVerifier.resolveHolders) never itself rejects the ATX first and masks
// the proof failure this test targets.
func buildATXWithBogusProof(rawTx []byte, objID [32]byte, attestationEpoch uint64) []byte {
	tx := types.GetRootAsTransaction(rawTx, 0)

	builder := flatbuffers.NewBuilder(len(rawTx) + 512)
	txOffset := genesis.RebuildTxInBuilder(builder, tx)

	types.AttestedTransactionStartObjectsVector(builder, 0)
	objectsVec := builder.EndVector(0)

	objIDVec := builder.CreateByteVector(objID[:])
	sigVec := builder.CreateByteVector([]byte{0xDE, 0xAD, 0xBE, 0xEF})
	bitmapVec := builder.CreateByteVector([]byte{0xFF})

	types.QuorumProofStart(builder)
	types.QuorumProofAddObjectId(builder, objIDVec)
	types.QuorumProofAddBlsSignature(builder, sigVec)
	types.QuorumProofAddSignerBitmap(builder, bitmapVec)
	proofOff := types.QuorumProofEnd(builder)

	types.AttestedTransactionStartProofsVector(builder, 1)
	builder.PrependUOffsetT(proofOff)
	proofsVec := builder.EndVector(1)

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOffset)
	types.AttestedTransactionAddObjects(builder, objectsVec)
	types.AttestedTransactionAddProofs(builder, proofsVec)
	types.AttestedTransactionAddAttestationEpoch(builder, attestationEpoch)
	atxOffset := types.AttestedTransactionEnd(builder)

	builder.Finish(atxOffset)

	return builder.FinishedBytes()
}

const (
	// indexAnchorHeaderTag is the domain-separation byte a light client
	// prefixes to a raw header before hashing it, mirroring
	// internal/consensus/header.go's headerDomainTag: a header's signature is
	// taken over BLAKE3(indexAnchorHeaderTag || header), never over the
	// header bytes alone.
	indexAnchorHeaderTag = 0x01

	// indexAnchorHeaderBytes is the width of the normative vertex header
	// inside a GetIndexAnchor record (internal/consensus/header.go's
	// headerSize), before the trailing Ed25519 signature.
	indexAnchorHeaderBytes = 120
)

// testIndexAnchorQuorum drives one more commit (so every node's committed
// frontier is freshly advanced) and then calls GetIndexAnchor on every alive
// node: the light-client surface a peer outside consensus uses to verify the
// index root without downloading the index itself.
//
// "Agree" here does not mean every node reports the identical frontier —
// each node serves its own committed frontier's quorum window
// (internal/consensus/anchorbundle.go's bundleWindow), and ordinary round
// skew across 5 producers can leave two nodes a few rounds apart. What must
// hold, and is load-bearing, is narrower: for any two nodes whose bundles
// name the SAME frontier, the index root they report for it must be
// identical — two honest nodes deriving the same tree from the same
// committed history can never disagree on its root at a shared frontier.
func testIndexAnchorQuorum(t *testing.T, c *harness.Cluster, node0 *harness.Node) {
	t.Helper()

	cli := c.Client(0)

	w, coinID := fundedWallet(stepCtx(t), t, cli, node0, 1_000_000)
	recipient := client.NewWallet()

	hash, err := w.Transfer(cli, coinID, recipient.Pubkey())
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	bundles := make(map[int]*network.GetIndexAnchorResponse)
	for _, n := range c.Alive() {
		bundle, err := c.Client(n.Index).GetIndexAnchor()
		requireNoErr(t, err)
		if !bundle.Found {
			t.Fatalf("node %d: no quorate index anchor bundle", n.Index)
		}
		if len(bundle.Headers) == 0 {
			t.Fatalf("node %d: index anchor bundle carries no headers", n.Index)
		}
		bundles[n.Index] = bundle
	}

	rootsByFrontier := make(map[uint64][32]byte)
	for idx, bundle := range bundles {
		prevRoot, seen := rootsByFrontier[bundle.FrontierRound]
		if !seen {
			rootsByFrontier[bundle.FrontierRound] = bundle.IndexRoot
			continue
		}

		if prevRoot != bundle.IndexRoot {
			t.Fatalf("frontier %d: index root disagreement, node %d reports %x, an earlier node reports %x",
				bundle.FrontierRound, idx, bundle.IndexRoot[:4], prevRoot[:4])
		}
	}

	for idx, bundle := range bundles {
		for _, record := range bundle.Headers {
			verifyIndexAnchorHeaderRecord(t, idx, bundle, record)
		}
	}
}

// verifyIndexAnchorHeaderRecord decodes one GetIndexAnchor header record the
// way a light client must: positionally, per the as-built wire layout
// (producer@0:32, round@32:40, epoch@40:48, frontier@48:56, root@56:88,
// bodyHash@88:120, signature@120:184), with no length prefix and no
// FlatBuffers involved. It asserts the header's own frontier and root match
// the bundle it was served in, and that the Ed25519 signature verifies over
// the tagged header hash under the producer key the record itself carries —
// mirroring internal/consensus/anchorbundle_test.go's verifyHeaderRecord from
// outside the consensus package. No external key source is needed: the
// producer key is self-describing in every record.
func verifyIndexAnchorHeaderRecord(t *testing.T, nodeIdx int, bundle *network.GetIndexAnchorResponse, record []byte) {
	t.Helper()

	if len(record) != network.IndexAnchorHeaderSize {
		t.Fatalf("node %d: header record is %d bytes, want %d", nodeIdx, len(record), network.IndexAnchorHeaderSize)
	}

	header := record[:indexAnchorHeaderBytes]
	signature := record[indexAnchorHeaderBytes:]

	producer := header[0:32]
	frontier := binary.BigEndian.Uint64(header[48:56])

	var root [32]byte
	copy(root[:], header[56:88])

	if frontier != bundle.FrontierRound {
		t.Fatalf("node %d: header frontier %d does not match bundle frontier %d", nodeIdx, frontier, bundle.FrontierRound)
	}
	if root != bundle.IndexRoot {
		t.Fatalf("node %d: header root %x does not match bundle root %x", nodeIdx, root[:4], bundle.IndexRoot[:4])
	}

	identity := blake3.Sum256(append([]byte{indexAnchorHeaderTag}, header...))
	if !ed25519.Verify(producer, identity[:], signature) {
		t.Fatalf("node %d: header signature does not verify for producer %x", nodeIdx, producer[:4])
	}
}
