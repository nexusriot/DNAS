package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// Parsing for the two BIP9 flags (see core/versionbits.go).
//
// Both are consensus configuration: -deployments states the terms of a vote, and
// every node on the network must be given identical terms or they will compute
// different activation heights and fork. -signalbits is local policy — which
// votes this operator casts — and nodes are expected to disagree about it, since
// that disagreement is the whole point of holding a vote.

// parseDeployment reads one -deployments entry:
//
//	name:bit:start:timeout:window:threshold
func parseDeployment(s string) (core.Deployment, error) {
	f := strings.Split(strings.TrimSpace(s), ":")
	if len(f) != 6 {
		return core.Deployment{}, fmt.Errorf("want name:bit:start:timeout:window:threshold, got %d fields", len(f))
	}
	nums := make([]uint64, 5)
	for i, raw := range f[1:] {
		v, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return core.Deployment{}, fmt.Errorf("field %d (%q) is not a number", i+2, raw)
		}
		nums[i] = v
	}
	if nums[0] > uint64(core.MaxDeploymentBit) {
		return core.Deployment{}, fmt.Errorf("bit %d out of range (0..%d)", nums[0], core.MaxDeploymentBit)
	}
	d := core.Deployment{
		Name:      strings.TrimSpace(f[0]),
		Bit:       uint8(nums[0]),
		Start:     nums[1],
		Timeout:   nums[2],
		Window:    nums[3],
		Threshold: nums[4],
	}
	// Validate here as well as in RegisterDeployment so a typo is reported against
	// the flag the operator typed rather than as a registry error.
	if err := d.Validate(); err != nil {
		return core.Deployment{}, err
	}
	return d, nil
}

// parseSignalBits reads a -signalbits list ("0,2,7") into bit numbers.
func parseSignalBits(s string) ([]uint8, error) {
	var out []uint8
	for _, raw := range strings.Split(s, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		v, err := strconv.ParseUint(raw, 10, 8)
		if err != nil {
			return nil, fmt.Errorf("%q is not a bit number", raw)
		}
		if v > uint64(core.MaxDeploymentBit) {
			return nil, fmt.Errorf("bit %d out of range (0..%d)", v, core.MaxDeploymentBit)
		}
		out = append(out, uint8(v))
	}
	return out, nil
}

// Address and derivation helpers for the wallet subcommands.

// showAddress renders an address in whichever spelling was asked for. The
// canonical one stays the default because it is what consensus and every stored
// chain use; bech32 is for handing to a person (see wallet/bech32.go).
func showAddress(addr string, bech bool) string {
	if !bech {
		return addr
	}
	b32, err := wallet.ToBech32(addr)
	if err != nil {
		return addr
	}
	return b32
}

// deriveAt derives one wallet, through the legacy scheme when asked. A mnemonic
// written down before SLIP-0010 landed still reaches its coin this way.
func deriveAt(hd *wallet.HDWallet, account, index uint32, legacy bool) *wallet.Wallet {
	if legacy {
		return hd.DeriveLegacy(index)
	}
	return hd.DeriveAccount(account, index)
}

// derivationLabel names the scheme an address came from, so a listing cannot be
// mistaken for the other one's.
func derivationLabel(account, index uint32, legacy bool) string {
	if legacy {
		return "pre-SLIP-0010 scheme (index only; -legacy)"
	}
	return wallet.AccountPath(account, index)
}
