package consensus

import (
	"math"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

// =============================================================================
// Declared-operation fees: the four-site lockstep
// =============================================================================

// TestDeclaredOpFees_AgreeAcrossSites pins the kinds x sites matrix. Every
// operation kind is priced identically by the four sites that must agree
// byte-for-byte — the ingress fee (calculateTxFee/CalculateFee), the commit
// split (calculateTxFeeSplit), the summary a producer builds (buildFeeSummary)
// and the summary a receiver recomputes (validateFeeSummary) — and a summary
// that omits the operation fees is rejected. A drift between any two of them
// rejects honest vertices or lets a producer forge fees.
func TestDeclaredOpFees_AgreeAcrossSites(t *testing.T) {
	env := newOpsFeeEnv(t)
	defer env.dag.Close()

	obj := Hash{0x11}
	target := Hash{0x22}

	// A declared-operation transaction runs no metered code: its compute term is
	// the flat min_gas floor, and every operation is priced on top of it.
	base := env.params.MinGas * env.params.GasPrice

	cases := []struct {
		name  string
		op    genesis.DeclaredOp
		opFee uint64
	}{
		{"reparent", genesis.DeclaredOp{Kind: reparentOp, ObjectID: obj[:], Target: target[:]}, env.params.ReparentFee},
		{"delete", genesis.DeclaredOp{Kind: deleteOp, ObjectID: obj[:]}, env.params.DeleteFee},
		{"domain_register", registerOp("alpha", obj, 10), env.params.RentalRatePerEpoch * 10},
		{"domain_renew", renewOp("alpha", 3), env.params.RentalRatePerEpoch * 3},
		{"domain_update", updateOp("alpha", obj), env.params.DomainUpdateFee},
		{"domain_transfer", transferOp("alpha", target), env.params.DomainTransferFee},
		{"domain_delete", deleteDomainOp("alpha"), env.params.DomainDeleteFee},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			atxBytes := env.atx(t, opsFeeTx{maxGas: 1_000_000, ops: []genesis.DeclaredOp{tc.op}})
			atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
			tx := atx.Transaction(nil)

			want := base + tc.opFee

			if got := env.dag.calculateTxFee(tx, atx); got != want {
				t.Errorf("ingress fee = %d, want %d (min_gas floor + op fee)", got, want)
			}

			consumed, storage := env.dag.calculateTxFeeSplit(tx, atx)
			if consumed != want || storage != 0 {
				t.Errorf("split = (consumed %d, storage %d), want (%d, 0): op fees are consumed, never locked", consumed, storage, want)
			}

			wantSplit := SplitFee(want, env.params)

			built := env.summary(t, atxBytes)
			if built.TotalFees() != wantSplit.Total || built.TotalEpoch() != wantSplit.Epoch || built.TotalBurned() != wantSplit.Burned {
				t.Errorf("built summary = (%d, %d, %d), want (%d, %d, %d)",
					built.TotalFees(), built.TotalBurned(), built.TotalEpoch(),
					wantSplit.Total, wantSplit.Burned, wantSplit.Epoch)
			}

			if err := env.validate(t, wantSplit, atxBytes); err != nil {
				t.Errorf("the honest summary must validate, got: %v", err)
			}

			if err := env.validate(t, SplitFee(want-1, env.params), atxBytes); err == nil {
				t.Error("a summary one unit short must be rejected")
			}

			if tc.opFee == 0 {
				t.Fatal("no operation kind is free: every leaf write is priced")
			}

			if err := env.validate(t, SplitFee(base, env.params), atxBytes); err == nil {
				t.Error("a summary omitting the op fee must be rejected")
			}
		})
	}
}

// TestEveryDeclaredOpIsPriced asserts no operation kind rewrites protocol state
// for free. Register and renew buy their leaf with rent; every other kind
// rewrites or removes a leaf every node re-hashes into the anchored root, so
// each carries a flat fee. A kind priced at zero lets one min_gas transaction
// drive unbounded SMT work across the network.
func TestEveryDeclaredOpIsPriced(t *testing.T) {
	params := DefaultFeeParams()

	obj := Hash{0x11}
	target := Hash{0x22}

	cases := []struct {
		name string
		op   genesis.DeclaredOp
	}{
		{"reparent", genesis.DeclaredOp{Kind: reparentOp, ObjectID: obj[:], Target: target[:]}},
		{"delete", genesis.DeclaredOp{Kind: deleteOp, ObjectID: obj[:]}},
		{"domain_register", registerOp("alpha", obj, 1)},
		{"domain_renew", renewOp("alpha", 1)},
		{"domain_update", updateOp("alpha", obj)},
		{"domain_transfer", transferOp("alpha", target)},
		{"domain_delete", deleteDomainOp("alpha")},
	}

	for _, tc := range cases {
		if fee := declaredOpFee(tc.op, params); fee == 0 {
			t.Errorf("%s is free: every leaf write is priced", tc.name)
		}
	}
}

// TestDomainRegisterFee_RentReachesEpochPool asserts a register-for-10-epochs
// transaction pays exactly ten times the rental rate over its compute floor,
// that the whole amount is consumed into the epoch pool (rent is never a
// refundable deposit), and that the gas coin loses exactly what the pool gains.
func TestDomainRegisterFee_RentReachesEpochPool(t *testing.T) {
	env := newOpsFeeEnv(t)
	defer env.dag.Close()

	sender := Hash{0x01}
	gasCoin := Hash{0xCC}
	const startBalance = 1_000_000
	env.coins.SetObject(buildTestCoinObject(gasCoin, startBalance, sender, 0))

	atxBytes := env.atx(t, opsFeeTx{
		sender:  sender,
		gasCoin: gasCoin,
		maxGas:  env.params.MinGas,
		ops:     []genesis.DeclaredOp{registerOp("alpha", Hash{0x11}, 10)},
	})
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	split, storage, proceed := env.dag.deductFees(tx, atx, env.producer.pubKey)
	if !proceed {
		t.Fatal("deductFees must proceed for a funded gas coin")
	}

	base := env.params.MinGas * env.params.GasPrice
	wantRent := 10 * env.params.RentalRatePerEpoch

	if split.Epoch != base+wantRent {
		t.Errorf("pooled = %d, want %d (min_gas floor + 10 x rate)", split.Epoch, base+wantRent)
	}
	if storage != 0 {
		t.Errorf("withheld storage = %d, want 0: a lease pays rent, never a deposit", storage)
	}

	balance, err := readCoinBalance(env.coins.GetObject(gasCoin))
	if err != nil {
		t.Fatalf("read gas coin: %v", err)
	}
	if want := uint64(startBalance) - (base + wantRent); balance != want {
		t.Errorf("coin balance = %d, want %d (debited exactly what the pool gained)", balance, want)
	}
}

// TestOpsTxPaysMinGasCompute asserts a transaction that declares operations and
// calls no pod pays the min_gas compute floor whatever max_gas it declares —
// no pod runs, so there is no metered execution to price — while the same
// shape carrying a pod call keeps paying for the execution it asks for.
func TestOpsTxPaysMinGasCompute(t *testing.T) {
	env := newOpsFeeEnv(t)
	defer env.dag.Close()

	obj := Hash{0x11}
	target := Hash{0x22}
	op := genesis.DeclaredOp{Kind: reparentOp, ObjectID: obj[:], Target: target[:]}
	refs := []objectRef{{id: obj, version: 1}}

	const hugeGas = 1_000_000

	opsOnly := env.atx(t, opsFeeTx{maxGas: hugeGas, mutRefs: refs, ops: []genesis.DeclaredOp{op}})
	atx := types.GetRootAsAttestedTransaction(opsOnly, 0)

	want := env.params.MinGas*env.params.GasPrice + env.params.ReparentFee
	if got := env.dag.calculateTxFee(atx.Transaction(nil), atx); got != want {
		t.Errorf("ops-only fee = %d, want %d (min_gas compute, not max_gas)", got, want)
	}

	// A transaction carrying BOTH operations and a pod call is not an
	// operations transaction: it is rejected at commit, and until then it pays
	// for the execution its holders were asked to run.
	mixed := env.atx(t, opsFeeTx{maxGas: hugeGas, funcName: "run", mutRefs: refs, ops: []genesis.DeclaredOp{op}})
	mixedATX := types.GetRootAsAttestedTransaction(mixed, 0)

	wantMixed := hugeGas*env.params.GasPrice + env.params.ReparentFee
	if got := env.dag.calculateTxFee(mixedATX.Transaction(nil), mixedATX); got != wantMixed {
		t.Errorf("mixed-tx fee = %d, want %d (full compute, the floor is for ops-only)", got, wantMixed)
	}
}

// TestDomainLeaseCap_FollowsGovernedParam asserts the cap that reverts a lease
// and the rate that prices it are the same governed parameter. Lowering
// FeeParams.MaxTermEpochs reverts a term the old package constant allowed, and
// the fee charged for the longest still-allowed term is exactly rate x that
// term: the cap and the price can never be changed apart.
func TestDomainLeaseCap_FollowsGovernedParam(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	params := DefaultFeeParams()
	params.MaxTermEpochs = 4
	env.dag.SetFeeSystem(newMockCoinStore(), &params, nil)

	obj := env.object(domainAlice, Hash{0x11})

	if env.apply(domainAlice, 0, registerOp("alpha", obj, uint32(defaultMaxTermEpochs))) {
		t.Fatal("the retired package constant must no longer govern the cap")
	}
	if env.apply(domainAlice, 0, registerOp("alpha", obj, 5)) {
		t.Fatal("a term past the governed cap must revert")
	}
	if !env.apply(domainAlice, 0, registerOp("alpha", obj, 4)) {
		t.Fatal("a term at the governed cap must apply")
	}
	if got := env.leaf(t, "alpha").expiry; got != 4 {
		t.Errorf("expiry = %d, want 4", got)
	}

	atxBytes := buildOpsFeeATX(t, opsFeeTx{
		sender:  domainAlice,
		gasCoin: Hash{0xCC},
		maxGas:  params.MinGas,
		ops:     []genesis.DeclaredOp{registerOp("beta", obj, 4)},
	})
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	want := params.MinGas*params.GasPrice + 4*params.RentalRatePerEpoch
	if got := env.dag.calculateTxFee(atx.Transaction(nil), atx); got != want {
		t.Errorf("fee at the cap = %d, want %d (rate x the term the cap allows)", got, want)
	}
}

// TestDomainRentOverflow_Saturates confirms a crafted rate and term saturate
// rather than wrapping: an attacker must never be able to pick a term whose
// rent multiplies back down to a small number.
func TestDomainRentOverflow_Saturates(t *testing.T) {
	env := newOpsFeeEnv(t)
	defer env.dag.Close()

	params := DefaultFeeParams()
	params.RentalRatePerEpoch = math.MaxUint64 / 2
	env.dag.SetFeeSystem(env.coins, &params, nil)

	atxBytes := env.atx(t, opsFeeTx{
		maxGas: params.MinGas,
		ops:    []genesis.DeclaredOp{registerOp("alpha", Hash{0x11}, math.MaxUint32)},
	})
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	if got := env.dag.calculateTxFee(tx, atx); got != math.MaxUint64 {
		t.Errorf("saturated fee = %d, want %d", got, uint64(math.MaxUint64))
	}

	consumed, storage := env.dag.calculateTxFeeSplit(tx, atx)
	if consumed != math.MaxUint64 || storage != 0 {
		t.Errorf("split = (%d, %d), want (MaxUint64, 0)", consumed, storage)
	}
}

// =============================================================================
// Helpers
// =============================================================================

// opsFeeTx describes the transaction shape a fee test builds. A non-empty
// funcName makes it a pod call, which is what separates an operations
// transaction from a mixed one.
type opsFeeTx struct {
	sender   Hash                 // sender is the paying key (defaults to a fixed test key)
	gasCoin  Hash                 // gasCoin is the fee source (defaults to a fixed test coin)
	maxGas   uint64               // maxGas is the declared gas bound
	funcName string               // funcName is the pod entrypoint, empty for an operations transaction
	mutRefs  []objectRef          // mutRefs are the declared mutable references
	ops      []genesis.DeclaredOp // ops are the declared operations
	hash     Hash                 // hash is the transaction hash
}

// opsFeeEnv is a DAG with the fee system wired, used to exercise the four fee
// sites on declared-operation transactions.
type opsFeeEnv struct {
	dag      *DAG           // dag is the system under test
	coins    *mockCoinStore // coins backs the gas coin
	params   FeeParams      // params are the frozen fee parameters the DAG reads
	producer testValidator  // producer signs the vertices the validate site reads
}

// newOpsFeeEnv builds a DAG with default fee parameters and an empty coin store.
func newOpsFeeEnv(t *testing.T) *opsFeeEnv {
	t.Helper()

	validators, vs := newTestValidatorSet(3)
	dag := New(newTestStorage(t), vs, &mockBroadcaster{}, testSystemPod, 0, validators[0].privKey, nil)
	disableTxAuth(dag)

	env := &opsFeeEnv{dag: dag, coins: newMockCoinStore(), params: DefaultFeeParams(), producer: validators[0]}
	dag.SetFeeSystem(env.coins, &env.params, nil)

	return env
}

// atx builds an AttestedTransaction for the given shape, filling the sender and
// gas coin a fee test does not care about.
func (e *opsFeeEnv) atx(t *testing.T, spec opsFeeTx) []byte {
	t.Helper()

	if spec.sender == (Hash{}) {
		spec.sender = Hash{0x01}
	}
	if spec.gasCoin == (Hash{}) {
		spec.gasCoin = Hash{0xCC}
	}

	return buildOpsFeeATX(t, spec)
}

// summary runs the production site over one transaction and reads the built
// FeeSummary back.
func (e *opsFeeEnv) summary(t *testing.T, atxBytes []byte) *types.FeeSummary {
	t.Helper()

	builder := flatbuffers.NewBuilder(512)
	off := e.dag.buildFeeSummary(builder, [][]byte{atxBytes})
	builder.Finish(off)

	return types.GetRootAsFeeSummary(builder.FinishedBytes(), 0)
}

// validate runs the ingress site against a vertex declaring the given split.
func (e *opsFeeEnv) validate(t *testing.T, split FeeSplit, atxBytes []byte) error {
	t.Helper()

	data := buildVertexWithFeeSummary(t, e.producer, 0, 0,
		&feeSummaryValues{split.Total, split.Burned, split.Epoch},
		[][]byte{atxBytes},
	)

	return e.dag.validateFeeSummary(types.GetRootAsVertex(data, 0))
}

// buildOpsFeeATX builds an AttestedTransaction carrying declared operations, a
// gas coin and a max_gas, which is what the fee sites read.
func buildOpsFeeATX(t *testing.T, spec opsFeeTx) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(1024)

	mutVec := buildObjectRefVector(builder, spec.mutRefs, true)
	opsVec := buildDeclaredOpsVector(builder, spec.ops)
	hashVec := builder.CreateByteVector(spec.hash[:])
	senderVec := builder.CreateByteVector(spec.sender[:])
	podVec := builder.CreateByteVector(make([]byte, 32))
	gasVec := builder.CreateByteVector(spec.gasCoin[:])

	var funcOff flatbuffers.UOffsetT
	if spec.funcName != "" {
		funcOff = builder.CreateString(spec.funcName)
	}

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	if funcOff != 0 {
		types.TransactionAddFunctionName(builder, funcOff)
	}
	types.TransactionAddMaxGas(builder, spec.maxGas)
	types.TransactionAddGasCoin(builder, gasVec)
	if mutVec != 0 {
		types.TransactionAddMutableRefs(builder, mutVec)
	}
	types.TransactionAddOperations(builder, opsVec)
	txOff := types.TransactionEnd(builder)

	types.AttestedTransactionStartObjectsVector(builder, 0)
	objVec := builder.EndVector(0)
	types.AttestedTransactionStartProofsVector(builder, 0)
	prfVec := builder.EndVector(0)

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOff)
	types.AttestedTransactionAddObjects(builder, objVec)
	types.AttestedTransactionAddProofs(builder, prfVec)
	atxOff := types.AttestedTransactionEnd(builder)

	builder.Finish(atxOff)

	return builder.FinishedBytes()
}
