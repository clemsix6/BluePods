package consensus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/genesis"
	"BluePods/internal/logger"
	"BluePods/internal/types"
)

const (
	// commitCheckInterval is how often to check for new commits.
	commitCheckInterval = 50 * time.Millisecond
)

// commitLoop runs in background and detects committed vertices.
func (d *DAG) commitLoop() {
	defer d.wg.Done()

	ticker := time.NewTicker(commitCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-d.stop:
			return
		case <-ticker.C:
			d.checkCommits()
		}
	}
}

// checkCommits advances the commit cursor round by round. Each round is decided by
// the anchor rule (commit, skip, or wait); commit batches are applied in
// deterministic order, and the loop stops at the first round that cannot yet be
// decided. Replaces the arrival-order round scan so every node commits the identical
// ordered log regardless of the order vertices arrived in.
func (d *DAG) checkCommits() {
	d.commitMu.Lock()
	defer d.commitMu.Unlock()

	for d.commitNextRound() {
	}
}

// commitNextRound decides the round at the commit cursor and advances past it. It
// returns true when the cursor advanced (so the loop continues) and false to stop:
// on WAIT (not yet decidable) or on a COMMIT whose anchor ancestry is still in
// flight. SKIP and an applied COMMIT both advance the cursor, so lastCommitted moves
// forward on every decision.
func (d *DAG) commitNextRound() bool {
	round := d.lastCommitted
	status := d.anchorStatus(round)

	if status.kind == anchorWait {
		d.clearStall()
		return false
	}

	if status.kind == anchorCommit && !d.commitAnchorBatch(round, status.anchor) {
		// Anchor decided, but its causal history is not fully local. Gossip and the
		// pending buffer retry passively; after two consecutive stalled ticks, ask
		// mesh peers for the missing ancestry.
		d.onCausalStall(status.anchor)
		return false
	}

	d.clearStall()
	d.advanceCommitCursor(round)
	return true
}

// onCausalStall records that the commit cursor's decided anchor is blocked on absent
// causal history for another tick, and after two consecutive stalled ticks asks mesh
// peers for the missing ancestry. The two-tick delay first gives gossip and the
// pending buffer (a passive retry queue) a chance to deliver. It keeps re-requesting
// each stalled tick thereafter; the fetcher's in-flight dedup bounds that to one
// outstanding request per hash, so it is a retry, not a storm. Runs under commitMu.
// I6 (deferred, out of scope): a gap on an UNDECIDED round has no trigger here.
func (d *DAG) onCausalStall(anchor Hash) {
	if d.stallAnchor != anchor {
		d.stallAnchor = anchor
		d.stallTicks = 1
		return
	}

	d.stallTicks++
	if d.stallTicks >= 2 {
		d.requestMissingAncestors(anchor)
	}
}

// clearStall resets the stall accounting when the cursor advances or the round is
// not yet decidable, so the next stall starts its two-tick count fresh. Runs under
// commitMu.
func (d *DAG) clearStall() {
	d.stallAnchor = Hash{}
	d.stallTicks = 0
}

// requestMissingAncestors hands the anchor's absent causal frontier to the vertex
// fetcher. It is a no-op when no fetcher is wired (nil-safe) or nothing is missing.
// The fetcher is asynchronous, so this returns without blocking the commit loop on
// network I/O; a fetched vertex re-enters through AddVertex and is picked up on a
// later tick. Runs under commitMu.
func (d *DAG) requestMissingAncestors(anchor Hash) {
	if d.vertexFetcher == nil {
		return
	}

	missing := d.store.missingAncestors(anchor)
	if len(missing) == 0 {
		return
	}

	d.vertexFetcher.FetchVertices(missing)
}

// advanceCommitCursor moves the persisted commit cursor past the decided round and,
// when that round is an epoch boundary, transitions the epoch and persists the new
// epoch state atomically WITH the cursor in one batch. Bundling them closes the crash
// window where a cursor advanced past the boundary would be restored beside a stale
// holder set (cursor says epoch k, holders say k-1). Off a boundary the epoch state
// is unchanged, so the cursor is written alone. A crash before the batch leaves the
// old cursor and old epoch state; the decided round's committed vertices are already
// flagged, so re-deciding it yields an empty (idempotent) batch and re-transitions.
func (d *DAG) advanceCommitCursor(round uint64) {
	d.lastCommitted = round + 1

	if d.isEpochBoundary(round) {
		d.transitionEpoch(round)
		d.store.saveCommitCursorBatch(d.lastCommitted, d.epochStateKVs())
		d.regimeDirty = false
		d.producedDirty = false
		return
	}

	// A committed registration this round may have refrozen the genesis snapshot or
	// fired the strict latch, and a member's FIRST committed vertex grows the live
	// produced set. Persist either change atomically WITH the cursor so a restart
	// restores a consistent (cursor, regime, produced) triple: a stale produced set
	// would make the next freeze derive a different eligible set than the rest of
	// the network.
	if d.regimeDirty || d.producedDirty {
		d.store.saveCommitCursorBatch(d.lastCommitted, d.epochStateKVs())
		d.regimeDirty = false
		d.producedDirty = false
		return
	}

	d.store.saveCommitCursor(d.lastCommitted)
}

// commitAnchorBatch applies the anchor's causal batch in deterministic order and
// latches full quorum on a direct certification. It returns false when the anchor's
// causal history is not fully present locally, so the caller stops and retries once
// the pending-parent buffer delivers the missing ancestor.
func (d *DAG) commitAnchorBatch(round uint64, anchor Hash) bool {
	batch, ok := d.store.causalBatch(anchor)
	if !ok {
		return false
	}

	logger.Debug("committing anchor batch", "round", round, "vertices", len(batch))
	d.applyBatch(round, batch)
	d.markQuorumIfDirect(round)

	return true
}

// markQuorumIfDirect latches fullQuorumAchieved when a STRICT-regime round is
// certified DIRECTLY by a round-N+1 stake quorum. Gating on the strict regime is
// the deliberate seam: a relaxed round certifies on a single supporter, which is
// not a BFT quorum, so latching the production posture on it would end convergence
// early. The strict regime beginning (round >= strictStartRound) is now what makes
// this fire — the first strict direct certification. It remains the sole caller of
// markFullQuorumAchieved: without it fullQuorumAchieved never latches, production
// quorum stays relaxed, and validateVertex keeps skipping validateParents, so a
// vertex citing a fabricated parent could enter certified causal history.
func (d *DAG) markQuorumIfDirect(round uint64) {
	if d.roundIsRelaxed(round) {
		return
	}

	if verdict, _ := d.directAnchorVerdict(round); verdict == verdictCertified {
		d.markFullQuorumAchieved()
	}
}

// knownProducersAt returns the set of distinct known-validator producers among the
// given vertex hashes. Vertices from unknown producers are ignored (security).
func (d *DAG) knownProducersAt(hashes []Hash) map[Hash]bool {
	producers := make(map[Hash]bool)

	for _, h := range hashes {
		v := d.store.get(h)
		if v == nil {
			continue
		}

		producer := extractProducer(v)
		if !d.validators.Contains(producer) {
			continue
		}

		producers[producer] = true
	}

	return producers
}

// producerRound keys liveness crediting by (producer, round) so an equivocating
// producer's two vertices in the same round of one batch are credited once, while a
// producer with vertices in several rounds of one batch is credited for each round.
type producerRound struct {
	producer Hash   // producer is the producing validator
	round    uint64 // round is the vertex's round
}

// applyBatch applies every vertex of a committed anchor batch in the batch's
// deterministic (round-then-hash) order, then marks each committed so no later batch
// revisits it. commitRound is the deciding round, passed unchanged to the proof gate
// and processTransactions so epoch and proof selection match the prior per-round
// path. Before applying, verifyRoundProofs verifies the batch's BLS quorum proofs in
// one parallel pass whose per-ATX verdicts feed the sequential apply in the identical
// order, so the set of accepted ATXs is unchanged.
func (d *DAG) applyBatch(commitRound uint64, batch []Hash) {
	d.epochTotalRounds++

	verdicts := d.verifyRoundProofs(commitRound, batch)
	credited := make(map[producerRound]bool)

	for _, h := range batch {
		v := d.store.get(h)
		if v == nil {
			continue
		}

		d.creditLiveness(v, credited)
		d.epochFees += d.processTransactions(v, commitRound, verdicts).Epoch
		d.store.markVertexCommitted(h)

		// The vertex is now committed history: its producer enters the live produced
		// set, in the same strictly cursor-ordered position on every node.
		d.recordProducedMember(extractProducer(v))
	}
}

// creditLiveness credits a vertex's producer one round of liveness, deduped by
// (producer, round) within the batch so an equivocating producer's same-round
// vertices count once. Liveness is per distinct round, not per vertex; a double
// credit would inflate that producer's effective_stake x liveness reward share.
func (d *DAG) creditLiveness(v *types.Vertex, credited map[producerRound]bool) {
	key := producerRound{producer: extractProducer(v), round: v.Round()}
	if credited[key] {
		return
	}

	credited[key] = true
	d.epochRoundsProduced[key.producer]++
}

// proofVerdicts carries the parallel proof-verification result of a round to the
// sequential apply loop. The two loops walk the round's vertices and transactions
// in the same order, so consuming one verdict per ATX with next() keeps the
// verdict aligned to its ATX. A nil verdicts pointer means proof verification is
// disabled, and next() always reports "no error".
type proofVerdicts struct {
	errs   []error // errs holds one entry per proof-bearing ATX, in committed order
	cursor int     // cursor is the index of the next proof-bearing ATX to consume
}

// next reports the verdict for the next proof-bearing ATX in committed order.
// hasProofs must mirror the apply loop's "this ATX carries proofs" test so the
// cursor advances in lockstep with the pre-pass. ATXs without proofs are not
// represented in errs and never advance the cursor, matching the pre-pass.
func (p *proofVerdicts) next(hasProofs bool) error {
	if p == nil || !hasProofs {
		return nil
	}

	if p.cursor >= len(p.errs) {
		return nil
	}

	err := p.errs[p.cursor]
	p.cursor++

	return err
}

// verifyRoundProofs runs the parallel proof gate for a round and returns the
// per-ATX verdicts the apply loop consumes. It returns nil when proof
// verification is disabled, so executeTx falls back to its no-verification path.
func (d *DAG) verifyRoundProofs(round uint64, hashes []Hash) *proofVerdicts {
	if d.verifyATXProofsBatch == nil {
		return nil
	}

	atxs := d.collectRoundProofs(hashes)
	if len(atxs) == 0 {
		return &proofVerdicts{}
	}

	return &proofVerdicts{errs: d.verifyATXProofsBatch(atxs, round)}
}

// collectRoundProofs walks the round's vertices and transactions in committed
// order and returns one fresh AttestedTransaction per proof-bearing ATX. It
// mirrors the apply loop's iteration exactly (same vertex order, same
// Transactions index, same skip conditions) so the returned slice lines up
// one-to-one with the apply loop's proof-bearing ATXs.
func (d *DAG) collectRoundProofs(hashes []Hash) []*types.AttestedTransaction {
	var atxs []*types.AttestedTransaction

	for _, h := range hashes {
		v := d.store.get(h)
		if v == nil {
			continue
		}

		for i := 0; i < v.TransactionsLength(); i++ {
			atx := new(types.AttestedTransaction)
			if !v.Transactions(atx, i) {
				continue
			}

			// Mirror executeTx exactly: an ATX with a nil transaction returns
			// before the proof check, and an ATX with no proofs skips it, so
			// neither is part of the batch. Keeping these predicates identical
			// keeps the verdict cursor aligned with the apply loop.
			if atx.Transaction(nil) == nil {
				continue
			}

			if atx.ProofsLength() == 0 {
				continue
			}

			atxs = append(atxs, atx)
		}
	}

	return atxs
}

// processTransactions handles committed transactions from a vertex.
// Returns the accumulated fee summary for the vertex.
func (d *DAG) processTransactions(v *types.Vertex, commitRound uint64, verdicts *proofVerdicts) FeeSplit {
	var atx types.AttestedTransaction
	var vertexFees FeeSplit

	producer := extractProducer(v)

	for i := 0; i < v.TransactionsLength(); i++ {
		if !v.Transactions(&atx, i) {
			continue
		}

		txFees := d.executeTx(&atx, commitRound, producer, verdicts)
		vertexFees.Total += txFees.Total
		vertexFees.Burned += txFees.Burned
		vertexFees.Epoch += txFees.Epoch
	}

	return vertexFees
}

// executeTx checks version conflicts, deducts fees, and executes a transaction.
// Returns the fee split for this transaction (zero if fees disabled).
//
// verdicts carries the round's parallel proof-verification result; it is nil
// only when executeTx is driven outside the round commit loop (such as direct
// unit tests), in which case proofs are verified inline.
func (d *DAG) executeTx(atx *types.AttestedTransaction, commitRound uint64, producer Hash, verdicts *proofVerdicts) FeeSplit {
	tx := atx.Transaction(nil)
	if tx == nil {
		logger.Warn("tx is nil, skipping")
		return FeeSplit{}
	}

	funcName := string(tx.FunctionName())
	logger.Debug("processing tx", "func", funcName)

	// Verify BLS quorum proofs first (skip genesis/singletons/creates_objects with
	// no proofs). This runs before the commit-once guard on purpose: a proof or
	// epoch failure depends on the attested-transaction wrapper (its proofs and
	// attestation epoch), not on the inner transaction, so a client that recollects
	// against the current epoch and resubmits sends the same inner transaction hash
	// with fresh proofs. Marking that hash on a proof failure would block the
	// legitimate recollect-after-grace resubmission, so a proof failure returns
	// without recording the hash.
	if err := d.proofVerdict(atx, commitRound, verdicts); err != nil {
		logger.Warn("ATX proof verification failed", "func", funcName, "error", err)
		d.emitTransaction(tx, false, FailAuth)
		return FeeSplit{}
	}

	// Commit-time authenticity: re-verify the inner transaction's sender signature
	// and hash. This runs deterministically on every node, after the proof verdict
	// is consumed (so the batch-proof cursor stays aligned) and before the
	// commit-once guard (so a forged tx cannot poison the tracker with a chosen
	// hash to censor a legitimate one). A gossiped transaction can reach commit
	// without passing local ingress validation, so authenticity must be enforced
	// here, where every node agrees. New always defaults verifyTxAuth to the real
	// verifyTxAuthenticity, so on a live node this is fail-closed by construction
	// and cannot be skipped; only unit tests with synthetic txs override it.
	if err := d.verifyTxAuth(tx); err != nil {
		logger.Warn("tx authenticity verification failed", "func", funcName, "error", err)
		d.emitTransaction(tx, false, FailAuth)
		return FeeSplit{}
	}

	// Sponsored-transaction expiry: a sponsor bounds its exposure to a stale signed
	// artifact with valid_until (in epochs). This runs before fee deduction so an
	// expired sponsorship never charges the sponsor's coin. A non-sponsored tx is
	// not subject to it (its absent valid_until defaults to 0).
	if !d.sponsoredTxStillValid(tx, commitRound) {
		logger.Warn("sponsored tx expired or unbounded", "func", funcName)
		d.emitTransaction(tx, false, FailExpired)
		return FeeSplit{}
	}

	// Commit-once guard: a transaction can reach the commit path more than once
	// when several producers include the same gossiped transaction in their
	// vertices. The first occurrence proceeds; later occurrences are skipped so
	// object creation, fees, and validator changes are never applied twice. The
	// hash covers the mutable refs (with versions), so a legitimate retry against a
	// newer version has a different hash and is not blocked.
	if txHash, ok := txCommitHash(tx); ok {
		if d.tracker.wasCommitted(txHash) {
			logger.Debug("duplicate tx skipped", "func", funcName)
			return FeeSplit{}
		}

		d.tracker.markCommitted(txHash)
	}

	// Check and update versions atomically
	if !d.tracker.checkAndUpdate(tx) {
		logger.Debug("version conflict", "func", funcName)
		d.emitTransaction(tx, false, FailVersion)
		return FeeSplit{}
	}

	// Protocol-level fee deduction (before execution)
	feeSplit, proceed := d.deductFees(tx, atx, producer)

	// If fee deduction rejected tx (insufficient funds, invalid gas_coin, min_gas violation)
	if !proceed {
		logger.Debug("fee deduction rejected", "func", funcName)
		d.emitTransaction(tx, false, FailFee)
		return feeSplit
	}

	// Ownership check: sender must own all mutable_ref objects
	if !d.validateMutableRefOwnership(tx) {
		logger.Warn("mutable_ref ownership rejected", "func", funcName)
		d.emitTransaction(tx, false, FailOwner)
		return feeSplit
	}

	// Handle system transactions
	d.handleRegisterValidator(tx, commitRound)
	d.handleDeregisterValidator(tx, commitRound)
	d.handleBond(tx)
	d.handleUnbond(tx)
	d.handleDelegate(tx)
	d.handleUndelegate(tx)

	// Execution sharding: skip execution if not a holder of any mutable object.
	// created_objects_replication/max_create_domains > 0 → ALL validators execute (holder unknown until after execution).
	// Otherwise → only holders of mutable objects execute.
	if tx.CreatedObjectsReplicationLength() == 0 && tx.MaxCreateDomains() == 0 && !d.shouldExecute(atx, tx) {
		logger.Debug("skipping execution (not holder)", "func", funcName)
		d.emitTransaction(tx, true, FailNone)
		return feeSplit
	}

	// Execute via state (objects are in AttestedTransaction)
	var success bool
	if d.executor != nil {
		// Re-serialize AttestedTransaction as standalone buffer
		atxBytes := serializeAttestedTx(atx)
		err := d.executor.Execute(atxBytes)
		success = err == nil
		if err != nil {
			logger.Error("executor error", "func", funcName, "error", err)
		}
	} else {
		success = true // no executor = skip execution
	}

	logger.Debug("tx completed", "func", funcName, "success", success)

	reason := FailNone
	if !success {
		reason = FailRevert
	}
	d.emitTransaction(tx, success, reason)

	return feeSplit
}

// proofVerdict returns the proof-verification verdict for one ATX. An ATX with no
// proofs is always accepted (genesis/singletons), exactly as before. When the
// round was verified in parallel, the verdict is read from verdicts in committed
// order so it lines up with this ATX. When verdicts is nil (direct unit-test
// calls outside the round loop), it falls back to the inline single verifier so
// the verdict is still identical to the sequential implementation.
func (d *DAG) proofVerdict(atx *types.AttestedTransaction, commitRound uint64, verdicts *proofVerdicts) error {
	hasProofs := atx.ProofsLength() > 0

	if verdicts != nil {
		return verdicts.next(hasProofs)
	}

	if d.verifyATXProofs != nil && hasProofs {
		return d.verifyATXProofs(atx, commitRound)
	}

	return nil
}

// deductFees performs protocol-level fee deduction from gas_coin.
// Returns the fee split and whether the tx should proceed.
// proceed=true means fees were successfully handled (or fees are disabled).
// proceed=false means tx must be rejected (missing/invalid gas_coin, min_gas,
// insufficient funds).
func (d *DAG) deductFees(tx *types.Transaction, atx *types.AttestedTransaction, producer Hash) (FeeSplit, bool) {
	if d.feeParams == nil || d.coinStore == nil {
		return FeeSplit{}, true
	}

	// No gas coin: reject, unless this is a validator-set-management action.
	// Genesis is seeded state and protocol actions (issuance, reward crediting,
	// slashing) are not transactions, so every user transaction must reference a
	// funded gas coin. Validator (de)registration is a network-formation action,
	// not a value transaction: a joining validator has no coin yet, and its
	// authenticity is already enforced at commit, so it is exempt here.
	gasCoinBytes := tx.GasCoinBytes()
	if len(gasCoinBytes) != 32 {
		if d.isRegisterValidatorTx(tx) || d.isDeregisterValidatorTx(tx) {
			return FeeSplit{}, true
		}

		return FeeSplit{}, false
	}

	var gasCoinID [32]byte
	copy(gasCoinID[:], gasCoinBytes)

	// min_gas anti-spam check
	if tx.MaxGas() < d.feeParams.MinGas {
		logger.Warn("max_gas below minimum", "max_gas", tx.MaxGas(), "min_gas", d.feeParams.MinGas)
		return FeeSplit{}, false
	}

	// Validate gas_coin ownership: must belong to sender
	if err := d.validateGasCoin(tx, gasCoinID); err != nil {
		logger.Warn("gas coin validation failed", "error", err)
		return FeeSplit{}, false
	}

	// Split the fee: consumed (compute+transit+domain) feeds the pool; storage is
	// the object's locked deposit, debited from the coin but never pooled.
	consumed, storage := d.calculateTxFeeSplit(tx, atx)
	fee := consumed + storage
	if fee == 0 {
		return FeeSplit{}, true
	}

	// Deduct consumed+storage from gas_coin in one debit.
	_, fullyCovered, err := deductCoinFee(d.coinStore, gasCoinID, fee)
	if err != nil {
		logger.Warn("fee deduction failed", "error", err)
		return FeeSplit{}, false
	}

	// If insufficient funds: fees partially deducted, tx rejected.
	if !fullyCovered {
		return FeeSplit{Total: fee}, false
	}

	// Pool only the consumed portion; the storage deposit stays locked in the object.
	return SplitFee(consumed, *d.feeParams), true
}

// calculateTxFeeSplit splits a transaction's fee into the consumed portion (fed
// to the epoch reward pool) and the storage portion (locked in the created
// objects as a refundable deposit, never pooled). The storage term sums
// StorageDeposit over the created objects using the SAME live validator count
// state.computeStorageDeposit reads, so the debited storage equals the stamped
// deposit and total_supply is unchanged at create time.
func (d *DAG) calculateTxFeeSplit(tx *types.Transaction, atx *types.AttestedTransaction) (consumed, storage uint64) {
	if d.feeParams == nil {
		return 0, 0
	}

	totalValidators := d.validators.Len()
	for _, rep := range extractCreatedObjectsReplication(tx) {
		storage = safeAdd(storage, StorageDeposit(rep, totalValidators, d.feeParams.StorageFee))
	}

	full := d.calculateTxFee(tx, atx)
	if full < storage {
		return 0, full // defensive: never let storage exceed the full fee
	}

	return full - storage, storage
}

// sponsoredTxStillValid reports whether a sponsored transaction may commit at the
// given round, enforcing its valid_until bound. A sponsored tx (fee_payer present)
// MUST carry a nonzero valid_until and may commit only while valid_until is at
// least the round's commit epoch. The boundary round k*epochLength is committed
// before transitionEpoch increments currentEpoch, so commitEpochForRound maps it
// to epoch k-1 (handled there), keeping this check exact across all validators. A
// non-sponsored transaction never reads valid_until and is always valid here.
func (d *DAG) sponsoredTxStillValid(tx *types.Transaction, commitRound uint64) bool {
	if !isSponsored(tx) {
		return true
	}

	validUntil := tx.ValidUntil()
	if validUntil == 0 {
		return false
	}

	return validUntil >= d.commitEpochForRound(commitRound)
}

// validateGasCoin checks that the gas_coin exists, is a singleton, and belongs to sender.
func (d *DAG) validateGasCoin(tx *types.Transaction, gasCoinID [32]byte) error {
	data := d.coinStore.GetObject(gasCoinID)
	if data == nil {
		return fmt.Errorf("gas coin not found: %x", gasCoinID[:8])
	}

	// Must be a singleton (replication=0)
	if rep := readCoinReplication(data); rep != 0 {
		return fmt.Errorf("gas coin is not a singleton: replication=%d", rep)
	}

	// The gas coin must belong to whoever pays: the fee_payer for a sponsored
	// transaction, the sender for a self-paid one. The fee_payer is already bound
	// into the body hash and its signature verified at commit, so this cannot be
	// pointed at a victim's coin without that victim having signed.
	owner, err := readCoinOwner(data)
	if err != nil {
		return err
	}

	payerBytes := tx.SenderBytes()
	if isSponsored(tx) {
		payerBytes = tx.FeePayerBytes()
	}

	if len(payerBytes) != 32 {
		return fmt.Errorf("invalid gas payer length: %d", len(payerBytes))
	}

	var payer [32]byte
	copy(payer[:], payerBytes)

	if owner != payer {
		return fmt.Errorf("gas coin owner mismatch: owner=%x payer=%x", owner[:8], payer[:8])
	}

	return nil
}

// validateMutableRefOwnership checks that all mutable refs are owned by the sender.
// Returns false if any mutable ref is not owned by the sender.
func (d *DAG) validateMutableRefOwnership(tx *types.Transaction) bool {
	count := tx.MutableRefsLength()
	if count == 0 || d.coinStore == nil {
		return true
	}

	senderBytes := tx.SenderBytes()
	if len(senderBytes) != 32 {
		return false
	}

	var sender Hash
	copy(sender[:], senderBytes)

	var ref types.ObjectRef

	for i := 0; i < count; i++ {
		tx.MutableRefs(&ref, i)

		idBytes := ref.IdBytes()
		if len(idBytes) != 32 {
			// Domain refs have no ID — skip ownership check
			if len(ref.Domain()) > 0 {
				continue
			}
			return false
		}

		var objectID Hash
		copy(objectID[:], idBytes)

		data := d.coinStore.GetObject(objectID)
		if data == nil {
			logger.Warn("mutable ref not found", "id_prefix", objectID[:4])
			return false
		}

		owner, err := readCoinOwner(data)
		if err != nil {
			logger.Warn("mutable ref invalid owner", "error", err)
			return false
		}

		if owner != sender {
			logger.Warn("mutable ref ownership mismatch",
				"id_prefix", objectID[:4],
				"owner_prefix", owner[:4],
				"sender_prefix", sender[:4],
			)
			return false
		}
	}

	return true
}

// calculateTxFee computes the total fee from transaction header fields.
func (d *DAG) calculateTxFee(tx *types.Transaction, atx *types.AttestedTransaction) uint64 {
	// Build mutable refs for replication ratio
	mutableRefs := extractMutableObjectRefs(tx, atx)

	// Count standard objects (non-singletons in read + mutable refs)
	readRefs := extractReadObjectRefs(tx, atx)
	allRefs := append(mutableRefs, readRefs...)
	standardCount := CountStandardObjects(allRefs)

	// Build created objects replication slice
	createdReps := extractCreatedObjectsReplication(tx)

	// Calculate replication ratio
	totalValidators := d.validators.Len()
	repNum, repDenom := ReplicationRatio(
		mutableRefs,
		len(createdReps),
		int(tx.MaxCreateDomains()),
		d.computeHolders,
		totalValidators,
	)

	return CalculateFee(
		tx.MaxGas(),
		repNum, repDenom,
		standardCount,
		createdReps,
		int(tx.MaxCreateDomains()),
		totalValidators,
		*d.feeParams,
	)
}

// extractMutableObjectRefs builds ObjectRef slice from tx mutable refs + ATX replication map.
func extractMutableObjectRefs(tx *types.Transaction, atx *types.AttestedTransaction) []ObjectRef {
	count := tx.MutableRefsLength()
	if count == 0 {
		return nil
	}

	repMap := buildReplicationMap(atx)
	refs := make([]ObjectRef, 0, count)
	var ref types.ObjectRef

	for i := 0; i < count; i++ {
		if !tx.MutableRefs(&ref, i) {
			continue
		}

		idBytes := ref.IdBytes()
		if len(idBytes) != 32 {
			continue
		}

		var id [32]byte
		copy(id[:], idBytes)

		replication := uint16(0)
		if rep, found := repMap[id]; found {
			replication = rep
		}

		refs = append(refs, ObjectRef{ID: id, Replication: replication})
	}

	return refs
}

// extractReadObjectRefs builds ObjectRef slice from tx read refs + ATX replication map.
func extractReadObjectRefs(tx *types.Transaction, atx *types.AttestedTransaction) []ObjectRef {
	count := tx.ReadRefsLength()
	if count == 0 {
		return nil
	}

	repMap := buildReplicationMap(atx)
	refs := make([]ObjectRef, 0, count)
	var ref types.ObjectRef

	for i := 0; i < count; i++ {
		if !tx.ReadRefs(&ref, i) {
			continue
		}

		idBytes := ref.IdBytes()
		if len(idBytes) != 32 {
			continue
		}

		var id [32]byte
		copy(id[:], idBytes)

		replication := uint16(0)
		if rep, found := repMap[id]; found {
			replication = rep
		}

		refs = append(refs, ObjectRef{ID: id, Replication: replication})
	}

	return refs
}

// extractCreatedObjectsReplication reads the created_objects_replication vector from tx.
func extractCreatedObjectsReplication(tx *types.Transaction) []uint16 {
	count := tx.CreatedObjectsReplicationLength()
	if count == 0 {
		return nil
	}

	reps := make([]uint16, count)
	for i := 0; i < count; i++ {
		reps[i] = tx.CreatedObjectsReplication(i)
	}

	return reps
}

// shouldExecute returns true if this node should execute the transaction.
// A node executes if it is a holder of at least one object in MutableRefs.
// Singletons (replication=0, not in ATX objects) are held by all validators.
func (d *DAG) shouldExecute(atx *types.AttestedTransaction, tx *types.Transaction) bool {
	if d.isHolder == nil {
		return true
	}

	replicationMap := buildReplicationMap(atx)

	var ref types.ObjectRef
	for i := 0; i < tx.MutableRefsLength(); i++ {
		if !tx.MutableRefs(&ref, i) {
			continue
		}

		idBytes := ref.IdBytes()
		if len(idBytes) != 32 {
			continue
		}

		var objectID [32]byte
		copy(objectID[:], idBytes)

		replication, found := replicationMap[objectID]
		if !found {
			replication = 0
		}

		if d.isHolder(objectID, replication) {
			return true
		}
	}

	return tx.MutableRefsLength() == 0
}

// buildReplicationMap extracts objectID → replication from ATX objects vector.
func buildReplicationMap(atx *types.AttestedTransaction) map[[32]byte]uint16 {
	count := atx.ObjectsLength()
	if count == 0 {
		return nil
	}

	m := make(map[[32]byte]uint16, count)
	var obj types.Object

	for i := 0; i < count; i++ {
		if !atx.Objects(&obj, i) {
			continue
		}

		idBytes := obj.IdBytes()
		if len(idBytes) != 32 {
			continue
		}

		var id [32]byte
		copy(id[:], idBytes)
		m[id] = obj.Replication()
	}

	return m
}

// handleRegisterValidator checks if TX is register_validator and adds the validator.
// The validator pubkey is taken from tx.Sender (matching the Rust pod behavior).
// Network addresses are parsed from tx.Args.
func (d *DAG) handleRegisterValidator(tx *types.Transaction, commitRound uint64) {
	if !d.isRegisterValidatorTx(tx) {
		return
	}

	sender := tx.SenderBytes()
	if len(sender) != 32 {
		return
	}

	var pubkey Hash
	copy(pubkey[:], sender)

	// Parse network address and BLS pubkey from transaction args
	quicAddr, blsPubkeyBytes := genesis.DecodeRegisterValidatorArgs(tx.ArgsBytes())

	var blsPubkey [48]byte
	if len(blsPubkeyBytes) == 48 {
		copy(blsPubkey[:], blsPubkeyBytes)
	}

	isNew := d.validators.Add(pubkey, quicAddr, blsPubkey)

	// Admit the validator to the committed member set and refresh the genesis regime.
	// This is the committed-only path (never an optimistic self-add), so the frozen
	// genesis snapshot and the strict latch derive identically on every node (I7).
	d.recordCommittedMember(pubkey, commitRound)

	d.setRewardCoinFromArgs(tx, pubkey)

	// Track mid-epoch additions for churn limiting
	if isNew && d.epochLength > 0 {
		d.epochAdditions = append(d.epochAdditions, pubkey)
	}

	// Retry pending vertices — some may be from this newly registered producer.
	// Run async to avoid blocking the commit path.
	if isNew {
		go d.processPendingVertices()
	}

	// Enter transition immediately when minValidators is reached.
	// This prevents producing vertices with cross-references before transition
	// parent filtering kicks in.
	if d.minValidators > 0 && d.validators.Len() >= d.minValidators {
		d.enterTransition(commitRound)
	}
}

// setRewardCoinFromArgs designates the validator's reward coin: an explicit
// reward-coin arg takes priority, else the transaction's gas coin when the
// validator owns it. If neither is determinable the reward coin is left zero, and
// the epoch reward split skips that validator's liquid portion with a warning
// rather than failing the epoch (per spec §6).
func (d *DAG) setRewardCoinFromArgs(tx *types.Transaction, pubkey Hash) {
	if coin, ok := genesis.DecodeRegisterValidatorRewardCoin(tx.ArgsBytes()); ok {
		d.validators.SetRewardCoin(pubkey, coin)
		return
	}

	if coin, ok := d.ownedGasCoin(tx, pubkey); ok {
		d.validators.SetRewardCoin(pubkey, coin)
	}
}

// ownedGasCoin returns the transaction's gas coin when it is a 32-byte ID owned
// by owner. It returns ok=false when there is no gas coin, the coin store is
// unwired, or the coin is not owned by owner.
func (d *DAG) ownedGasCoin(tx *types.Transaction, owner Hash) (Hash, bool) {
	gasCoinBytes := tx.GasCoinBytes()
	if len(gasCoinBytes) != 32 || d.coinStore == nil {
		return Hash{}, false
	}

	var coinID Hash
	copy(coinID[:], gasCoinBytes)

	data := d.coinStore.GetObject(coinID)
	if data == nil {
		return Hash{}, false
	}

	coinOwner, err := readCoinOwner(data)
	if err != nil || coinOwner != owner {
		return Hash{}, false
	}

	return coinID, true
}

// handleDeregisterValidator checks if TX is deregister_validator and marks for removal.
// The validator stays active until the next epoch boundary.
func (d *DAG) handleDeregisterValidator(tx *types.Transaction, commitRound uint64) {
	if !d.isDeregisterValidatorTx(tx) {
		return
	}

	sender := tx.SenderBytes()
	if len(sender) != 32 {
		return
	}

	var pubkey Hash
	copy(pubkey[:], sender)

	d.pendingRemovals[pubkey] = true

	logger.Info("validator deregistration pending",
		"pubkey_prefix", pubkey[:4],
		"commitRound", commitRound,
	)
}

// handleBond applies a bond transaction: it strictly debits the staked coin and
// raises the sender's self-stake. The staked coin is the transaction's first
// mutable_ref (its ownership is validated upstream by validateMutableRefOwnership).
// Returns true when the bond was applied. The debit is strict: an under-funded
// bond is rejected without touching the coin (it never uses deductCoinFee, which
// zeroes a coin on shortfall).
func (d *DAG) handleBond(tx *types.Transaction) bool {
	sender, coinID, amount, ok := d.parseStakeTx(tx, d.isBondTx)
	if !ok {
		return false
	}

	current := d.validators.Get(sender)
	if current == nil {
		return false
	}

	newStake := safeAdd(current.SelfStake, amount)
	if newStake < d.minStake {
		logger.Warn("bond below minimum stake", "new_stake", newStake, "min_stake", d.minStake)
		return false
	}

	if !d.strictDebit(coinID, amount) {
		return false
	}

	d.validators.SetSelfStake(sender, newStake)
	return true
}

// handleUnbond applies an unbond transaction: it lowers the sender's self-stake
// and credits the amount back to the coin (the first mutable_ref). Returns true
// when applied. TODO: enforce an unbonding delay (the withdrawn stake should stay
// locked and slashable for a period, finalized with slashing in spec §10).
func (d *DAG) handleUnbond(tx *types.Transaction) bool {
	sender, coinID, amount, ok := d.parseStakeTx(tx, d.isUnbondTx)
	if !ok {
		return false
	}

	current := d.validators.Get(sender)
	if current == nil || current.SelfStake < amount {
		return false
	}

	// Minimum-stake floor: a full exit (down to exactly 0) is allowed, but an
	// unbond that would leave a POSITIVE self-stake below minStake is rejected, so
	// a registered validator can never sit beneath the floor handleBond enforces.
	remaining := current.SelfStake - amount
	if d.minStake > 0 && remaining > 0 && remaining < d.minStake {
		logger.Warn("unbond would leave sub-minimum stake",
			"remaining", remaining, "min_stake", d.minStake)
		return false
	}

	if err := creditCoin(d.coinStore, coinID, amount); err != nil {
		logger.Warn("unbond credit failed", "error", err)
		return false
	}

	d.validators.SetSelfStake(sender, current.SelfStake-amount)
	return true
}

// handleDelegate applies a delegate transaction atomically: it validates the
// target validator is known and not jailed, strictly debits the delegator's coin
// (the first mutable_ref) by amount, creates the stake-position object owned by
// the delegator, and raises the validator's delegated total — all or nothing.
// The delegated total is never raised without a funded position. Returns true
// when the delegation was applied.
func (d *DAG) handleDelegate(tx *types.Transaction) bool {
	delegator, coinID, validator, amount, ok := d.parseDelegateTx(tx, d.isDelegateTx, true)
	if !ok {
		return false
	}

	target := d.validators.Get(validator)
	if target == nil || target.Jailed {
		return false
	}

	if !d.strictDebit(coinID, amount) {
		return false
	}

	d.coinStore.SetObject(buildDelegationObject(delegator, validator, amount))
	d.validators.AddDelegated(validator, amount)
	return true
}

// handleUndelegate applies an undelegate transaction: it reads the delegator's
// stake position, destroys it, credits the principal back to the coin (the first
// mutable_ref), and lowers the validator's delegated total. Returns true when
// applied. TODO: enforce an unbonding delay (the withdrawn delegation should stay
// locked and slashable for a period, finalized with slashing in spec §10).
func (d *DAG) handleUndelegate(tx *types.Transaction) bool {
	delegator, coinID, validator, _, ok := d.parseDelegateTx(tx, d.isUndelegateTx, false)
	if !ok {
		return false
	}

	posID := DelegationID(delegator, validator)
	amount, ok := d.readDelegationAmount(posID, validator)
	if !ok {
		return false
	}

	if err := creditCoin(d.coinStore, coinID, amount); err != nil {
		logger.Warn("undelegate credit failed", "error", err)
		return false
	}

	d.coinStore.DeleteObject(posID)
	d.validators.SubDelegated(validator, amount)
	return true
}

// readDelegationAmount loads a delegation position and returns its amount. ok is
// false when the position is missing or its content does not target validator.
func (d *DAG) readDelegationAmount(posID, validator [32]byte) (uint64, bool) {
	data := d.coinStore.GetObject(posID)
	if data == nil {
		return 0, false
	}

	obj := types.GetRootAsObject(data, 0)
	gotValidator, amount, ok := decodeDelegationContent(obj.ContentBytes())
	if !ok || gotValidator != validator {
		return 0, false
	}

	return amount, true
}

// parseDelegateTx validates a delegate/undelegate transaction and extracts the
// delegator (sender), the coin ID (first mutable_ref), the target validator, and
// (for delegate) the Borsh u64 amount. matches gates the pod and function name.
// ok is false on any malformed input.
func (d *DAG) parseDelegateTx(tx *types.Transaction, matches func(*types.Transaction) bool, withAmount bool) (delegator, coinID, validator [32]byte, amount uint64, ok bool) {
	if d.coinStore == nil || !matches(tx) {
		return delegator, coinID, validator, 0, false
	}

	senderBytes := tx.SenderBytes()
	if len(senderBytes) != 32 {
		return delegator, coinID, validator, 0, false
	}
	copy(delegator[:], senderBytes)

	coinID, ok = firstMutableRefID(tx)
	if !ok {
		return delegator, coinID, validator, 0, false
	}

	validator, amount, ok = decodeDelegateArgs(tx.ArgsBytes(), withAmount)
	if !ok || (withAmount && amount == 0) {
		return delegator, coinID, validator, 0, false
	}

	return delegator, coinID, validator, amount, true
}

// decodeDelegateArgs reads the validator(32) and optional amount(8 LE) from
// delegate/undelegate args. ok is false when the args are too short.
func decodeDelegateArgs(args []byte, withAmount bool) (validator [32]byte, amount uint64, ok bool) {
	need := 32
	if withAmount {
		need += 8
	}
	if len(args) < need {
		return validator, 0, false
	}

	copy(validator[:], args[:32])
	if withAmount {
		amount = binary.LittleEndian.Uint64(args[32:40])
	}

	return validator, amount, true
}

// isDelegateTx checks if a transaction calls delegate on the system pod.
func (d *DAG) isDelegateTx(tx *types.Transaction) bool {
	return d.isSystemFunc(tx, delegateFunc)
}

// isUndelegateTx checks if a transaction calls undelegate on the system pod.
func (d *DAG) isUndelegateTx(tx *types.Transaction) bool {
	return d.isSystemFunc(tx, undelegateFunc)
}

// parseStakeTx validates a bond/unbond transaction and extracts the sender, the
// staked coin ID (first mutable_ref), and the Borsh u64 amount. matches gates the
// pod, function name, and coin store. ok is false on any malformed input.
func (d *DAG) parseStakeTx(tx *types.Transaction, matches func(*types.Transaction) bool) (sender, coinID [32]byte, amount uint64, ok bool) {
	if d.coinStore == nil || !matches(tx) {
		return sender, coinID, 0, false
	}

	senderBytes := tx.SenderBytes()
	if len(senderBytes) != 32 {
		return sender, coinID, 0, false
	}
	copy(sender[:], senderBytes)

	coinID, ok = firstMutableRefID(tx)
	if !ok {
		return sender, coinID, 0, false
	}

	amount, ok = decodeStakeAmount(tx.ArgsBytes())
	if !ok || amount == 0 {
		return sender, coinID, 0, false
	}

	return sender, coinID, amount, true
}

// strictDebit subtracts amount from a coin only if it fully covers it, leaving
// the coin untouched otherwise. Unlike deductCoinFee it never zeroes on shortfall.
// It debits only a singleton (replication==0) coin, mirroring validateGasCoin: a
// staking/delegation debit against a replicated coin would diverge across nodes
// (only holders execute), so a non-singleton is rejected to keep the debit uniform.
func (d *DAG) strictDebit(coinID [32]byte, amount uint64) bool {
	data := d.coinStore.GetObject(coinID)
	if data == nil {
		return false
	}

	if rep := readCoinReplication(data); rep != 0 {
		logger.Warn("staked coin is not a singleton", "replication", rep)
		return false
	}

	balance, err := readCoinBalance(data)
	if err != nil || balance < amount {
		return false
	}

	d.coinStore.SetObject(writeCoinBalance(data, balance-amount))
	return true
}

// firstMutableRefID returns the 32-byte ID of the transaction's first mutable_ref.
func firstMutableRefID(tx *types.Transaction) ([32]byte, bool) {
	var id [32]byte
	if tx.MutableRefsLength() == 0 {
		return id, false
	}

	var ref types.ObjectRef
	tx.MutableRefs(&ref, 0)
	idBytes := ref.IdBytes()
	if len(idBytes) != 32 {
		return id, false
	}

	copy(id[:], idBytes)
	return id, true
}

// decodeStakeAmount reads a Borsh u64 (little-endian) amount from bond/unbond args.
func decodeStakeAmount(args []byte) (uint64, bool) {
	if len(args) < 8 {
		return 0, false
	}

	return binary.LittleEndian.Uint64(args[:8]), true
}

// isBondTx checks if a transaction calls bond on the system pod.
func (d *DAG) isBondTx(tx *types.Transaction) bool {
	return d.isSystemFunc(tx, bondFunc)
}

// isUnbondTx checks if a transaction calls unbond on the system pod.
func (d *DAG) isUnbondTx(tx *types.Transaction) bool {
	return d.isSystemFunc(tx, unbondFunc)
}

// isSystemFunc reports whether tx targets the system pod with the given function.
func (d *DAG) isSystemFunc(tx *types.Transaction, fn string) bool {
	podBytes := tx.PodBytes()
	if len(podBytes) != 32 || !bytes.Equal(podBytes, d.systemPod[:]) {
		return false
	}

	return string(tx.FunctionName()) == fn
}

// isDeregisterValidatorTx checks if a transaction calls deregister_validator on system pod.
func (d *DAG) isDeregisterValidatorTx(tx *types.Transaction) bool {
	podBytes := tx.PodBytes()
	if len(podBytes) != 32 {
		return false
	}

	if !bytes.Equal(podBytes, d.systemPod[:]) {
		return false
	}

	funcName := string(tx.FunctionName())
	return funcName == deregisterValidatorFunc
}

// isRegisterValidatorTx checks if a transaction calls register_validator on system pod.
func (d *DAG) isRegisterValidatorTx(tx *types.Transaction) bool {
	podBytes := tx.PodBytes()
	if len(podBytes) != 32 {
		return false
	}

	if !bytes.Equal(podBytes, d.systemPod[:]) {
		return false
	}

	funcName := string(tx.FunctionName())
	return funcName == registerValidatorFunc
}

// txCommitHash returns a transaction's 32-byte hash, or ok=false when the hash
// field is malformed (such a transaction is processed without the commit-once
// guard rather than being dropped on a bad hash length).
func txCommitHash(tx *types.Transaction) (Hash, bool) {
	hashBytes := tx.HashBytes()
	if len(hashBytes) != 32 {
		return Hash{}, false
	}

	var hash Hash
	copy(hash[:], hashBytes)

	return hash, true
}

// emitTransaction sends a committed transaction to the output channel.
func (d *DAG) emitTransaction(tx *types.Transaction, success bool, reason FailReason) {
	var txHash Hash
	if hashBytes := tx.HashBytes(); len(hashBytes) == 32 {
		copy(txHash[:], hashBytes)
	}

	var sender Hash
	if senderBytes := tx.SenderBytes(); len(senderBytes) == 32 {
		copy(sender[:], senderBytes)
	}

	committed := CommittedTx{
		Hash:     txHash,
		Success:  success,
		Reason:   reason,
		Function: string(tx.FunctionName()),
		Sender:   sender,
	}

	select {
	case d.committed <- committed:
	case <-d.stop:
	}
}

// serializeAttestedTx re-serializes an AttestedTransaction as a standalone buffer.
// This is needed because atx.Table().Bytes returns the parent Vertex buffer.
func serializeAttestedTx(atx *types.AttestedTransaction) []byte {
	builder := flatbuffers.NewBuilder(1024)

	// Rebuild Transaction
	tx := atx.Transaction(nil)
	txOffset := serializeTx(builder, tx)

	// Rebuild Objects vector
	objOffsets := make([]flatbuffers.UOffsetT, atx.ObjectsLength())
	for i := 0; i < atx.ObjectsLength(); i++ {
		var obj types.Object
		atx.Objects(&obj, i)
		objOffsets[i] = serializeObject(builder, &obj)
	}

	types.AttestedTransactionStartObjectsVector(builder, len(objOffsets))
	for i := len(objOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(objOffsets[i])
	}
	objectsVec := builder.EndVector(len(objOffsets))

	// Rebuild Proofs vector
	proofOffsets := make([]flatbuffers.UOffsetT, atx.ProofsLength())
	for i := 0; i < atx.ProofsLength(); i++ {
		var proof types.QuorumProof
		atx.Proofs(&proof, i)
		proofOffsets[i] = serializeQuorumProof(builder, &proof)
	}

	types.AttestedTransactionStartProofsVector(builder, len(proofOffsets))
	for i := len(proofOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(proofOffsets[i])
	}
	proofsVec := builder.EndVector(len(proofOffsets))

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOffset)
	types.AttestedTransactionAddObjects(builder, objectsVec)
	types.AttestedTransactionAddProofs(builder, proofsVec)
	types.AttestedTransactionAddAttestationEpoch(builder, atx.AttestationEpoch())
	atxOffset := types.AttestedTransactionEnd(builder)

	builder.Finish(atxOffset)

	return builder.FinishedBytes()
}

// serializeTx rebuilds a Transaction in the builder.
func serializeTx(builder *flatbuffers.Builder, tx *types.Transaction) flatbuffers.UOffsetT {
	if tx == nil {
		types.TransactionStart(builder)
		return types.TransactionEnd(builder)
	}

	return genesis.RebuildTxInBuilder(builder, tx)
}

// serializeObject rebuilds an Object in the builder.
func serializeObject(builder *flatbuffers.Builder, obj *types.Object) flatbuffers.UOffsetT {
	idVec := builder.CreateByteVector(obj.IdBytes())
	ownerVec := builder.CreateByteVector(obj.OwnerBytes())
	contentVec := builder.CreateByteVector(obj.ContentBytes())

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, obj.Version())
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, obj.Replication())
	types.ObjectAddContent(builder, contentVec)
	types.ObjectAddFees(builder, obj.Fees())

	return types.ObjectEnd(builder)
}

// serializeQuorumProof rebuilds a QuorumProof in the builder.
func serializeQuorumProof(builder *flatbuffers.Builder, proof *types.QuorumProof) flatbuffers.UOffsetT {
	objIdVec := builder.CreateByteVector(proof.ObjectIdBytes())
	blsSigVec := builder.CreateByteVector(proof.BlsSignatureBytes())
	bitmapVec := builder.CreateByteVector(proof.SignerBitmapBytes())

	types.QuorumProofStart(builder)
	types.QuorumProofAddObjectId(builder, objIdVec)
	types.QuorumProofAddBlsSignature(builder, blsSigVec)
	types.QuorumProofAddSignerBitmap(builder, bitmapVec)

	return types.QuorumProofEnd(builder)
}
