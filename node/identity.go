package node

import (
	"fmt"

	"github.com/nexusriot/DNAS/wallet"
)

// A node's network identity is a separate key from its wallet, and that
// separation is a privacy requirement rather than tidiness.
//
// Every connection ends with each side signing the session id with its identity
// key and sending the PUBLIC KEY along (MsgIdentity), which is what makes ban
// scoring attributable to a peer rather than to an address that can be changed
// for free. But a DNAS address IS a hash of a public key: given the identity key
// a peer just received, anyone can compute `AddressFromPubKeyHex(...)` and get
// the address that key controls.
//
// So a node that uses its WALLET key as its identity hands every peer — and
// anyone who can get a peer to talk — the address holding its coin. They can
// then watch its balance, its mining income, and every payment it makes. It also
// substantially undoes the Dandelion++ origin privacy the node already
// implements: obscuring which peer first relayed a transaction matters much less
// when peers know which address each peer owns.
//
// The identity therefore lives in its own key file, generated on first use. It
// is not a spending key and holds no coin, so it needs no backup — losing it
// costs a node its accumulated peer reputation and nothing else.

// IdentityFile is the default filename for a node's network identity key,
// alongside its chain and soft state.
const IdentityFile = "nodekey.json"

// LoadOrCreateIdentity loads the node identity key at path, creating it if it is
// missing. The passphrase, if any, is the same at-rest encryption the wallet
// uses. It reports whether the key was newly created.
//
// This deliberately does NOT fall back to the wallet: an identity that leaks the
// wallet address is the thing it exists to avoid.
func LoadOrCreateIdentity(path, passphrase string) (*wallet.Wallet, bool, error) {
	w, created, err := wallet.LoadOrCreateEncrypted(path, passphrase)
	if err != nil {
		return nil, false, fmt.Errorf("node identity %s: %w", path, err)
	}
	return w, created, nil
}

// identityWarning is logged when a node runs with its wallet key as its network
// identity — which New still allows, because an in-process node (a test, an
// embedded use) has no file to read and no peers to leak to.
func warnSharedIdentity(walletAddr string) {
	Warnf("network identity is the wallet key",
		"address", walletAddr,
		"impact", "every peer can derive this address and watch its balance",
		"fix", "give the node its own -nodekey")
}
