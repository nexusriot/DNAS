package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// withNetwork switches the process onto a network for the duration of one test
// and restores both the network and the retarget policy afterwards (SetNetwork
// applies the network's difficulty rule, and the suite runs with retargeting
// off — see TestMain).
func withNetwork(t *testing.T, name string) {
	t.Helper()
	prev, prevRetarget := NetworkName(), NoRetarget
	if err := SetNetwork(name); err != nil {
		t.Fatalf("set network %s: %v", name, err)
	}
	t.Cleanup(func() {
		if err := SetNetwork(prev); err != nil {
			t.Fatalf("restore network: %v", err)
		}
		NoRetarget = prevRetarget
	})
}

func TestUnknownNetworkRejected(t *testing.T) {
	if err := SetNetwork("nosuchnet"); err == nil {
		t.Fatal("expected an unknown network to be refused")
	}
	if NetworkName() != MainNet {
		t.Fatalf("a failed SetNetwork changed the network to %q", NetworkName())
	}
}

// The mainnet encoding must be byte-identical to what it was before networks
// existed: an empty id writes nothing, so no stored chain and no existing
// transaction id changes.
func TestMainnetEncodingUnchanged(t *testing.T) {
	withNetwork(t, MainNet)
	if id := NetworkID(); id != "" {
		t.Fatalf("mainnet network id must be empty, got %q", id)
	}
	if got := GenesisBlock().PrevHash; got != GenesisPrevHash {
		t.Fatalf("mainnet genesis prev hash = %q, want %q", got, GenesisPrevHash)
	}
	w, _ := wallet.New()
	tx := Transaction{From: w.Address(), To: "dnasdeadbeef", Amount: 5, Fee: 1, Nonce: 0}
	// The signing preimage ends with the memo field on mainnet; anything appended
	// after it would show up as extra bytes here.
	plain := len(tx.canonicalSigningBytes())
	withNetwork(t, TestNet)
	if withID := len(tx.canonicalSigningBytes()); withID <= plain {
		t.Fatalf("testnet preimage (%d bytes) should be longer than mainnet's (%d)", withID, plain)
	}
}

func TestGenesisDiffersPerNetwork(t *testing.T) {
	seen := map[string]string{}
	for _, name := range Networks() {
		withNetwork(t, name)
		h := GenesisBlock().Hash
		if other, dup := seen[h]; dup {
			t.Fatalf("networks %s and %s share a genesis hash", name, other)
		}
		seen[h] = name
	}
}

// A signature made on one network must not authorize the same transfer on
// another: the network id is part of the signing preimage, so the signature
// simply does not verify there.
func TestSignatureDoesNotReplayAcrossNetworks(t *testing.T) {
	withNetwork(t, TestNet)
	w, _ := wallet.New()
	other, _ := wallet.New()
	tx := Transaction{From: w.Address(), To: other.Address(), Amount: 100, Fee: 10, Nonce: 0}
	if err := tx.Sign(w); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := tx.VerifySignature(); err != nil {
		t.Fatalf("signature must verify on the network it was made for: %v", err)
	}
	withNetwork(t, RegTest)
	if err := tx.VerifySignature(); err == nil {
		t.Fatal("a testnet signature verified on regtest: cross-network replay is possible")
	}
}

// A chain built on one network must not be adoptable on another, even though
// every other parameter matches.
func TestChainFromAnotherNetworkRejected(t *testing.T) {
	withNetwork(t, RegTest)
	bc := NewBlockchain()
	miner, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	blocks := bc.Blocks()

	withNetwork(t, TestNet)
	fresh := NewBlockchain()
	if _, _, err := fresh.ReplaceChain(blocks); err == nil {
		t.Fatal("a regtest chain was adopted by a testnet node")
	}
}

func TestRegtestHoldsDifficultyFixed(t *testing.T) {
	withNetwork(t, RegTest)
	if !NoRetarget {
		t.Fatal("regtest must hold difficulty fixed")
	}
	withNetwork(t, MainNet)
	if NoRetarget {
		t.Fatal("mainnet must retarget difficulty")
	}
}
