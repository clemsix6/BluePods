package aggregation

// ObjectRef identifies an object with its expected version.
type ObjectRef struct {
	ID      [32]byte // ID is the object identifier
	Version uint64   // Version is the expected object version
}

// HolderAttestation is a holder's response to an attestation request.
type HolderAttestation struct {
	ObjectID    [32]byte // ObjectID is the attested object's identifier
	Hash        [32]byte // Hash is BLAKE3(content || version)
	Signature   []byte   // Signature is the BLS signature (96 bytes)
	ObjectData  []byte   // ObjectData is the full object (only from top-1 holder)
	IsNegative  bool     // IsNegative indicates the holder does not have the object
	HolderIndex int      // HolderIndex is the holder's position in the rendezvous ordering
}

// CollectionResult holds attestations for one object.
type CollectionResult struct {
	ObjectID    [32]byte // ObjectID is the object's identifier
	ObjectData  []byte   // ObjectData is the full object content
	Replication uint16   // Replication is the object's replication factor
	Signatures  [][]byte // Signatures are the collected BLS signatures
	SignerMask  []byte   // SignerMask is a bitmap of which holders signed
	IsSingleton bool     // IsSingleton is true if object has replication=0 (no aggregation needed)
	Error       error    // Error is set if collection failed
}
