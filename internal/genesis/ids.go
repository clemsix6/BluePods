package genesis

import "github.com/zeebo/blake3"

// GenesisCoinID derives the deterministic ID of the initial coin seeded for an
// owner at genesis. There is no creating transaction, so the ID is fixed by a
// domain-separated hash of a constant tag and the owner pubkey.
func GenesisCoinID(owner [32]byte) [32]byte {
	h := blake3.New()
	_, _ = h.Write([]byte("bluepods/genesis/coin/v1"))
	_, _ = h.Write(owner[:])

	var id [32]byte
	copy(id[:], h.Sum(nil))

	return id
}
