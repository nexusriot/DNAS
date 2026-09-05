package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// Labels, notes, and watch-only export.
//
// A light wallet that can only show addresses and amounts is an accounting tool
// missing the half that makes accounting useful: WHICH address, and WHY that
// payment. Labels and per-transaction notes are that half, and they are private
// client-side annotations — nothing here touches consensus or leaves the machine.
//
// The export is the other half of the same idea: a watch-only file carrying the
// addresses and their labels and NO key material, so a second machine can track
// the same accounts without being able to spend from them. HD derivation here is
// hardened (it needs the seed, see wallet/hd.go), so there is no extended public
// key to share — the address list IS the watch-only credential.

// watchOnlyVersion is the export format's version, so a future change can be
// detected rather than silently misread.
const watchOnlyVersion = 1

// WatchOnlyExport is the portable, key-free view of a light wallet.
type WatchOnlyExport struct {
	Version   int              `json:"version"`
	Network   string           `json:"network"`
	Addresses []WatchOnlyEntry `json:"addresses"`
}

// WatchOnlyEntry is one watched address and what its owner calls it.
type WatchOnlyEntry struct {
	Address string `json:"address"`
	Label   string `json:"label,omitempty"`
}

// label returns an address's label, or "" if it has none.
func (sw *SPVWallet) label(addr string) string { return sw.Labels[addr] }

// setLabel names an address (an empty label removes the name). It does not
// require the address to be watched: labelling a counterparty you have not added
// is exactly how an address book gets built.
func (sw *SPVWallet) setLabel(addr, text string) error {
	if err := wallet.ValidateAddress(addr); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		delete(sw.Labels, addr)
		return nil
	}
	if len(text) > maxAnnotationBytes {
		return fmt.Errorf("label too long (%d > %d bytes)", len(text), maxAnnotationBytes)
	}
	if sw.Labels == nil {
		sw.Labels = map[string]string{}
	}
	sw.Labels[addr] = text
	return nil
}

// setNote annotates one transaction (an empty note removes it).
func (sw *SPVWallet) setNote(txHash, text string) error {
	if len(txHash) != 64 {
		return errors.New("a transaction id is 64 hex characters")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		delete(sw.Notes, txHash)
		return nil
	}
	if len(text) > maxAnnotationBytes {
		return fmt.Errorf("note too long (%d > %d bytes)", len(text), maxAnnotationBytes)
	}
	if sw.Notes == nil {
		sw.Notes = map[string]string{}
	}
	sw.Notes[txHash] = text
	return nil
}

// maxAnnotationBytes bounds a label or note. They live in the wallet file, not
// on the chain, so this is only there to keep the file sane.
const maxAnnotationBytes = 256

// describe renders an address with its label, for display.
func (sw *SPVWallet) describe(addr string) string {
	if l := sw.label(addr); l != "" {
		return fmt.Sprintf("%s  (%s)", addr, l)
	}
	return addr
}

// exportWatchOnly builds the key-free export of everything this wallet watches.
func (sw *SPVWallet) exportWatchOnly() WatchOnlyExport {
	out := WatchOnlyExport{
		Version:   watchOnlyVersion,
		Network:   core.NetworkName(),
		Addresses: make([]WatchOnlyEntry, 0, len(sw.Addresses)),
	}
	for _, a := range sw.Addresses {
		out.Addresses = append(out.Addresses, WatchOnlyEntry{Address: a, Label: sw.label(a)})
	}
	// Labels on addresses that are not watched (an address book of counterparties)
	// travel too — that is most of what makes the export worth having.
	extra := make([]string, 0, len(sw.Labels))
	for addr := range sw.Labels {
		if !sw.has(addr) {
			extra = append(extra, addr)
		}
	}
	sort.Strings(extra)
	for _, addr := range extra {
		out.Addresses = append(out.Addresses, WatchOnlyEntry{Address: addr, Label: sw.Labels[addr]})
	}
	return out
}

// importWatchOnly merges an export into this wallet: every address becomes
// watched and every label is adopted. It reports how many addresses were new.
// A file from another network is refused — the addresses would be meaningless
// and the resulting balances silently wrong.
func (sw *SPVWallet) importWatchOnly(exp WatchOnlyExport) (int, error) {
	if exp.Version != watchOnlyVersion {
		return 0, fmt.Errorf("unsupported watch-only format version %d (this build reads %d)", exp.Version, watchOnlyVersion)
	}
	if exp.Network != "" && exp.Network != core.NetworkName() {
		return 0, fmt.Errorf("this file is for the %s network, not %s", exp.Network, core.NetworkName())
	}
	added := 0
	for _, e := range exp.Addresses {
		if err := wallet.ValidateAddress(e.Address); err != nil {
			return added, fmt.Errorf("invalid address %q: %w", e.Address, err)
		}
		if sw.addAddress(e.Address) {
			added++
		}
		if e.Label != "" {
			if err := sw.setLabel(e.Address, e.Label); err != nil {
				return added, err
			}
		}
	}
	return added, nil
}

// writeWatchOnly saves an export to a file.
func (sw *SPVWallet) writeWatchOnly(path string) error {
	data, err := json.MarshalIndent(sw.exportWatchOnly(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// readWatchOnly loads an export from a file and merges it in.
func (sw *SPVWallet) readWatchOnly(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var exp WatchOnlyExport
	if err := json.Unmarshal(data, &exp); err != nil {
		return 0, fmt.Errorf("%s is not a watch-only export: %w", path, err)
	}
	return sw.importWatchOnly(exp)
}
