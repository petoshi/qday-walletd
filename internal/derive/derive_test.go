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

func TestSwapKeysUseSeparateStableNamespace(t *testing.T) {
	master := [32]byte{1, 2, 3, 4}
	a, err := SwapKeys(master, "dex-order-17")
	if err != nil {
		t.Fatal(err)
	}
	b, err := SwapKeys(master, "dex-order-17")
	if err != nil {
		t.Fatal(err)
	}
	c, err := SwapKeys(master, "dex-order-18")
	if err != nil {
		t.Fatal(err)
	}
	custody, err := Keys(master, 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.Public != b.Public {
		t.Fatal("same swap ID produced different keys")
	} else if a.Public == c.Public {
		t.Fatal("different swap IDs produced the same keys")
	} else if a.Public == custody.Public {
		t.Fatal("swap derivation collided with the custody namespace")
	}
	seed := SwapSeed(master, "dex-order-17")
	if got := hex.EncodeToString(seed[:]); got != "aa2605584ef7f795bb4be46189ee4c614872e595a6bc30cf50e53ce9fa4bb308" {
		t.Fatalf("swap seed changed: %s", got)
	}
	if got := a.Public.String(); got != "qday1p2etdds8uzx7r2a60n6la4peu02raw4fxkdpcwtkd8sn7l5dyn3cqhl9lwm" {
		t.Fatalf("swap address changed: %s", got)
	}
}
