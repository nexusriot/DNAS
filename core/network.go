package core

import (
	"fmt"
	"sort"
	"sync"
)

// Network identity separates one DNAS chain from another.
//
// Without it every DNAS node is on the same network by construction: the genesis
// block is a fixed function of the parameters, so a regtest node and a live node
// compute the same genesis hash, complete a handshake and try to converge; and a
// transaction's signing preimage names no chain, so a signature made on one
// network authorizes the identical transfer on every other one. Both are the
// same bug wearing two hats — nothing binds a chain to an identity.
//
// A network therefore has an ID, and that ID is bound in three places:
//
//   - the GENESIS block, via its PrevHash, so two networks cannot share a chain
//     even if every other parameter matches;
//   - the transaction SIGNING PREIMAGE (see codec.go), so a signature is valid on
//     exactly one network and cannot be replayed onto another;
//   - the peer HANDSHAKE (see node/protocol.go), so nodes on different networks
//     disconnect immediately instead of failing to converge.
//
// Mainnet's id is deliberately the EMPTY string, and both the genesis PrevHash
// and the signing preimage omit an empty id entirely. That makes every mainnet
// encoding byte-for-byte what it was before networks existed: no flag day, no
// changed transaction ids, and a stored chain still replays.

// Network names. A network is selected once at startup (SetNetwork) and must
// match on every node that is meant to converge.
const (
	MainNet = "mainnet" // the real chain: unbounded difficulty retargeting
	TestNet = "testnet" // a public throwaway chain with its own genesis and coin
	RegTest = "regtest" // a private local chain: fixed difficulty, blocks on demand
)

// NetworkParams is everything that distinguishes one network from another.
type NetworkParams struct {
	// Name is the operator-facing network name, also exchanged in the peer
	// handshake so mismatched nodes disconnect.
	Name string
	// ID is the consensus-visible identifier bound into the genesis block and
	// every signing preimage. Empty on mainnet (see above).
	ID string
	// DefaultNetKey is the pre-shared key a node adopts when the operator gives
	// none. Empty means an open, permissionless network.
	DefaultNetKey string
	// NoRetarget holds difficulty at the genesis target, so blocks are instant.
	NoRetarget bool
	// Faucet allows the node to give coin away on request (see the node's faucet).
	// Never true on mainnet: a faucet exists to make a throwaway chain usable.
	Faucet bool
}

var networks = map[string]NetworkParams{
	MainNet: {Name: MainNet, ID: "", DefaultNetKey: "", NoRetarget: false, Faucet: false},
	TestNet: {Name: TestNet, ID: "dnas-testnet", DefaultNetKey: "", NoRetarget: false, Faucet: true},
	RegTest: {Name: RegTest, ID: "dnas-regtest", DefaultNetKey: "dnas-regtest", NoRetarget: true, Faucet: true},
}

var (
	networkMu sync.RWMutex
	network   = networks[MainNet]
)

// Networks lists the known network names, sorted.
func Networks() []string {
	out := make([]string, 0, len(networks))
	for name := range networks {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// SetNetwork selects the network this process runs on. Call it at startup,
// before opening a chain or dialing peers — it changes the genesis hash and the
// signing preimage, so switching networks mid-run would invalidate everything
// already computed. It also applies the network's difficulty policy.
func SetNetwork(name string) error {
	p, ok := networks[name]
	if !ok {
		return fmt.Errorf("unknown network %q (known: %v)", name, Networks())
	}
	networkMu.Lock()
	network = p
	networkMu.Unlock()
	NoRetarget = p.NoRetarget
	return nil
}

// Network returns the parameters of the network in force.
func Network() NetworkParams {
	networkMu.RLock()
	defer networkMu.RUnlock()
	return network
}

// NetworkName is the name of the network in force.
func NetworkName() string { return Network().Name }

// NetworkID is the consensus-visible network identifier (empty on mainnet).
func NetworkID() string { return Network().ID }

// genesisPrevHash is the PrevHash the genesis block commits to: the fixed
// sentinel on mainnet, and the sentinel bound to the network id elsewhere, so
// every network has a distinct genesis hash.
func genesisPrevHash() string {
	if id := NetworkID(); id != "" {
		return GenesisPrevHash + ":" + id
	}
	return GenesisPrevHash
}
