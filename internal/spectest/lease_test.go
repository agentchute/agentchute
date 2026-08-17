package spectest

import "testing"

func TestLeaseVectors(t *testing.T) {
	vectors, err := LoadVectors("lease.json")
	if err != nil {
		t.Fatal(err)
	}
	AssertLeaseVectors(t, vectors)
}
