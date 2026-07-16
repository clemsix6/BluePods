package state

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"BluePods/internal/events"
	"BluePods/internal/types"
)

// captureEvents swaps the default slog logger for a JSON handler writing into a
// fresh buffer, restoring the previous default on test cleanup.
func captureEvents(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	return &buf
}

// eventsNamed decodes one JSON object per captured line and returns those whose
// reserved events.Key attribute equals name, in emission order.
func eventsNamed(t *testing.T, buf *bytes.Buffer, name string) []map[string]any {
	t.Helper()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}

		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("captured line is not JSON: %v (line=%q)", err, line)
		}

		if rec[events.Key] == name {
			out = append(out, rec)
		}
	}

	return out
}

// TestApplyCreatedObjects_EmitsObjectCreatedAndDepositLocked verifies that a
// created object with a nonzero storage deposit emits both state.object.created
// and fees.deposit.locked, for every created object regardless of holding.
func TestApplyCreatedObjects_EmitsObjectCreatedAndDepositLocked(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)
	s.SetIsHolder(func(objectID [32]byte, replication uint16) bool { return true })
	s.SetStorageFees(1000, 9500, 100) // storageFee=1000, totalValidators=100

	buf := captureEvents(t)

	txHash := Hash{0xAA}
	output := buildPodOutputWithCreated(2, 10) // 2 objects, replication=10

	s.applyCreatedObjects(output, txHash)

	created := eventsNamed(t, buf, events.EvObjectCreated)
	if len(created) != 2 {
		t.Fatalf("want 2 %s events, got %d: %v", events.EvObjectCreated, len(created), created)
	}

	expectedID := computeObjectID(txHash, 0)
	rec := created[0]
	if rec["object"] != hex.EncodeToString(expectedID[:]) {
		t.Errorf("object = %v, want %s", rec["object"], hex.EncodeToString(expectedID[:]))
	}
	if rec["tx"] != hex.EncodeToString(txHash[:]) {
		t.Errorf("tx = %v, want %s", rec["tx"], hex.EncodeToString(txHash[:]))
	}
	if rec["version"] != float64(0) {
		t.Errorf("version = %v, want 0", rec["version"])
	}
	if rec["replication"] != float64(10) {
		t.Errorf("replication = %v, want 10", rec["replication"])
	}

	locked := eventsNamed(t, buf, events.EvDepositLocked)
	if len(locked) != 2 {
		t.Fatalf("want 2 %s events, got %d: %v", events.EvDepositLocked, len(locked), locked)
	}

	// deposit = replication(10) * storageFee(1000) / totalValidators(100) = 100
	if locked[0]["object"] != hex.EncodeToString(expectedID[:]) {
		t.Errorf("locked object = %v, want %s", locked[0]["object"], hex.EncodeToString(expectedID[:]))
	}
	if locked[0]["amount"] != float64(100) {
		t.Errorf("locked amount = %v, want 100", locked[0]["amount"])
	}
}

// TestApplyCreatedObjects_NoDepositEventWhenFeesZero verifies that no
// fees.deposit.locked event fires when the storage deposit computes to zero
// (no storage fee configured), while state.object.created still fires.
func TestApplyCreatedObjects_NoDepositEventWhenFeesZero(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)
	s.SetIsHolder(func(objectID [32]byte, replication uint16) bool { return true })
	// No SetStorageFees call: storageFee stays 0, so computeStorageDeposit returns 0.

	buf := captureEvents(t)

	txHash := Hash{0xBB}
	output := buildPodOutputWithCreated(1, 10)

	s.applyCreatedObjects(output, txHash)

	created := eventsNamed(t, buf, events.EvObjectCreated)
	if len(created) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvObjectCreated, len(created))
	}

	locked := eventsNamed(t, buf, events.EvDepositLocked)
	if len(locked) != 0 {
		t.Fatalf("want 0 %s events when fees == 0, got %d: %v", events.EvDepositLocked, len(locked), locked)
	}
}

// TestApplyCreatedObjects_ObjectCreatedFiresEvenIfNotHolder verifies the event
// fires for every created object regardless of local holding, matching the
// tracker callback semantics (all validators track all objects).
func TestApplyCreatedObjects_ObjectCreatedFiresEvenIfNotHolder(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)
	s.SetIsHolder(func(objectID [32]byte, replication uint16) bool { return false })

	buf := captureEvents(t)

	txHash := Hash{0xCC}
	output := buildPodOutputWithCreated(2, 10)

	s.applyCreatedObjects(output, txHash)

	created := eventsNamed(t, buf, events.EvObjectCreated)
	if len(created) != 2 {
		t.Fatalf("want 2 %s events even as non-holder, got %d", events.EvObjectCreated, len(created))
	}
}

// TestApplyUpdatedObjects_EmitsObjectUpdated verifies the event carries the
// object id, tx hash, and the version actually persisted (old version + 1).
func TestApplyUpdatedObjects_EmitsObjectUpdated(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	buf := captureEvents(t)

	objID := Hash{0x60}
	txHash := Hash{0xDD}
	output := buildPodOutputWithUpdated(objID, 3, []byte("new content"))

	s.applyUpdatedObjects(output, txHash)

	recs := eventsNamed(t, buf, events.EvObjectUpdated)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvObjectUpdated, len(recs))
	}

	rec := recs[0]
	if rec["object"] != hex.EncodeToString(objID[:]) {
		t.Errorf("object = %v, want %s", rec["object"], hex.EncodeToString(objID[:]))
	}
	if rec["tx"] != hex.EncodeToString(txHash[:]) {
		t.Errorf("tx = %v, want %s", rec["tx"], hex.EncodeToString(txHash[:]))
	}
	if rec["version"] != float64(4) {
		t.Errorf("version = %v, want 4", rec["version"])
	}
}

// TestApplyDeletedObjects_RefundEmitsDepositRefundedAndSupplyBurned verifies
// that a deletion with a landed refund emits fees.deposit.refunded (the
// refunded amount) and supply.burned (only the burned remainder), plus
// state.object.deleted carrying the refund.
func TestApplyDeletedObjects_RefundEmitsDepositRefundedAndSupplyBurned(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)
	s.SetStorageFees(1000, 9500, 100) // 95% refund

	owner := Hash{0xAA}
	objID := Hash{0x74}
	gasCoinID := Hash{0xEE}

	s.SetObject(buildCoinObject(gasCoinID, 5000, owner))
	s.SetObject(buildTestObjectFullWithFees(objID, 1, owner, 10, []byte("data"), 10000))

	tx := buildMinimalTxWithGasCoin(owner, gasCoinID)
	output := buildPodOutputWithDeleted(objID)

	buf := captureEvents(t)

	s.applyDeletedObjects(output, tx)

	refunded := eventsNamed(t, buf, events.EvDepositRefunded)
	if len(refunded) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvDepositRefunded, len(refunded))
	}
	if refunded[0]["object"] != hex.EncodeToString(objID[:]) {
		t.Errorf("refunded object = %v, want %s", refunded[0]["object"], hex.EncodeToString(objID[:]))
	}
	if refunded[0]["coin"] != hex.EncodeToString(gasCoinID[:]) {
		t.Errorf("refunded coin = %v, want %s", refunded[0]["coin"], hex.EncodeToString(gasCoinID[:]))
	}
	// refund = 10000 * 9500 / 10000 = 9500
	if refunded[0]["amount"] != float64(9500) {
		t.Errorf("refunded amount = %v, want 9500", refunded[0]["amount"])
	}

	burned := eventsNamed(t, buf, events.EvSupplyBurned)
	if len(burned) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvSupplyBurned, len(burned))
	}
	// burned = 10000 - 9500 = 500
	if burned[0]["amount"] != float64(500) {
		t.Errorf("burned amount = %v, want 500", burned[0]["amount"])
	}
	if burned[0]["reason"] != "deletion" {
		t.Errorf("burned reason = %v, want deletion", burned[0]["reason"])
	}

	deleted := eventsNamed(t, buf, events.EvObjectDeleted)
	if len(deleted) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvObjectDeleted, len(deleted))
	}
	if deleted[0]["refund"] != float64(9500) {
		t.Errorf("deleted refund = %v, want 9500", deleted[0]["refund"])
	}
}

// TestApplyDeletedObjects_NoGasCoinBurnsFullAmountNoRefundEvent verifies that
// a deletion with no gas coin to receive the refund burns the WHOLE deposit
// (supply.burned with the full amount) and never emits fees.deposit.refunded,
// and state.object.deleted carries a zero refund.
func TestApplyDeletedObjects_NoGasCoinBurnsFullAmountNoRefundEvent(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)
	s.SetStorageFees(1000, 9500, 100)
	s.SetTotalSupply(10000)

	owner := Hash{0xAA}
	objID := Hash{0x76}

	s.SetObject(buildTestObjectFullWithFees(objID, 1, owner, 10, []byte("data"), 1000))

	// tx has sender=owner but NO gas_coin.
	tx := buildMinimalTx(owner)
	output := buildPodOutputWithDeleted(objID)

	buf := captureEvents(t)

	s.applyDeletedObjects(output, tx)

	refunded := eventsNamed(t, buf, events.EvDepositRefunded)
	if len(refunded) != 0 {
		t.Fatalf("want 0 %s events when there is no gas coin, got %d: %v", events.EvDepositRefunded, len(refunded), refunded)
	}

	burned := eventsNamed(t, buf, events.EvSupplyBurned)
	if len(burned) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvSupplyBurned, len(burned))
	}
	if burned[0]["amount"] != float64(1000) {
		t.Errorf("burned amount = %v, want 1000 (full deposit)", burned[0]["amount"])
	}

	deleted := eventsNamed(t, buf, events.EvObjectDeleted)
	if len(deleted) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvObjectDeleted, len(deleted))
	}
	if deleted[0]["refund"] != float64(0) {
		t.Errorf("deleted refund = %v, want 0", deleted[0]["refund"])
	}
}

// TestApplyRegisteredDomains_EmitsDomainRegistered verifies the event carries
// the domain name, its resolved object id, and the tx hash.
func TestApplyRegisteredDomains_EmitsDomainRegistered(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	buf := captureEvents(t)

	txHash := Hash{0x11, 0x22}
	domains := []testDomain{{name: "indexed.pod", objectIndex: 2}}
	output := buildPodOutputWithDomainsRaw(0, 10, domains)
	out := types.GetRootAsPodExecuteOutput(output, 0)

	s.applyRegisteredDomains(out, txHash)

	recs := eventsNamed(t, buf, events.EvDomainRegistered)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvDomainRegistered, len(recs))
	}

	expectedID := computeObjectID(txHash, 2)
	rec := recs[0]
	if rec["name"] != "indexed.pod" {
		t.Errorf("name = %v, want indexed.pod", rec["name"])
	}
	if rec["object"] != hex.EncodeToString(expectedID[:]) {
		t.Errorf("object = %v, want %s", rec["object"], hex.EncodeToString(expectedID[:]))
	}
	if rec["tx"] != hex.EncodeToString(txHash[:]) {
		t.Errorf("tx = %v, want %s", rec["tx"], hex.EncodeToString(txHash[:]))
	}
}

// TestApplyRegisteredDomains_RebindEmitsDomainUpdated verifies that applying a
// RegisteredDomain for a name already bound in the domain store emits
// DomainUpdated instead of DomainRegistered.
func TestApplyRegisteredDomains_RebindEmitsDomainUpdated(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	firstTx := Hash{0x11, 0x22}
	domains := []testDomain{{name: "rebound.pod", objectIndex: 0}}
	first := buildPodOutputWithDomainsRaw(0, 10, domains)
	s.applyRegisteredDomains(types.GetRootAsPodExecuteOutput(first, 0), firstTx)

	buf := captureEvents(t)

	secondTx := Hash{0x33, 0x44}
	second := buildPodOutputWithDomainsRaw(0, 10, domains)
	s.applyRegisteredDomains(types.GetRootAsPodExecuteOutput(second, 0), secondTx)

	if recs := eventsNamed(t, buf, events.EvDomainRegistered); len(recs) != 0 {
		t.Fatalf("rebind must not emit %s, got %d", events.EvDomainRegistered, len(recs))
	}

	recs := eventsNamed(t, buf, events.EvDomainUpdated)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvDomainUpdated, len(recs))
	}
	if recs[0]["name"] != "rebound.pod" {
		t.Errorf("name = %v, want rebound.pod", recs[0]["name"])
	}
}
