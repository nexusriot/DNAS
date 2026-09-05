package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// runVault implements `dnas vault ...`, the command-line tools for time-delayed
// vaults: an account whose warm key can only spend after a height, and whose
// cold key can spend at any time.
//
//	dnas vault address -hot HEX -cold HEX -unlock H       derive the address (offline)
//	dnas vault spend   -wallet FILE -hot|-cold HEX -unlock H -to ADDR [-api URL] [-fee F]
//
// The spending party's own key comes from the wallet file and the counterpart
// key is a flag, exactly as `dnas htlc` does it — together they reconstruct the
// script the address commits to.
func runVault(args []string) {
	if len(args) == 0 {
		fmt.Println(`usage: dnas vault <address | spend> [flags]
  address -hot HEX -cold HEX -unlock H     derive the vault address (offline)
  spend   -wallet FILE -unlock H -to ADDR [-hot HEX | -cold HEX] [-api URL] [-fee F]
          sweep the vault; supply the OTHER key's hex, the wallet holds yours.
          The cold key may spend at any height; the hot key only from -unlock on.`)
		return
	}
	switch args[0] {
	case "address":
		vaultAddressCmd(args[1:])
	case "spend":
		vaultSpendCmd(args[1:])
	default:
		fmt.Println("unknown vault command:", args[0], "(address | spend)")
	}
}

func vaultAddressCmd(args []string) {
	fs := flag.NewFlagSet("vault address", flag.ExitOnError)
	hot := fs.String("hot", "", "hot (delayed) public key, hex")
	cold := fs.String("cold", "", "cold (recovery) public key, hex")
	unlock := fs.Uint64("unlock", 0, "height at/after which the hot key may spend")
	_ = fs.Parse(args)
	addr, err := wallet.VaultAddress(*hot, *cold, *unlock)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(addr)
}

// vaultSpendCmd builds, signs and submits a vault sweep. Exactly one of -hot or
// -cold names the counterpart key: whichever is given is the role the wallet is
// NOT playing.
func vaultSpendCmd(args []string) {
	fs := flag.NewFlagSet("vault spend", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	walletPath := fs.String("wallet", "wallet.json", "wallet key file for the spending party")
	hot := fs.String("hot", "", "hot public key hex (give this when spending with the COLD key)")
	cold := fs.String("cold", "", "cold public key hex (give this when spending with the HOT key)")
	unlock := fs.Uint64("unlock", 0, "the vault's unlock height")
	to := fs.String("to", "", "address to sweep the vault balance to")
	fee := fs.Uint64("fee", core.DefaultMinRelayFee*1000, "fee in base units")
	_ = fs.Parse(args)

	if (*hot == "") == (*cold == "") {
		log.Fatal("give exactly one of -hot or -cold: the counterparty's key (yours comes from -wallet)")
	}
	if *to == "" {
		log.Fatal("-to is required (where to sweep the vault)")
	}
	w, err := loadWallet(*walletPath)
	if err != nil {
		log.Fatalf("wallet: %v", err)
	}
	hotKey, coldKey := *hot, *cold
	spendingCold := hotKey != "" // we were given the hot key, so we hold the cold one
	if spendingCold {
		coldKey = w.PublicKeyHex()
	} else {
		hotKey = w.PublicKeyHex()
	}
	addr, err := wallet.VaultAddress(hotKey, coldKey, *unlock)
	if err != nil {
		log.Fatalf("bad script: %v", err)
	}

	base := ensureHTTP(*apiAddr)
	adoptNetwork(base) // the spend is signed below, and signatures are network-bound
	acc, err := fetchAccount(base, addr)
	if err != nil {
		log.Fatalf("fetch vault account: %v", err)
	}
	if acc.Balance <= *fee {
		log.Fatalf("vault balance %s does not cover the fee %s",
			core.FormatAmount(acc.Balance), core.FormatAmount(*fee))
	}
	tx := core.Transaction{
		From:   addr,
		To:     *to,
		Amount: acc.Balance - *fee,
		Fee:    *fee,
		Nonce:  acc.Nonce,
		Vault:  &core.VaultScript{Hot: hotKey, Cold: coldKey, Unlock: *unlock},
	}
	tx.SignVault(w)
	if err := tx.VerifySignature(); err != nil { // local sanity check before submitting
		log.Fatalf("built an invalid spend: %v", err)
	}
	if err := postJSON(base+"/tx", tx); err != nil {
		log.Fatalf("submit: %v", err)
	}
	key := "hot"
	if spendingCold {
		key = "cold"
	}
	fmt.Printf("submitted %s-key sweep of %s (%s) from %s to %s\n",
		key, core.FormatAmount(tx.Amount), short(tx.Hash()), short(addr), short(*to))
	if !spendingCold {
		fmt.Printf("the hot path is only valid from height %d on\n", *unlock)
	}
}
