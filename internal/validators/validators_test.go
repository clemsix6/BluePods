package validators

import "testing"

func TestValidatorSetAddAndLen(t *testing.T) {
	vs := NewValidatorSet([]Hash{{1}, {2}, {3}})
	if vs.Len() != 3 {
		t.Fatalf("want 3, got %d", vs.Len())
	}
	vs.Add(Hash{4}, "addr:9000", [48]byte{0xAB})
	if vs.Len() != 4 {
		t.Fatalf("want 4 after Add, got %d", vs.Len())
	}
}
