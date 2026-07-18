// Command dnas runs a node of the DNAS toy cryptocurrency, or manages wallets.
//
//	dnas node   [flags]      run a full node (default if no subcommand)
//	dnas wallet new   [-o]   create a new wallet key file
//	dnas wallet address [-o] print the address of a wallet key file
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/nexusriot/DNAS/api"
	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
	"github.com/nexusriot/DNAS/wallet"
)

// version is the build version, injected at link time with
// -ldflags "-X main.version=...". It defaults to "dev" for plain `go build`.
var version = "dev"

// printVersion writes the binary's version line.
func printVersion(w io.Writer) { fmt.Fprintf(w, "dnas %s\n", version) }

func main() {
	log.SetFlags(log.Ltime)
	args := os.Args[1:]
	if len(args) == 0 {
		runNode(nil)
		return
	}
	switch args[0] {
	case "node":
		runNode(args[1:])
	case "wallet":
		runWallet(args[1:])
	case "spv":
		runSPV(args[1:])
	case "fastsync":
		runFastSync(args[1:])
	case "htlc":
		runHTLC(args[1:])
	case "help", "-h", "--help":
		usage()
	case "version", "-v", "--version":
		printVersion(os.Stdout)
	default:
		// Bare flags (e.g. `dnas -mine`) are treated as node flags.
		runNode(args)
	}
}

func usage() {
	fmt.Println(`dnas - a small proof-of-work cryptocurrency

Usage:
  dnas node [flags]              run a node (default)
  dnas wallet new [-o FILE]      create a wallet
  dnas wallet address [-o FILE]
  dnas spv [-api URL] sync            verify the header chain (light client)
  dnas spv [-api URL] verify <txhash> prove a payment is in the chain
  dnas spv [-api URL] scan <address>  find/prove non-inclusion of an address
  dnas spv [-api URL] balance <addr>  prove an address's balance (state proof)
  dnas spv [-api URL] history <addr>  reconstruct an address's history (light wallet)
  dnas spv [-api URL] wallet ...      persistent light wallet (add/update/status/watch)
  dnas spv [-api URL] wallet -key F new|send <to> <amount>   self-custodial light wallet (signs locally)
  dnas fastsync [-api URL] [-checkpoint H:HASH]   bootstrap state from a verified snapshot
  dnas htlc new                       mint a preimage + hash for an atomic swap
  dnas htlc <address|claim|refund>    build hash-time-locked contract spends
  dnas version                        print the build version

Node flags:
  -listen ADDR    p2p listen address (default ":3000")
  -advertise ADDR address peers should dial us at (default: -listen)
  -api ADDR       HTTP API address (default ":8080")
  -peers LIST     comma-separated seed peer addresses
  -wallet FILE    wallet key file, created if missing (default "wallet.json")
  -db FILE        blockchain storage file (default "chain.json")
  -netkey KEY     pre-shared network key; peers must match (default "dnas-devnet")
  -maxpeers N     maximum outbound peer connections (default 8)
  -mempool N      max pending transactions (default 5000)
  -mine           enable mining
  -regtest        regtest mode: mine blocks on demand via POST /generate
  -checkpoints L  finality checkpoints, comma-separated height:hash pairs`)
}

// walletPassphrase reads the optional at-rest encryption passphrase from the
// environment (kept out of flags so it doesn't leak into `ps`).
func walletPassphrase() string { return os.Getenv("DNAS_WALLET_PASSPHRASE") }

func runWallet(args []string) {
	if len(args) == 0 {
		fmt.Println(`usage: dnas wallet <cmd> [-o FILE] [flags]   (set DNAS_WALLET_PASSPHRASE to encrypt at rest)
  new                       create a random wallet
  address                   print a wallet file's address
  pubkey                    print a wallet file's public key (for multisig/HTLC)
  mnemonic                  create an HD wallet, print its BIP39 backup phrase
  restore   [-index N]      rebuild a wallet file from a mnemonic (read from stdin)
  addresses [-n N]          print the first N HD addresses for a mnemonic (stdin)
  multisig  -threshold M -pubkeys a,b,c   print an M-of-N multisig address`)
		return
	}
	fs := flag.NewFlagSet("wallet", flag.ExitOnError)
	out := fs.String("o", "wallet.json", "wallet key file")
	index := fs.Uint("index", 0, "HD account index")
	count := fs.Int("n", 5, "number of HD addresses to list")
	threshold := fs.Int("threshold", 2, "multisig signature threshold (M)")
	pubkeys := fs.String("pubkeys", "", "comma-separated member public keys (hex) for multisig")
	_ = fs.Parse(args[1:])
	pass := walletPassphrase()

	// loadFile opens the existing wallet key file, honouring the passphrase.
	loadFile := func() *wallet.Wallet {
		var (
			w   *wallet.Wallet
			err error
		)
		if pass != "" {
			w, err = wallet.LoadEncrypted(*out, pass)
		} else {
			w, err = wallet.Load(*out)
		}
		if err != nil {
			log.Fatal(err)
		}
		return w
	}

	save := func(w *wallet.Wallet) {
		var err error
		if pass != "" {
			err = w.SaveEncrypted(*out, pass)
		} else {
			err = w.Save(*out)
		}
		if err != nil {
			log.Fatal(err)
		}
	}
	suffix := ""
	if pass != "" {
		suffix = " (encrypted)"
	}

	switch args[0] {
	case "new":
		w, err := wallet.New()
		if err != nil {
			log.Fatal(err)
		}
		save(w)
		fmt.Printf("wrote %s%s\naddress: %s\n", *out, suffix, w.Address())

	case "address":
		fmt.Println(loadFile().Address())

	case "pubkey":
		// The public key is shared with counterparties to build multisig and HTLC
		// scripts (which are addressed by a hash of the member public keys).
		fmt.Println(loadFile().PublicKeyHex())

	case "mnemonic":
		m, hd, err := wallet.NewHD(128, "")
		if err != nil {
			log.Fatal(err)
		}
		w := hd.Derive(0)
		save(w)
		fmt.Printf("wrote %s%s\naddress (index 0): %s\n\n", *out, suffix, w.Address())
		fmt.Println("BIP39 backup phrase — write it down, it restores every derived address:")
		fmt.Println("  " + m)

	case "restore":
		m := readMnemonic()
		hd, err := wallet.HDFromMnemonic(m, "")
		if err != nil {
			log.Fatal(err)
		}
		w := hd.Derive(uint32(*index))
		save(w)
		fmt.Printf("restored index %d to %s%s\naddress: %s\n", *index, *out, suffix, w.Address())

	case "addresses":
		m := readMnemonic()
		hd, err := wallet.HDFromMnemonic(m, "")
		if err != nil {
			log.Fatal(err)
		}
		for i := 0; i < *count; i++ {
			fmt.Printf("  [%d] %s\n", i, hd.Derive(uint32(i)).Address())
		}

	case "multisig":
		addr, err := wallet.MultisigAddress(*threshold, parsePeers(*pubkeys))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%d-of-%d multisig address: %s\n", *threshold, len(parsePeers(*pubkeys)), addr)
		fmt.Println("(fund it like any address; spend by submitting a signed multisig tx to POST /tx)")

	default:
		fmt.Println("unknown wallet command:", args[0])
	}
}

// readMnemonic reads a BIP39 mnemonic from stdin (so it doesn't land in shell
// history or `ps`).
func readMnemonic() string {
	fmt.Print("enter mnemonic: ")
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		log.Fatal("no mnemonic provided")
	}
	return strings.TrimSpace(sc.Text())
}

// nodeConfig holds optional JSON config values (all fields optional). It maps
// each node flag to a key; a value present here becomes that flag's default.
type nodeConfig map[string]any

func (c nodeConfig) str(key, def string) string {
	if v, ok := c[key].(string); ok {
		return v
	}
	return def
}
func (c nodeConfig) integer(key string, def int) int {
	if v, ok := c[key].(float64); ok { // JSON numbers decode as float64
		return int(v)
	}
	return def
}
func (c nodeConfig) boolean(key string, def bool) bool {
	if v, ok := c[key].(bool); ok {
		return v
	}
	return def
}

// scanConfigPath finds the value of -config / --config in args (before the main
// flagset is built), so the config file can seed flag defaults.
func scanConfigPath(args []string) string {
	for i, a := range args {
		switch {
		case a == "-config" || a == "--config":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(a, "-config="):
			return strings.TrimPrefix(a, "-config=")
		case strings.HasPrefix(a, "--config="):
			return strings.TrimPrefix(a, "--config=")
		}
	}
	return ""
}

// loadNodeConfig reads a JSON config file (empty path -> empty config).
func loadNodeConfig(path string) nodeConfig {
	if path == "" {
		return nodeConfig{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	var c nodeConfig
	if err := json.Unmarshal(data, &c); err != nil {
		log.Fatalf("config %s: %v", path, err)
	}
	return c
}

func runNode(args []string) {
	// A JSON config file supplies defaults; any flag given on the command line
	// overrides its value. -config is pre-scanned so it can seed the defaults.
	cfg := loadNodeConfig(scanConfigPath(args))
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	_ = fs.String("config", "", "JSON config file (flags override its values)")
	listen := fs.String("listen", cfg.str("listen", ":3000"), "p2p listen address")
	advertise := fs.String("advertise", cfg.str("advertise", ""), "address peers should dial us at (default: -listen)")
	apiAddr := fs.String("api", cfg.str("api", ":8080"), "HTTP API address")
	peersStr := fs.String("peers", cfg.str("peers", ""), "comma-separated seed peer addresses")
	walletPath := fs.String("wallet", cfg.str("wallet", "wallet.json"), "wallet key file (created if missing)")
	dbPath := fs.String("db", cfg.str("db", "chain.db"), "blockchain append-only store file")
	netKey := fs.String("netkey", cfg.str("netkey", node.DefaultNetKey), "pre-shared network key (peers must match)")
	maxPeers := fs.Int("maxpeers", cfg.integer("maxpeers", node.DefaultMaxPeers), "maximum outbound peer connections")
	mempoolMax := fs.Int("mempool", cfg.integer("mempool", core.DefaultMempoolSize), "max pending transactions")
	minRelayFee := fs.Int("minrelayfee", cfg.integer("minrelayfee", int(core.DefaultMinRelayFee)), "base minimum relay fee in base units (rises with mempool load; 0 disables)")
	mine := fs.Bool("mine", cfg.boolean("mine", false), "enable mining")
	regtest := fs.Bool("regtest", cfg.boolean("regtest", false), "regtest mode: enable on-demand block generation (POST /generate)")
	dandelion := fs.Bool("dandelion", cfg.boolean("dandelion", true), "relay new transactions via Dandelion++ stem/fluff (origin privacy)")
	checkpoints := fs.String("checkpoints", cfg.str("checkpoints", ""), "finality checkpoints as comma-separated height:hash pairs")
	_ = fs.Parse(args)

	// Pin any finality checkpoints before syncing, so a block at a checkpointed
	// height must match and no reorg may fork below it.
	for _, cp := range parsePeers(*checkpoints) {
		height, hash, ok := strings.Cut(cp, ":")
		h, err := strconv.ParseUint(strings.TrimSpace(height), 10, 64)
		if !ok || err != nil || strings.TrimSpace(hash) == "" {
			log.Fatalf("bad -checkpoints entry %q (want height:hash)", cp)
		}
		core.AddCheckpoint(h, strings.TrimSpace(hash))
		log.Printf("checkpoint pinned: height %d", h)
	}

	// In regtest, isolate the network by default (a distinct pre-shared key) so a
	// local test node can't accidentally peer with a devnet, unless the operator
	// set -netkey explicitly.
	if *regtest && *netKey == node.DefaultNetKey {
		*netKey = "dnas-regtest"
	}

	var relayFloor uint64
	if *minRelayFee > 0 {
		relayFloor = uint64(*minRelayFee)
	}

	w, created, err := wallet.LoadOrCreateEncrypted(*walletPath, walletPassphrase())
	if err != nil {
		log.Fatalf("wallet: %v", err)
	}
	if created {
		log.Printf("created new wallet %s", *walletPath)
	}
	log.Printf("wallet address: %s", w.Address())

	chain, err := core.Open(*dbPath)
	if err != nil {
		log.Fatalf("open chain %s: %v", *dbPath, err)
	}
	log.Printf("chain height=%d (%s)", chain.Height(), *dbPath)

	mp := core.NewMempoolWithPolicy(*mempoolMax, relayFloor)
	if relayFloor > 0 {
		log.Printf("min relay fee: %s (base, rises with mempool load)", core.FormatAmount(relayFloor))
	}
	n := node.New(node.Config{
		ListenAddr:    *listen,
		AdvertiseAddr: *advertise,
		Peers:         parsePeers(*peersStr),
		NetKey:        *netKey,
		MaxPeers:      *maxPeers,
		Mine:          *mine,
		StateDir:      filepath.Dir(*dbPath), // persist peers/bans/mempool beside the chain
		Regtest:       *regtest,
		Dandelion:     *dandelion,
	}, chain, mp, w)
	n.Start()

	srv := api.New(n)
	if srv.AuthEnabled() {
		log.Print("API write endpoints require a bearer token (DNAS_API_TOKEN)")
	}
	go srv.Start(*apiAddr)

	// Clean shutdown: stop mining, close peers, flush the chain store.
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			log.Print("shutting down…")
			n.Shutdown()
			if err := chain.Close(); err != nil {
				log.Printf("close chain: %v", err)
			}
		})
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		shutdown()
		os.Exit(0)
	}()

	if stdinIsTTY() {
		repl(n)
		shutdown() // "quit" from the REPL
	} else {
		select {} // no terminal (e.g. background/demo); wait for a signal
	}
}

func parsePeers(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// stdinIsTTY reports whether stdin is an interactive terminal. It uses the
// TCGETS ioctl (the classic isatty) rather than os.ModeCharDevice, which would
// wrongly treat /dev/null and other char devices as terminals.
func stdinIsTTY() bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdin.Fd(),
		syscall.TCGETS, uintptr(unsafe.Pointer(&termios)))
	return errno == 0
}

func repl(n *node.Node) {
	fmt.Println("commands: send <to> <amount> [fee] [expiry-height] | balance [addr] | address | info | peers | mempool | help | quit")
	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("dnas> ")
		if !sc.Scan() {
			return
		}
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "send":
			if len(fields) < 3 {
				fmt.Println("usage: send <to> <amount> [fee] [expiry-height]")
				continue
			}
			if err := wallet.ValidateAddress(fields[1]); err != nil {
				fmt.Println("invalid recipient:", err)
				continue
			}
			amount, err := core.ParseAmount(fields[2])
			if err != nil {
				fmt.Println(err)
				continue
			}
			var fee uint64
			if len(fields) > 3 {
				if fee, err = core.ParseAmount(fields[3]); err != nil {
					fmt.Println(err)
					continue
				}
			}
			var expiry uint64
			if len(fields) > 4 {
				if expiry, err = strconv.ParseUint(fields[4], 10, 64); err != nil {
					fmt.Println("bad expiry height:", err)
					continue
				}
			}
			w := n.Wallet()
			tx := core.Transaction{
				From:   w.Address(),
				To:     fields[1],
				Amount: amount,
				Fee:    fee,
				Nonce:  n.NextNonce(w.Address()),
				Expiry: expiry,
			}
			if err := tx.Sign(w); err != nil {
				fmt.Println(err)
				continue
			}
			if err := n.SubmitTx(tx); err != nil {
				fmt.Println("rejected:", err)
				continue
			}
			fmt.Println("submitted", tx.Hash()[:10])
		case "balance":
			addr := n.Wallet().Address()
			if len(fields) > 1 {
				addr = fields[1]
			}
			acc := n.Chain().Account(addr)
			fmt.Printf("%s\n  balance=%s nonce=%d\n", addr, core.FormatAmount(acc.Balance), acc.Nonce)
		case "address":
			fmt.Println(n.Wallet().Address())
		case "info":
			tip := n.Chain().Tip()
			fmt.Printf("height=%d diff(next)=%.2f tip=%s mempool=%d peers=%d\n",
				tip.Index, core.TargetDifficulty(n.Chain().NextBits()), tip.Hash[:10], n.Mempool().Size(), len(n.PeerAddrs()))
		case "peers":
			fmt.Println(n.PeerAddrs())
		case "mempool":
			fmt.Printf("%d pending\n", n.Mempool().Size())
		case "help":
			fmt.Println("send <to> <amount> [fee] [expiry-height] | balance [addr] | address | info | peers | mempool | quit")
		case "quit", "exit":
			return
		default:
			fmt.Println("unknown command:", fields[0])
		}
	}
}
