package main

import (
	"log"

	"github.com/nexusriot/DNAS/core"
)

// A client and the node it talks to must agree on the network.
//
// This is not configuration hygiene — it is correctness. The network id is part
// of the transaction signing preimage and of the genesis hash (see
// core/network.go), so a client that thinks it is on mainnet while the node runs
// regtest signs transactions the node rejects as forgeries, and rejects the
// node's genesis as foreign. Both failures look like something else: "invalid
// signature", "genesis mismatch".
//
// Rather than make every user pass a flag that has to match, a client asks the
// node. `GET /info` reports the network, and adoptNetwork switches this process
// onto it before anything is signed or verified. An explicit -network flag still
// wins where a command has one (`dnas db`, which reads a store with no node to
// ask).

// adoptNetwork puts this process on the same network as the node at base, so
// signatures and the genesis check line up. A node that cannot be reached or
// predates the field leaves the default (mainnet) in place — the command that
// follows will fail on its own terms, with its own error, rather than being
// pre-empted by a confusing one from here.
func adoptNetwork(base string) {
	var info struct {
		Network string `json:"network"`
	}
	if err := getJSON(base+"/info", &info); err != nil || info.Network == "" {
		return
	}
	if info.Network == core.NetworkName() {
		return
	}
	if err := core.SetNetwork(info.Network); err != nil {
		log.Printf("node reports an unknown network %q; continuing on %s", info.Network, core.NetworkName())
		return
	}
	log.Printf("using the node's network: %s", info.Network)
}
