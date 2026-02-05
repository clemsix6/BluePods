package podvm

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"BluePods/internal/types"

	flatbuffers "github.com/google/flatbuffers/go"
)

// Error codes from pod-system
const (
	errInvalidArgs         = 3
	errMissingObject       = 4
	errInsufficientBalance = 100
	errInsufficientCoins   = 101
)

// =============================================================================
// Mint Function Tests
// =============================================================================

// TestMint_Success tests minting tokens and verifies a new coin is created.
func TestMint_Success(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	mintAmount := uint64(1000)
	owner := [32]byte{1, 2, 3, 4, 5, 6, 7, 8}
	input := buildMintInput(mintAmount, owner)

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != 0 {
		t.Fatalf("expected success (error=0), got error=%d", result.Error())
	}

	// Verify created object
	if result.CreatedObjectsLength() != 1 {
		t.Fatalf("expected 1 created object, got %d", result.CreatedObjectsLength())
	}

	var createdObj types.Object
	if !result.CreatedObjects(&createdObj, 0) {
		t.Fatal("failed to get created object")
	}

	balance := decodeCoinBalance(createdObj.ContentBytes())
	if balance != mintAmount {
		t.Errorf("expected balance %d, got %d", mintAmount, balance)
	}

	if !bytes.Equal(createdObj.OwnerBytes(), owner[:]) {
		t.Errorf("expected owner %x, got %x", owner[:], createdObj.OwnerBytes())
	}

	t.Logf("mint success: created coin with balance %d", balance)
}

// TestMint_ZeroAmount tests minting zero tokens.
func TestMint_ZeroAmount(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	owner := [32]byte{1, 2, 3, 4, 5, 6, 7, 8}
	input := buildMintInput(0, owner)

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != 0 {
		t.Fatalf("expected success, got error=%d", result.Error())
	}

	var createdObj types.Object
	result.CreatedObjects(&createdObj, 0)
	balance := decodeCoinBalance(createdObj.ContentBytes())

	if balance != 0 {
		t.Errorf("expected balance 0, got %d", balance)
	}
}

// TestMint_LargeAmount tests minting a large amount of tokens.
func TestMint_LargeAmount(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	largeAmount := uint64(1_000_000_000_000)
	owner := [32]byte{1, 2, 3, 4, 5, 6, 7, 8}
	input := buildMintInput(largeAmount, owner)

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != 0 {
		t.Fatalf("expected success, got error=%d", result.Error())
	}

	var createdObj types.Object
	result.CreatedObjects(&createdObj, 0)
	balance := decodeCoinBalance(createdObj.ContentBytes())

	if balance != largeAmount {
		t.Errorf("expected balance %d, got %d", largeAmount, balance)
	}
}

// TestMint_InvalidArgs tests mint with malformed arguments.
func TestMint_InvalidArgs(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	input := buildMintInputInvalidArgs()

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != errInvalidArgs {
		t.Errorf("expected ERR_INVALID_ARGS (%d), got error=%d", errInvalidArgs, result.Error())
	}
}

// =============================================================================
// Transfer Function Tests
// =============================================================================

// TestTransfer_Success tests transferring ownership of a coin.
func TestTransfer_Success(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	coinBalance := uint64(1000)
	oldOwner := [32]byte{1, 2, 3, 4, 5, 6, 7, 8}
	newOwner := [32]byte{9, 10, 11, 12, 13, 14, 15, 16}

	input := buildTransferInput(coinBalance, oldOwner, newOwner)

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != 0 {
		t.Fatalf("expected success (error=0), got error=%d", result.Error())
	}

	// Verify updated object
	if result.UpdatedObjectsLength() != 1 {
		t.Fatalf("expected 1 updated object, got %d", result.UpdatedObjectsLength())
	}

	var updatedObj types.Object
	result.UpdatedObjects(&updatedObj, 0)

	// Balance should be unchanged
	balance := decodeCoinBalance(updatedObj.ContentBytes())
	if balance != coinBalance {
		t.Errorf("expected balance %d (unchanged), got %d", coinBalance, balance)
	}

	// Owner should be updated
	if !bytes.Equal(updatedObj.OwnerBytes(), newOwner[:]) {
		t.Errorf("expected owner %x, got %x", newOwner[:], updatedObj.OwnerBytes())
	}

	t.Logf("transfer success: ownership changed from %x to %x", oldOwner[:4], newOwner[:4])
}

// TestTransfer_MissingCoin tests transfer without providing a coin.
func TestTransfer_MissingCoin(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	newOwner := [32]byte{9, 10, 11, 12, 13, 14, 15, 16}
	input := buildTransferInputNoCoin(newOwner)

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != errMissingObject {
		t.Errorf("expected ERR_MISSING_OBJECT (%d), got error=%d", errMissingObject, result.Error())
	}
}

// TestTransfer_InvalidArgs tests transfer with malformed arguments.
func TestTransfer_InvalidArgs(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	coinBalance := uint64(1000)
	owner := [32]byte{1, 2, 3, 4, 5, 6, 7, 8}
	input := buildTransferInputInvalidArgs(coinBalance, owner)

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != errInvalidArgs {
		t.Errorf("expected ERR_INVALID_ARGS (%d), got error=%d", errInvalidArgs, result.Error())
	}
}

// =============================================================================
// Split Function Tests
// =============================================================================

// TestSplit_Success tests splitting a coin into two.
func TestSplit_Success(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	sourceBalance := uint64(1000)
	splitAmount := uint64(300)
	sourceOwner := [32]byte{1, 2, 3, 4, 5, 6, 7, 8}
	newOwner := [32]byte{9, 10, 11, 12, 13, 14, 15, 16}

	input := buildSplitInput(sourceBalance, sourceOwner, splitAmount, newOwner)

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != 0 {
		t.Fatalf("expected success (error=0), got error=%d", result.Error())
	}

	// Verify updated source coin
	if result.UpdatedObjectsLength() != 1 {
		t.Fatalf("expected 1 updated object, got %d", result.UpdatedObjectsLength())
	}

	var updatedObj types.Object
	result.UpdatedObjects(&updatedObj, 0)
	newSourceBalance := decodeCoinBalance(updatedObj.ContentBytes())

	expectedSourceBalance := sourceBalance - splitAmount
	if newSourceBalance != expectedSourceBalance {
		t.Errorf("source: expected %d, got %d", expectedSourceBalance, newSourceBalance)
	}

	// Verify created coin
	if result.CreatedObjectsLength() != 1 {
		t.Fatalf("expected 1 created object, got %d", result.CreatedObjectsLength())
	}

	var createdObj types.Object
	result.CreatedObjects(&createdObj, 0)
	newCoinBalance := decodeCoinBalance(createdObj.ContentBytes())

	if newCoinBalance != splitAmount {
		t.Errorf("new coin: expected %d, got %d", splitAmount, newCoinBalance)
	}

	if !bytes.Equal(createdObj.OwnerBytes(), newOwner[:]) {
		t.Errorf("new coin owner: expected %x, got %x", newOwner[:], createdObj.OwnerBytes())
	}

	t.Logf("split success: %d -> %d + %d", sourceBalance, newSourceBalance, newCoinBalance)
}

// TestSplit_ExactBalance tests splitting the entire balance.
func TestSplit_ExactBalance(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	sourceBalance := uint64(1000)
	sourceOwner := [32]byte{1, 2, 3, 4, 5, 6, 7, 8}
	newOwner := [32]byte{9, 10, 11, 12, 13, 14, 15, 16}

	input := buildSplitInput(sourceBalance, sourceOwner, sourceBalance, newOwner)

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != 0 {
		t.Fatalf("expected success, got error=%d", result.Error())
	}

	var updatedObj types.Object
	result.UpdatedObjects(&updatedObj, 0)

	if decodeCoinBalance(updatedObj.ContentBytes()) != 0 {
		t.Errorf("source should be empty after exact split")
	}

	var createdObj types.Object
	result.CreatedObjects(&createdObj, 0)

	if decodeCoinBalance(createdObj.ContentBytes()) != sourceBalance {
		t.Errorf("new coin should have full amount")
	}
}

// TestSplit_InsufficientBalance tests split with insufficient balance.
func TestSplit_InsufficientBalance(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	sourceBalance := uint64(100)
	splitAmount := uint64(200)
	sourceOwner := [32]byte{1, 2, 3, 4, 5, 6, 7, 8}
	newOwner := [32]byte{9, 10, 11, 12, 13, 14, 15, 16}

	input := buildSplitInput(sourceBalance, sourceOwner, splitAmount, newOwner)

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != errInsufficientBalance {
		t.Errorf("expected ERR_INSUFFICIENT_BALANCE (%d), got error=%d",
			errInsufficientBalance, result.Error())
	}

	if result.UpdatedObjectsLength() != 0 {
		t.Errorf("expected 0 updated objects on error")
	}

	if result.CreatedObjectsLength() != 0 {
		t.Errorf("expected 0 created objects on error")
	}
}

// TestSplit_MissingCoin tests split without source coin.
func TestSplit_MissingCoin(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	splitAmount := uint64(100)
	newOwner := [32]byte{9, 10, 11, 12, 13, 14, 15, 16}

	input := buildSplitInputNoCoin(splitAmount, newOwner)

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != errMissingObject {
		t.Errorf("expected ERR_MISSING_OBJECT (%d), got error=%d", errMissingObject, result.Error())
	}
}

// TestSplit_InvalidArgs tests split with malformed arguments.
func TestSplit_InvalidArgs(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	sourceBalance := uint64(1000)
	sourceOwner := [32]byte{1, 2, 3, 4, 5, 6, 7, 8}

	input := buildSplitInputInvalidArgs(sourceBalance, sourceOwner)

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != errInvalidArgs {
		t.Errorf("expected ERR_INVALID_ARGS (%d), got error=%d", errInvalidArgs, result.Error())
	}
}

// =============================================================================
// Merge Function Tests
// =============================================================================

// TestMerge_Success tests merging multiple coins into one.
func TestMerge_Success(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	balances := []uint64{1000, 500, 300}
	owner := [32]byte{1, 2, 3, 4, 5, 6, 7, 8}

	input := buildMergeInput(balances, owner)

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != 0 {
		t.Fatalf("expected success (error=0), got error=%d", result.Error())
	}

	// Only destination coin should be updated
	if result.UpdatedObjectsLength() != 1 {
		t.Fatalf("expected 1 updated object, got %d", result.UpdatedObjectsLength())
	}

	var mergedObj types.Object
	result.UpdatedObjects(&mergedObj, 0)

	totalBalance := uint64(0)
	for _, b := range balances {
		totalBalance += b
	}

	mergedBalance := decodeCoinBalance(mergedObj.ContentBytes())
	if mergedBalance != totalBalance {
		t.Errorf("expected balance %d, got %d", totalBalance, mergedBalance)
	}

	t.Logf("merge success: %v -> %d", balances, mergedBalance)
}

// TestMerge_TwoCoins tests merging exactly two coins.
func TestMerge_TwoCoins(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	balances := []uint64{700, 300}
	owner := [32]byte{1, 2, 3, 4, 5, 6, 7, 8}

	input := buildMergeInput(balances, owner)

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != 0 {
		t.Fatalf("expected success, got error=%d", result.Error())
	}

	var mergedObj types.Object
	result.UpdatedObjects(&mergedObj, 0)

	if decodeCoinBalance(mergedObj.ContentBytes()) != 1000 {
		t.Errorf("expected total balance 1000")
	}
}

// TestMerge_InsufficientCoins tests merge with only one coin.
func TestMerge_InsufficientCoins(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	balances := []uint64{1000}
	owner := [32]byte{1, 2, 3, 4, 5, 6, 7, 8}

	input := buildMergeInput(balances, owner)

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != errInsufficientCoins {
		t.Errorf("expected ERR_INSUFFICIENT_COINS (%d), got error=%d",
			errInsufficientCoins, result.Error())
	}
}

// TestMerge_InvalidArgs tests merge with malformed arguments.
func TestMerge_InvalidArgs(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	balances := []uint64{1000, 500}
	owner := [32]byte{1, 2, 3, 4, 5, 6, 7, 8}

	input := buildMergeInputInvalidArgs(balances, owner)

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != errInvalidArgs {
		t.Errorf("expected ERR_INVALID_ARGS (%d), got error=%d", errInvalidArgs, result.Error())
	}
}

// =============================================================================
// Register Validator Tests
// =============================================================================

// TestRegisterValidator_Success tests validator registration and verifies the created object.
func TestRegisterValidator_Success(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	// Sender pubkey is used as validator pubkey
	senderPubkey := [32]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}
	httpAddr := []byte("192.168.1.1:8080")
	quicAddr := []byte("192.168.1.1:9000")

	input := buildRegisterValidatorInput(senderPubkey, httpAddr, quicAddr)

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != 0 {
		t.Fatalf("expected success (error=0), got error=%d", result.Error())
	}

	// Verify created object
	if result.CreatedObjectsLength() != 1 {
		t.Fatalf("expected 1 created object, got %d", result.CreatedObjectsLength())
	}

	var createdObj types.Object
	if !result.CreatedObjects(&createdObj, 0) {
		t.Fatal("failed to get created object")
	}

	// Verify it's a singleton (replication=0)
	if createdObj.Replication() != 0 {
		t.Errorf("expected replication=0 (singleton), got %d", createdObj.Replication())
	}

	// Verify owner is sender
	if !bytes.Equal(createdObj.OwnerBytes(), senderPubkey[:]) {
		t.Error("owner should be sender pubkey")
	}

	// Parse the Validator FlatBuffer content
	content := createdObj.ContentBytes()
	validator := types.GetRootAsValidator(content, 0)

	// Verify validator fields
	if !bytes.Equal(validator.PubkeyBytes(), senderPubkey[:]) {
		t.Error("validator pubkey should match sender")
	}

	if !bytes.Equal(validator.HttpAddressBytes(), httpAddr) {
		t.Errorf("http_address mismatch: got %s", validator.HttpAddressBytes())
	}

	if !bytes.Equal(validator.QuicAddressBytes(), quicAddr) {
		t.Errorf("quic_address mismatch: got %s", validator.QuicAddressBytes())
	}

	t.Logf("validator created: pubkey=%x, http=%s, quic=%s",
		validator.PubkeyBytes()[:8], validator.HttpAddressBytes(), validator.QuicAddressBytes())
}

// TestRegisterValidator_InvalidArgs tests registration with malformed args.
func TestRegisterValidator_InvalidArgs(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	input := buildRegisterValidatorInputInvalidArgs()

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != errInvalidArgs {
		t.Errorf("expected ERR_INVALID_ARGS (%d), got error=%d", errInvalidArgs, result.Error())
	}

	// No objects should be created on error
	if result.CreatedObjectsLength() != 0 {
		t.Errorf("expected 0 created objects on error, got %d", result.CreatedObjectsLength())
	}
}

// =============================================================================
// Unknown Function Tests
// =============================================================================

// TestUnknownFunction tests calling a function that doesn't exist.
func TestUnknownFunction(t *testing.T) {
	pool, wasmID := loadSystemPod(t)
	defer pool.Close()

	input := buildUnknownFunctionInput()

	output, _, err := pool.Execute(wasmID, input, 100000)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	// ERR_UNKNOWN_FUNCTION = 2
	if result.Error() != 2 {
		t.Errorf("expected ERR_UNKNOWN_FUNCTION (2), got error=%d", result.Error())
	}
}

// =============================================================================
// Helper Functions
// =============================================================================

// loadSystemPod loads the pod-system WASM and returns the pool and module ID.
func loadSystemPod(t *testing.T) (*Pool, [32]byte) {
	t.Helper()

	wasmPath := findSystemWasm(t)

	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("failed to read wasm: %v", err)
	}

	pool := New()

	id, err := pool.Load(wasmBytes, nil)
	if err != nil {
		pool.Close()
		t.Fatalf("failed to load module: %v", err)
	}

	return pool, id
}

// findSystemWasm locates the pod-system WASM file.
func findSystemWasm(t *testing.T) string {
	t.Helper()

	paths := []string{
		"../../pods/pod-system/target/wasm32-unknown-unknown/debug/pod_system.wasm",
		"pods/pod-system/target/wasm32-unknown-unknown/debug/pod_system.wasm",
	}

	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}

		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}

	t.Skip("pod_system.wasm not found, run: cd pods/pod-system && make build")

	return ""
}

// =============================================================================
// Encoding/Decoding Helpers
// =============================================================================

// encodeCoin encodes a Coin struct in borsh format (u64 little-endian).
func encodeCoin(balance uint64) []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, balance)

	return buf
}

// decodeCoinBalance decodes a Coin balance from borsh format.
func decodeCoinBalance(data []byte) uint64 {
	if len(data) < 8 {
		return 0
	}

	return binary.LittleEndian.Uint64(data)
}

// encodeMintArgs encodes mint args in borsh format.
// Format: u64 amount + [u8; 32] owner
func encodeMintArgs(amount uint64, owner [32]byte) []byte {
	buf := make([]byte, 8+32)
	binary.LittleEndian.PutUint64(buf[0:8], amount)
	copy(buf[8:], owner[:])

	return buf
}

// encodeTransferArgs encodes transfer args in borsh format.
// Format: [u8; 32] new_owner
func encodeTransferArgs(newOwner [32]byte) []byte {
	return newOwner[:]
}

// encodeSplitArgs encodes split args in borsh format.
// Format: u64 amount + [u8; 32] new_owner
func encodeSplitArgs(amount uint64, newOwner [32]byte) []byte {
	buf := make([]byte, 8+32)
	binary.LittleEndian.PutUint64(buf[0:8], amount)
	copy(buf[8:], newOwner[:])

	return buf
}

// encodeMergeArgs encodes merge args in borsh format (empty struct).
func encodeMergeArgs() []byte {
	return []byte{}
}

// encodeRegisterValidatorArgs encodes RegisterValidator args in borsh format.
// Format: u32 len + http_address bytes + u32 len + quic_address bytes
// (pubkey is taken from sender, not args)
func encodeRegisterValidatorArgs(httpAddr, quicAddr []byte) []byte {
	buf := make([]byte, 0, 4+len(httpAddr)+4+len(quicAddr))

	// http_address (Vec<u8>: u32 length + bytes)
	lenBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(lenBuf, uint32(len(httpAddr)))
	buf = append(buf, lenBuf...)
	buf = append(buf, httpAddr...)

	// quic_address (Vec<u8>: u32 length + bytes)
	binary.LittleEndian.PutUint32(lenBuf, uint32(len(quicAddr)))
	buf = append(buf, lenBuf...)
	buf = append(buf, quicAddr...)

	return buf
}

// =============================================================================
// Input Builders - Mint
// =============================================================================

// buildMintInput creates a PodExecuteInput for the "mint" function.
func buildMintInput(amount uint64, owner [32]byte) []byte {
	builder := flatbuffers.NewBuilder(512)

	txArgs := builder.CreateByteVector(encodeMintArgs(amount, owner))
	txHash := builder.CreateByteVector(make([]byte, 32))
	txSender := builder.CreateByteVector(make([]byte, 32))
	txPod := builder.CreateByteVector(make([]byte, 32))
	txFuncName := builder.CreateString("mint")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, txHash)
	types.TransactionAddSender(builder, txSender)
	types.TransactionAddPod(builder, txPod)
	types.TransactionAddFunctionName(builder, txFuncName)
	types.TransactionAddArgs(builder, txArgs)
	tx := types.TransactionEnd(builder)

	sender := builder.CreateByteVector(make([]byte, 32))

	types.PodExecuteInputStart(builder)
	types.PodExecuteInputAddTransaction(builder, tx)
	types.PodExecuteInputAddSender(builder, sender)
	input := types.PodExecuteInputEnd(builder)

	types.FinishPodExecuteInputBuffer(builder, input)

	return builder.FinishedBytes()
}

// buildMintInputInvalidArgs creates a mint input with malformed arguments.
func buildMintInputInvalidArgs() []byte {
	builder := flatbuffers.NewBuilder(512)

	txArgs := builder.CreateByteVector([]byte{1, 2, 3, 4})
	txHash := builder.CreateByteVector(make([]byte, 32))
	txSender := builder.CreateByteVector(make([]byte, 32))
	txPod := builder.CreateByteVector(make([]byte, 32))
	txFuncName := builder.CreateString("mint")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, txHash)
	types.TransactionAddSender(builder, txSender)
	types.TransactionAddPod(builder, txPod)
	types.TransactionAddFunctionName(builder, txFuncName)
	types.TransactionAddArgs(builder, txArgs)
	tx := types.TransactionEnd(builder)

	sender := builder.CreateByteVector(make([]byte, 32))

	types.PodExecuteInputStart(builder)
	types.PodExecuteInputAddTransaction(builder, tx)
	types.PodExecuteInputAddSender(builder, sender)
	input := types.PodExecuteInputEnd(builder)

	types.FinishPodExecuteInputBuffer(builder, input)

	return builder.FinishedBytes()
}

// =============================================================================
// Input Builders - Transfer
// =============================================================================

// buildTransferInput creates a PodExecuteInput for the "transfer" function.
func buildTransferInput(coinBalance uint64, oldOwner, newOwner [32]byte) []byte {
	builder := flatbuffers.NewBuilder(512)

	txArgs := builder.CreateByteVector(encodeTransferArgs(newOwner))
	txHash := builder.CreateByteVector(make([]byte, 32))
	txSender := builder.CreateByteVector(make([]byte, 32))
	txPod := builder.CreateByteVector(make([]byte, 32))
	txFuncName := builder.CreateString("transfer")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, txHash)
	types.TransactionAddSender(builder, txSender)
	types.TransactionAddPod(builder, txPod)
	types.TransactionAddFunctionName(builder, txFuncName)
	types.TransactionAddArgs(builder, txArgs)
	tx := types.TransactionEnd(builder)

	coin := buildCoinObjectWithOwner(builder, coinBalance, oldOwner)

	types.PodExecuteInputStartLocalObjectsVector(builder, 1)
	builder.PrependUOffsetT(coin)
	localObjects := builder.EndVector(1)

	sender := builder.CreateByteVector(make([]byte, 32))

	types.PodExecuteInputStart(builder)
	types.PodExecuteInputAddTransaction(builder, tx)
	types.PodExecuteInputAddSender(builder, sender)
	types.PodExecuteInputAddLocalObjects(builder, localObjects)
	input := types.PodExecuteInputEnd(builder)

	types.FinishPodExecuteInputBuffer(builder, input)

	return builder.FinishedBytes()
}

// buildTransferInputNoCoin creates transfer input without a coin.
func buildTransferInputNoCoin(newOwner [32]byte) []byte {
	builder := flatbuffers.NewBuilder(512)

	txArgs := builder.CreateByteVector(encodeTransferArgs(newOwner))
	txHash := builder.CreateByteVector(make([]byte, 32))
	txSender := builder.CreateByteVector(make([]byte, 32))
	txPod := builder.CreateByteVector(make([]byte, 32))
	txFuncName := builder.CreateString("transfer")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, txHash)
	types.TransactionAddSender(builder, txSender)
	types.TransactionAddPod(builder, txPod)
	types.TransactionAddFunctionName(builder, txFuncName)
	types.TransactionAddArgs(builder, txArgs)
	tx := types.TransactionEnd(builder)

	sender := builder.CreateByteVector(make([]byte, 32))

	types.PodExecuteInputStart(builder)
	types.PodExecuteInputAddTransaction(builder, tx)
	types.PodExecuteInputAddSender(builder, sender)
	input := types.PodExecuteInputEnd(builder)

	types.FinishPodExecuteInputBuffer(builder, input)

	return builder.FinishedBytes()
}

// buildTransferInputInvalidArgs creates transfer input with malformed args.
func buildTransferInputInvalidArgs(coinBalance uint64, owner [32]byte) []byte {
	builder := flatbuffers.NewBuilder(512)

	txArgs := builder.CreateByteVector([]byte{1, 2, 3, 4})
	txHash := builder.CreateByteVector(make([]byte, 32))
	txSender := builder.CreateByteVector(make([]byte, 32))
	txPod := builder.CreateByteVector(make([]byte, 32))
	txFuncName := builder.CreateString("transfer")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, txHash)
	types.TransactionAddSender(builder, txSender)
	types.TransactionAddPod(builder, txPod)
	types.TransactionAddFunctionName(builder, txFuncName)
	types.TransactionAddArgs(builder, txArgs)
	tx := types.TransactionEnd(builder)

	coin := buildCoinObjectWithOwner(builder, coinBalance, owner)

	types.PodExecuteInputStartLocalObjectsVector(builder, 1)
	builder.PrependUOffsetT(coin)
	localObjects := builder.EndVector(1)

	sender := builder.CreateByteVector(make([]byte, 32))

	types.PodExecuteInputStart(builder)
	types.PodExecuteInputAddTransaction(builder, tx)
	types.PodExecuteInputAddSender(builder, sender)
	types.PodExecuteInputAddLocalObjects(builder, localObjects)
	input := types.PodExecuteInputEnd(builder)

	types.FinishPodExecuteInputBuffer(builder, input)

	return builder.FinishedBytes()
}

// =============================================================================
// Input Builders - Split
// =============================================================================

// buildSplitInput creates a PodExecuteInput for the "split" function.
func buildSplitInput(sourceBalance uint64, sourceOwner [32]byte, splitAmount uint64, newOwner [32]byte) []byte {
	builder := flatbuffers.NewBuilder(512)

	txArgs := builder.CreateByteVector(encodeSplitArgs(splitAmount, newOwner))
	txHash := builder.CreateByteVector(make([]byte, 32))
	txSender := builder.CreateByteVector(make([]byte, 32))
	txPod := builder.CreateByteVector(make([]byte, 32))
	txFuncName := builder.CreateString("split")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, txHash)
	types.TransactionAddSender(builder, txSender)
	types.TransactionAddPod(builder, txPod)
	types.TransactionAddFunctionName(builder, txFuncName)
	types.TransactionAddArgs(builder, txArgs)
	tx := types.TransactionEnd(builder)

	coin := buildCoinObjectWithOwner(builder, sourceBalance, sourceOwner)

	types.PodExecuteInputStartLocalObjectsVector(builder, 1)
	builder.PrependUOffsetT(coin)
	localObjects := builder.EndVector(1)

	sender := builder.CreateByteVector(make([]byte, 32))

	types.PodExecuteInputStart(builder)
	types.PodExecuteInputAddTransaction(builder, tx)
	types.PodExecuteInputAddSender(builder, sender)
	types.PodExecuteInputAddLocalObjects(builder, localObjects)
	input := types.PodExecuteInputEnd(builder)

	types.FinishPodExecuteInputBuffer(builder, input)

	return builder.FinishedBytes()
}

// buildSplitInputNoCoin creates split input without source coin.
func buildSplitInputNoCoin(splitAmount uint64, newOwner [32]byte) []byte {
	builder := flatbuffers.NewBuilder(512)

	txArgs := builder.CreateByteVector(encodeSplitArgs(splitAmount, newOwner))
	txHash := builder.CreateByteVector(make([]byte, 32))
	txSender := builder.CreateByteVector(make([]byte, 32))
	txPod := builder.CreateByteVector(make([]byte, 32))
	txFuncName := builder.CreateString("split")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, txHash)
	types.TransactionAddSender(builder, txSender)
	types.TransactionAddPod(builder, txPod)
	types.TransactionAddFunctionName(builder, txFuncName)
	types.TransactionAddArgs(builder, txArgs)
	tx := types.TransactionEnd(builder)

	sender := builder.CreateByteVector(make([]byte, 32))

	types.PodExecuteInputStart(builder)
	types.PodExecuteInputAddTransaction(builder, tx)
	types.PodExecuteInputAddSender(builder, sender)
	input := types.PodExecuteInputEnd(builder)

	types.FinishPodExecuteInputBuffer(builder, input)

	return builder.FinishedBytes()
}

// buildSplitInputInvalidArgs creates split input with malformed args.
func buildSplitInputInvalidArgs(sourceBalance uint64, sourceOwner [32]byte) []byte {
	builder := flatbuffers.NewBuilder(512)

	txArgs := builder.CreateByteVector([]byte{1, 2, 3, 4})
	txHash := builder.CreateByteVector(make([]byte, 32))
	txSender := builder.CreateByteVector(make([]byte, 32))
	txPod := builder.CreateByteVector(make([]byte, 32))
	txFuncName := builder.CreateString("split")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, txHash)
	types.TransactionAddSender(builder, txSender)
	types.TransactionAddPod(builder, txPod)
	types.TransactionAddFunctionName(builder, txFuncName)
	types.TransactionAddArgs(builder, txArgs)
	tx := types.TransactionEnd(builder)

	coin := buildCoinObjectWithOwner(builder, sourceBalance, sourceOwner)

	types.PodExecuteInputStartLocalObjectsVector(builder, 1)
	builder.PrependUOffsetT(coin)
	localObjects := builder.EndVector(1)

	sender := builder.CreateByteVector(make([]byte, 32))

	types.PodExecuteInputStart(builder)
	types.PodExecuteInputAddTransaction(builder, tx)
	types.PodExecuteInputAddSender(builder, sender)
	types.PodExecuteInputAddLocalObjects(builder, localObjects)
	input := types.PodExecuteInputEnd(builder)

	types.FinishPodExecuteInputBuffer(builder, input)

	return builder.FinishedBytes()
}

// =============================================================================
// Input Builders - Merge
// =============================================================================

// buildMergeInput creates a PodExecuteInput for the "merge" function.
func buildMergeInput(balances []uint64, owner [32]byte) []byte {
	builder := flatbuffers.NewBuilder(512)

	txArgs := builder.CreateByteVector(encodeMergeArgs())
	txHash := builder.CreateByteVector(make([]byte, 32))
	txSender := builder.CreateByteVector(make([]byte, 32))
	txPod := builder.CreateByteVector(make([]byte, 32))
	txFuncName := builder.CreateString("merge")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, txHash)
	types.TransactionAddSender(builder, txSender)
	types.TransactionAddPod(builder, txPod)
	types.TransactionAddFunctionName(builder, txFuncName)
	types.TransactionAddArgs(builder, txArgs)
	tx := types.TransactionEnd(builder)

	// Build coin objects first (bottom-up construction)
	coins := make([]flatbuffers.UOffsetT, len(balances))
	for i := 0; i < len(balances); i++ {
		coins[i] = buildCoinObjectWithOwner(builder, balances[i], owner)
	}

	// Then create the vector
	types.PodExecuteInputStartLocalObjectsVector(builder, len(balances))
	for i := len(coins) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(coins[i])
	}
	localObjects := builder.EndVector(len(balances))

	sender := builder.CreateByteVector(make([]byte, 32))

	types.PodExecuteInputStart(builder)
	types.PodExecuteInputAddTransaction(builder, tx)
	types.PodExecuteInputAddSender(builder, sender)
	types.PodExecuteInputAddLocalObjects(builder, localObjects)
	input := types.PodExecuteInputEnd(builder)

	types.FinishPodExecuteInputBuffer(builder, input)

	return builder.FinishedBytes()
}

// buildMergeInputInvalidArgs creates merge input with malformed args.
func buildMergeInputInvalidArgs(balances []uint64, owner [32]byte) []byte {
	builder := flatbuffers.NewBuilder(512)

	txArgs := builder.CreateByteVector([]byte{1, 2, 3, 4})
	txHash := builder.CreateByteVector(make([]byte, 32))
	txSender := builder.CreateByteVector(make([]byte, 32))
	txPod := builder.CreateByteVector(make([]byte, 32))
	txFuncName := builder.CreateString("merge")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, txHash)
	types.TransactionAddSender(builder, txSender)
	types.TransactionAddPod(builder, txPod)
	types.TransactionAddFunctionName(builder, txFuncName)
	types.TransactionAddArgs(builder, txArgs)
	tx := types.TransactionEnd(builder)

	// Build coin objects first (bottom-up construction)
	coins := make([]flatbuffers.UOffsetT, len(balances))
	for i := 0; i < len(balances); i++ {
		coins[i] = buildCoinObjectWithOwner(builder, balances[i], owner)
	}

	// Then create the vector
	types.PodExecuteInputStartLocalObjectsVector(builder, len(balances))
	for i := len(coins) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(coins[i])
	}
	localObjects := builder.EndVector(len(balances))

	sender := builder.CreateByteVector(make([]byte, 32))

	types.PodExecuteInputStart(builder)
	types.PodExecuteInputAddTransaction(builder, tx)
	types.PodExecuteInputAddSender(builder, sender)
	types.PodExecuteInputAddLocalObjects(builder, localObjects)
	input := types.PodExecuteInputEnd(builder)

	types.FinishPodExecuteInputBuffer(builder, input)

	return builder.FinishedBytes()
}

// =============================================================================
// Input Builders - Register Validator
// =============================================================================

// buildRegisterValidatorInput creates input for register_validator function.
// senderPubkey is used as both the tx sender and the validator pubkey.
func buildRegisterValidatorInput(senderPubkey [32]byte, httpAddr, quicAddr []byte) []byte {
	builder := flatbuffers.NewBuilder(512)

	txArgs := builder.CreateByteVector(encodeRegisterValidatorArgs(httpAddr, quicAddr))
	txHash := builder.CreateByteVector(make([]byte, 32))
	txSender := builder.CreateByteVector(senderPubkey[:])
	txPod := builder.CreateByteVector(make([]byte, 32))
	txFuncName := builder.CreateString("register_validator")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, txHash)
	types.TransactionAddSender(builder, txSender)
	types.TransactionAddPod(builder, txPod)
	types.TransactionAddFunctionName(builder, txFuncName)
	types.TransactionAddArgs(builder, txArgs)
	tx := types.TransactionEnd(builder)

	sender := builder.CreateByteVector(senderPubkey[:])

	types.PodExecuteInputStart(builder)
	types.PodExecuteInputAddTransaction(builder, tx)
	types.PodExecuteInputAddSender(builder, sender)
	input := types.PodExecuteInputEnd(builder)

	types.FinishPodExecuteInputBuffer(builder, input)

	return builder.FinishedBytes()
}

// buildRegisterValidatorInputInvalidArgs creates input with malformed args.
func buildRegisterValidatorInputInvalidArgs() []byte {
	builder := flatbuffers.NewBuilder(512)

	txArgs := builder.CreateByteVector([]byte{1, 2, 3, 4, 5})
	txHash := builder.CreateByteVector(make([]byte, 32))
	txSender := builder.CreateByteVector(make([]byte, 32))
	txPod := builder.CreateByteVector(make([]byte, 32))
	txFuncName := builder.CreateString("register_validator")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, txHash)
	types.TransactionAddSender(builder, txSender)
	types.TransactionAddPod(builder, txPod)
	types.TransactionAddFunctionName(builder, txFuncName)
	types.TransactionAddArgs(builder, txArgs)
	tx := types.TransactionEnd(builder)

	sender := builder.CreateByteVector(make([]byte, 32))

	types.PodExecuteInputStart(builder)
	types.PodExecuteInputAddTransaction(builder, tx)
	types.PodExecuteInputAddSender(builder, sender)
	input := types.PodExecuteInputEnd(builder)

	types.FinishPodExecuteInputBuffer(builder, input)

	return builder.FinishedBytes()
}

// =============================================================================
// Input Builders - Unknown Function
// =============================================================================

// buildUnknownFunctionInput creates input for an unknown function.
func buildUnknownFunctionInput() []byte {
	builder := flatbuffers.NewBuilder(512)

	txArgs := builder.CreateByteVector([]byte{})
	txHash := builder.CreateByteVector(make([]byte, 32))
	txSender := builder.CreateByteVector(make([]byte, 32))
	txPod := builder.CreateByteVector(make([]byte, 32))
	txFuncName := builder.CreateString("unknown_function")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, txHash)
	types.TransactionAddSender(builder, txSender)
	types.TransactionAddPod(builder, txPod)
	types.TransactionAddFunctionName(builder, txFuncName)
	types.TransactionAddArgs(builder, txArgs)
	tx := types.TransactionEnd(builder)

	sender := builder.CreateByteVector(make([]byte, 32))

	types.PodExecuteInputStart(builder)
	types.PodExecuteInputAddTransaction(builder, tx)
	types.PodExecuteInputAddSender(builder, sender)
	input := types.PodExecuteInputEnd(builder)

	types.FinishPodExecuteInputBuffer(builder, input)

	return builder.FinishedBytes()
}

// =============================================================================
// Object Builders
// =============================================================================

// buildCoinObject creates a Coin object in FlatBuffers format (deprecated, use buildCoinObjectWithOwner).
func buildCoinObject(builder *flatbuffers.Builder, balance uint64) flatbuffers.UOffsetT {
	return buildCoinObjectWithOwner(builder, balance, [32]byte{})
}

// buildCoinObjectWithOwner creates a Coin object with specified owner in FlatBuffers format.
func buildCoinObjectWithOwner(builder *flatbuffers.Builder, balance uint64, owner [32]byte) flatbuffers.UOffsetT {
	objContent := builder.CreateByteVector(encodeCoin(balance))
	objID := builder.CreateByteVector(make([]byte, 32))
	objOwner := builder.CreateByteVector(owner[:])

	types.ObjectStart(builder)
	types.ObjectAddId(builder, objID)
	types.ObjectAddVersion(builder, 1)
	types.ObjectAddOwner(builder, objOwner)
	types.ObjectAddContent(builder, objContent)

	return types.ObjectEnd(builder)
}
