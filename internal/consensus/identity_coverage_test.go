package consensus

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"BluePods/internal/types"
)

// identityTableSchemas maps every FlatBuffers table the vertex identity covers to
// the schema file declaring it. The Transaction table is deliberately absent: its
// canonical rebuild is genesis.RebuildTxInBuilder, owned and covered by that
// package, and the AttestedTransaction.transaction case below is what proves the
// transaction's content reaches this vertex's body hash at all.
var identityTableSchemas = map[string]string{
	"Vertex":              "../../types/vertex.fbs",
	"VertexLink":          "../../types/vertex.fbs",
	"FeeSummary":          "../../types/vertex.fbs",
	"AttestedTransaction": "../../types/vertex.fbs",
	"QuorumProof":         "../../types/vertex.fbs",
	"Object":              "../../types/object.fbs",
}

// identityMutation names one field of one table feeding the vertex identity and
// carries a mutation that flips it in place.
type identityMutation struct {
	table  string                   // table is the FlatBuffers table declaring the field
	field  string                   // field is the field name as declared in the .fbs
	mutate func(*types.Vertex) bool // mutate flips the field, reporting whether it was present
}

// TestVertexIdentity_EveryFieldIsCovered flips every field of every table feeding
// the vertex identity, one at a time, and requires each flip to break validation:
// either the recomputed identity no longer matches the declared one, or the
// signature no longer verifies over it. A field that survives a flip is a field
// outside both hashes — a relaying peer could rewrite it and the vertex would keep
// its identity, its signature and its place in the DAG.
func TestVertexIdentity_EveryFieldIsCovered(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	pristine := buildFullVertex(t, validators[0])

	if err := dag.validateSignature(types.GetRootAsVertex(pristine, 0)); err != nil {
		t.Fatalf("the unmutated fixture must verify: %v", err)
	}

	for _, m := range identityMutations() {
		t.Run(m.table+"."+m.field, func(t *testing.T) {
			data := append([]byte(nil), pristine...)
			vertex := types.GetRootAsVertex(data, 0)

			if !m.mutate(vertex) {
				t.Fatalf("%s.%s is absent from the fixture, so the mutation applied nothing", m.table, m.field)
			}

			if err := dag.validateSignature(vertex); err == nil {
				t.Fatalf("flipping %s.%s left the identity and the signature intact: the field is covered by neither hash", m.table, m.field)
			}
		})
	}
}

// TestVertexIdentity_MutationMatrixMatchesSchema keeps the mutation matrix in
// lockstep with the schema: it reads the field names each table declares in its
// .fbs and requires exactly one mutation case per field. A field appended to a
// table and forgotten in the canonical serializer fails here, at the schema, rather
// than shipping as a field two nodes can disagree on without either noticing.
func TestVertexIdentity_MutationMatrixMatchesSchema(t *testing.T) {
	covered := make(map[string]map[string]bool, len(identityTableSchemas))

	for _, m := range identityMutations() {
		if covered[m.table] == nil {
			covered[m.table] = make(map[string]bool)
		}

		if covered[m.table][m.field] {
			t.Errorf("%s.%s has two mutation cases", m.table, m.field)
		}

		covered[m.table][m.field] = true
	}

	for table, schema := range identityTableSchemas {
		for _, field := range fbsTableFields(t, schema, table) {
			if !covered[table][field] {
				t.Errorf("%s.%s has no mutation case: add one, and make sure the canonical serializer writes the field", table, field)
			}

			delete(covered[table], field)
		}

		for field := range covered[table] {
			t.Errorf("%s.%s is mutated but not declared in %s", table, field, schema)
		}
	}
}

// fbsTableFields returns the field names a FlatBuffers table declares, read from
// the schema itself so the matrix is checked against the source of truth.
func fbsTableFields(t *testing.T, schema, table string) []string {
	t.Helper()

	src, err := os.ReadFile(schema)
	if err != nil {
		t.Fatalf("read %s: %v", schema, err)
	}

	body := tableBody(t, string(src), table, schema)

	var fields []string
	for _, m := range regexp.MustCompile(`(?m)^\s+([a-z_0-9]+)\s*:`).FindAllStringSubmatch(body, -1) {
		fields = append(fields, m[1])
	}

	if len(fields) == 0 {
		t.Fatalf("no fields parsed for table %s in %s", table, schema)
	}

	return fields
}

// tableBody returns the text between a table's braces.
func tableBody(t *testing.T, src, table, schema string) string {
	t.Helper()

	open := strings.Index(src, "table "+table+" {")
	if open < 0 {
		t.Fatalf("table %s not found in %s", table, schema)
	}

	rest := src[open:]

	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("table %s is unterminated in %s", table, schema)
	}

	return rest[:end]
}

// identityMutations enumerates one flip per declared field. Fields holding a
// nested table or a vector of them are flipped through a DIFFERENT element than
// the one that table's own cases use, so the case proves the whole vector reaches
// the body hash rather than repeating what the element's cases already prove.
func identityMutations() []identityMutation {
	mutations := []identityMutation{
		{table: "Vertex", field: "hash", mutate: func(v *types.Vertex) bool { return v.MutateHash(0, v.HashBytes()[0]^0xFF) }},
		{table: "Vertex", field: "round", mutate: func(v *types.Vertex) bool { return v.MutateRound(v.Round() + 1) }},
		{table: "Vertex", field: "producer", mutate: func(v *types.Vertex) bool { return v.MutateProducer(31, v.ProducerBytes()[31]^0xFF) }},
		{table: "Vertex", field: "signature", mutate: func(v *types.Vertex) bool { return v.MutateSignature(0, v.SignatureBytes()[0]^0xFF) }},
		{table: "Vertex", field: "epoch", mutate: func(v *types.Vertex) bool { return v.MutateEpoch(v.Epoch() + 1) }},
		{table: "Vertex", field: "timestamp", mutate: func(v *types.Vertex) bool { return v.MutateTimestamp(v.Timestamp() + 1) }},
		{table: "Vertex", field: "frontier_round", mutate: func(v *types.Vertex) bool { return v.MutateFrontierRound(v.FrontierRound() + 1) }},
		{table: "Vertex", field: "index_root", mutate: func(v *types.Vertex) bool { return v.MutateIndexRoot(0, v.IndexRootBytes()[0]^0xFF) }},
		{table: "Vertex", field: "body_hash", mutate: func(v *types.Vertex) bool { return v.MutateBodyHash(0, v.BodyHashBytes()[0]^0xFF) }},

		{table: "Vertex", field: "parents", mutate: func(v *types.Vertex) bool {
			link := fixtureLink(v, 1)
			return link.MutateHash(0, link.HashBytes()[0]^0xFF)
		}},
		{table: "Vertex", field: "transactions", mutate: func(v *types.Vertex) bool {
			atx := fixtureATX(v, 1)
			return atx.MutateAttestationEpoch(atx.AttestationEpoch() + 1)
		}},
		{table: "Vertex", field: "fee_summary", mutate: func(v *types.Vertex) bool {
			summary := v.FeeSummary(nil)
			return summary.MutateTotalEpoch(summary.TotalEpoch() + 1)
		}},

		{table: "VertexLink", field: "hash", mutate: func(v *types.Vertex) bool {
			link := fixtureLink(v, 0)
			return link.MutateHash(31, link.HashBytes()[31]^0xFF)
		}},
		{table: "VertexLink", field: "producer", mutate: func(v *types.Vertex) bool {
			link := fixtureLink(v, 0)
			return link.MutateProducer(0, link.ProducerBytes()[0]^0xFF)
		}},

		{table: "FeeSummary", field: "total_fees", mutate: func(v *types.Vertex) bool {
			summary := v.FeeSummary(nil)
			return summary.MutateTotalFees(summary.TotalFees() + 1)
		}},
		{table: "FeeSummary", field: "total_burned", mutate: func(v *types.Vertex) bool {
			summary := v.FeeSummary(nil)
			return summary.MutateTotalBurned(summary.TotalBurned() + 1)
		}},
		{table: "FeeSummary", field: "total_epoch", mutate: func(v *types.Vertex) bool {
			summary := v.FeeSummary(nil)
			return summary.MutateTotalEpoch(summary.TotalEpoch() + 7)
		}},

		{table: "AttestedTransaction", field: "transaction", mutate: func(v *types.Vertex) bool {
			tx := fixtureATX(v, 0).Transaction(nil)
			return tx.MutateHash(0, tx.HashBytes()[0]^0xFF)
		}},
		{table: "AttestedTransaction", field: "attestation_epoch", mutate: func(v *types.Vertex) bool {
			atx := fixtureATX(v, 0)
			return atx.MutateAttestationEpoch(atx.AttestationEpoch() + 1)
		}},
		{table: "AttestedTransaction", field: "objects", mutate: func(v *types.Vertex) bool {
			obj := fixtureObject(v, 0, 1)
			return obj.MutateVersion(obj.Version() + 1)
		}},
		{table: "AttestedTransaction", field: "proofs", mutate: func(v *types.Vertex) bool {
			proof := fixtureProof(v, 0, 1)
			return proof.MutateObjectId(0, proof.ObjectIdBytes()[0]^0xFF)
		}},
	}

	return append(mutations, objectAndProofMutations()...)
}

// objectAndProofMutations enumerates the Object and QuorumProof fields, all
// flipped on the first element of the first attested transaction.
func objectAndProofMutations() []identityMutation {
	return []identityMutation{
		{table: "Object", field: "id", mutate: func(v *types.Vertex) bool {
			obj := fixtureObject(v, 0, 0)
			return obj.MutateId(0, obj.IdBytes()[0]^0xFF)
		}},
		{table: "Object", field: "version", mutate: func(v *types.Vertex) bool {
			obj := fixtureObject(v, 0, 0)
			return obj.MutateVersion(obj.Version() + 1)
		}},
		{table: "Object", field: "owner", mutate: func(v *types.Vertex) bool {
			obj := fixtureObject(v, 0, 0)
			return obj.MutateOwner(0, obj.OwnerBytes()[0]^0xFF)
		}},
		{table: "Object", field: "replication", mutate: func(v *types.Vertex) bool {
			obj := fixtureObject(v, 0, 0)
			return obj.MutateReplication(obj.Replication() + 1)
		}},
		{table: "Object", field: "content", mutate: func(v *types.Vertex) bool {
			obj := fixtureObject(v, 0, 0)
			return obj.MutateContent(0, obj.ContentBytes()[0]^0xFF)
		}},
		{table: "Object", field: "fees", mutate: func(v *types.Vertex) bool {
			obj := fixtureObject(v, 0, 0)
			return obj.MutateFees(obj.Fees() + 1)
		}},
		{table: "Object", field: "parent_kind", mutate: func(v *types.Vertex) bool {
			obj := fixtureObject(v, 0, 0)
			return obj.MutateParentKind(obj.ParentKind() ^ 0xFF)
		}},

		{table: "QuorumProof", field: "object_id", mutate: func(v *types.Vertex) bool {
			proof := fixtureProof(v, 0, 0)
			return proof.MutateObjectId(31, proof.ObjectIdBytes()[31]^0xFF)
		}},
		{table: "QuorumProof", field: "bls_signature", mutate: func(v *types.Vertex) bool {
			proof := fixtureProof(v, 0, 0)
			return proof.MutateBlsSignature(0, proof.BlsSignatureBytes()[0]^0xFF)
		}},
		{table: "QuorumProof", field: "signer_bitmap", mutate: func(v *types.Vertex) bool {
			proof := fixtureProof(v, 0, 0)
			return proof.MutateSignerBitmap(0, proof.SignerBitmapBytes()[0]^0xFF)
		}},
	}
}

// fixtureLink returns the vertex's nth parent link.
func fixtureLink(v *types.Vertex, n int) *types.VertexLink {
	var link types.VertexLink
	v.Parents(&link, n)

	return &link
}

// fixtureATX returns the vertex's nth attested transaction.
func fixtureATX(v *types.Vertex, n int) *types.AttestedTransaction {
	var atx types.AttestedTransaction
	v.Transactions(&atx, n)

	return &atx
}

// fixtureObject returns the objIdx-th object of the atxIdx-th attested transaction.
func fixtureObject(v *types.Vertex, atxIdx, objIdx int) *types.Object {
	var obj types.Object
	fixtureATX(v, atxIdx).Objects(&obj, objIdx)

	return &obj
}

// fixtureProof returns the proofIdx-th proof of the atxIdx-th attested transaction.
func fixtureProof(v *types.Vertex, atxIdx, proofIdx int) *types.QuorumProof {
	var proof types.QuorumProof
	fixtureATX(v, atxIdx).Proofs(&proof, proofIdx)

	return &proof
}
