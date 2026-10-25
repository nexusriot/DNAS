package node

import "testing"

func TestFetchCFHeadersAgreesWithNode(t *testing.T) {
	addr := freeAddr(t)
	n := startTestNode(t, addr, nil, false)
	if _, err := n.Generate(3); err != nil {
		t.Fatal(err)
	}
	set, err := FetchCFHeaders(addr, nil, 0, n.chain.Height())
	if err != nil {
		t.Fatal(err)
	}
	want := n.chain.FilterHeaders()
	if set.From != 0 || len(set.Headers) != len(want) {
		t.Fatalf("got from %d, %d headers; node has %d", set.From, len(set.Headers), len(want))
	}
	for i := range want {
		if set.Headers[i] != want[i] {
			t.Fatalf("header %d differs", i)
		}
	}
}

func TestCompareCFHeaderChains(t *testing.T) {
	a := CFHeaderSet{Addr: "a", From: 0, Headers: []string{"aa", "bb"}}
	b := CFHeaderSet{Addr: "b", From: 0, Headers: []string{"aa", "bb"}}
	if err := CompareCFHeaderChains([]CFHeaderSet{a, b}); err != nil {
		t.Fatal(err)
	}
	b.Headers = []string{"aa", "cc"}
	if err := CompareCFHeaderChains([]CFHeaderSet{a, b}); err == nil {
		t.Fatal("a split was accepted")
	}
	if err := CompareCFHeaderChains([]CFHeaderSet{a}); err == nil {
		t.Fatal("one chain was treated as agreement")
	}
	short := CFHeaderSet{Addr: "c", From: 2, Headers: []string{"aa"}}
	if err := CompareCFHeaderChains([]CFHeaderSet{a, short}); err == nil {
		t.Fatal("a chain that does not cover the prefix was accepted")
	}
}
