package daemon

import "testing"

func TestParsePeers(t *testing.T) {
	peers, err := ParsePeers("seed1.pqday.com:19771, seed2.pqday.com:19771,seed1.pqday.com:19771,[::1]:19771")
	if err != nil {
		t.Fatal(err)
	} else if len(peers) != 3 {
		t.Fatalf("got %d unique peers: %#v", len(peers), peers)
	}
	for _, invalid := range []string{"", "seed1.pqday.com", "seed1.pqday.com:0", "seed1.pqday.com:65536", ":19771", "seed1.pqday.com:qday"} {
		if _, err := ParsePeers(invalid); err == nil {
			t.Fatalf("accepted invalid peers %q", invalid)
		}
	}
}
