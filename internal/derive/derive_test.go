package derive

import (
	"encoding/hex"
	"testing"
)

func TestDeterministicDistinctChildren(t *testing.T) {
	master := [32]byte{1, 2, 3, 4}
	a, err := Keys(master, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Keys(master, 0)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Keys(master, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Public != b.Public {
		t.Fatal("same master and index produced different public keys")
	}
	if a.Public == c.Public {
		t.Fatal("different child indices produced the same public keys")
	}
	child := Seed(master, 0)
	if got := hex.EncodeToString(child[:]); got != "a44783bfd58305c7f2003aaca0cbac28790c4aff46573e12d99680dee25d03d3" {
		t.Fatalf("child seed changed: %s", got)
	}
	if got := a.Public.String(); got != "qday1phvrc8fs0jauhhxhcq0yktyheu35adxcy7areps987zpw327043tq6s7wkk" {
		t.Fatalf("child address changed: %s", got)
	}
}
