package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nexusriot/DNAS/wallet"
)

// A fresh address per invoice.
//
// The problem it fixes, which the invoice file has admitted since it was
// written: an invoice is matched by (address, amount, payments at or above its
// starting height). Two invoices for the same amount at the same address are
// therefore indistinguishable — pay one and both settle, or neither does,
// depending on which the watcher looks at first. The command printed a warning
// and told the operator to use a fresh key per invoice, which is advice rather
// than a feature: it means managing a key file per customer.
//
// What unblocked it is SLIP-0010. One seed derives an unlimited run of
// addresses at m/44'/0'/account'/0/index, so an invoice can take the next
// unused index and still be spendable from the one backup phrase. Before
// standard derivation landed, handing out derived addresses would have meant
// handing out addresses that only THIS implementation could recover — which is
// a worse failure than the one being fixed.
//
// The index counter lives beside the seed in a small JSON file. It is advisory:
// losing it re-uses indexes, which is a privacy and matching problem rather
// than a loss of funds, since the keys are all still derivable from the seed.

// invoiceSeedVersion is the on-disk format version of the derivation state.
const invoiceSeedVersion = 1

// invoiceSeedFile is a BIP39 seed plus the next unused derivation index.
//
// The mnemonic is stored, not the derived keys: the whole point is that one
// phrase recovers every address the merchant has ever handed out. It is written
// 0600 and the command says so, because a file that is quietly a wallet is how
// people lose money.
type invoiceSeedFile struct {
	Version  int    `json:"version"`
	Network  string `json:"network"`
	Mnemonic string `json:"mnemonic"`
	Account  uint32 `json:"account"`
	NextIdx  uint32 `json:"next_index"`
}

// loadInvoiceSeed reads the derivation state, creating it with a new mnemonic
// if the file does not exist.
//
// It returns whether it created one, so the caller can print the phrase exactly
// once — at creation, where someone is in a position to write it down.
func loadInvoiceSeed(path, network string) (*invoiceSeedFile, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		mnemonic, _, err := wallet.NewHD(128, "")
		if err != nil {
			return nil, false, fmt.Errorf("generate a seed: %w", err)
		}
		sf := &invoiceSeedFile{Version: invoiceSeedVersion, Network: network, Mnemonic: mnemonic}
		if err := writeInvoiceSeed(path, sf); err != nil {
			return nil, false, err
		}
		return sf, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	var sf invoiceSeedFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	if sf.Version != invoiceSeedVersion {
		return nil, false, fmt.Errorf("%s is version %d, this build writes %d", path, sf.Version, invoiceSeedVersion)
	}
	// The network is checked because the derivation is network-independent but
	// the ADDRESSES are not interchangeable across chains: handing a mainnet
	// payer a regtest address is a payment nobody can find.
	if sf.Network != "" && sf.Network != network {
		return nil, false, fmt.Errorf("%s holds %s addresses, but this node is on %s", path, sf.Network, network)
	}
	return &sf, false, nil
}

func writeInvoiceSeed(path string, sf *invoiceSeedFile) error {
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	// Written through a temp file and renamed: a crash partway through a write
	// would otherwise leave a seed file that parses as nothing, and the next run
	// would mint a NEW mnemonic and quietly orphan every address handed out.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// nextInvoiceAddress derives the next unused address and advances the counter.
//
// The counter is advanced and persisted BEFORE the address is handed back, so a
// crash between deriving and writing the invoice burns an index rather than
// re-using one. A gap costs nothing; a repeat is the bug this exists to fix.
func nextInvoiceAddress(path, network string) (addr string, index uint32, created bool, err error) {
	sf, created, err := loadInvoiceSeed(path, network)
	if err != nil {
		return "", 0, false, err
	}
	hd, err := wallet.HDFromMnemonic(sf.Mnemonic, "")
	if err != nil {
		return "", 0, created, fmt.Errorf("restore the seed: %w", err)
	}
	index = sf.NextIdx
	w := hd.DeriveAccount(sf.Account, index)
	sf.NextIdx++
	sf.Network = network
	if err := writeInvoiceSeed(path, sf); err != nil {
		return "", 0, created, err
	}
	return w.Address(), index, created, nil
}

// invoiceKeyAt re-derives the key for an index, so the merchant can actually
// spend what was paid to an address the invoice handed out.
func invoiceKeyAt(path, network string, account, index uint32) (*wallet.Wallet, error) {
	sf, created, err := loadInvoiceSeed(path, network)
	if err != nil {
		return nil, err
	}
	if created {
		return nil, fmt.Errorf("%s did not exist: there is no seed to derive from", path)
	}
	hd, err := wallet.HDFromMnemonic(sf.Mnemonic, "")
	if err != nil {
		return nil, err
	}
	return hd.DeriveAccount(account, index), nil
}
