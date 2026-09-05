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
	"time"
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
	case "miner":
		runMiner(args[1:])
	case "htlc":
		runHTLC(args[1:])
	case "supply":
		runSupply(args[1:])
	case "db":
		runDB(args[1:])
	case "vault":
		runVault(args[1:])
	case "faucet":
		runFaucet(args[1:])
	case "sponsor":
		runSponsor(args[1:])
	case "multisig":
		runMultisig(args[1:])
	case "anchor":
		runAnchor(args[1:])
	case "escrow":
		runEscrow(args[1:])
	case "backup":
		runBackup(args[1:])
	case "tx":
		runTx(args[1:])
	case "assets":
		runAssets(args[1:])
	case "invoice":
		runInvoice(args[1:])
	case "peers":
		runPeers(args[1:])
	case "stats":
		runStats(args[1:])
	case "reorgs":
		runReorgs(args[1:])
	case "health":
		runHealth(args[1:])
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
  dnas wallet address|pubkey [-o FILE]
  dnas wallet mnemonic|restore|addresses      BIP39 backup + HD addresses
  dnas wallet multisig -threshold M -pubkeys a,b,c   M-of-N multisig address
  dnas spv [-api URL] sync            verify the header chain (light client)
  dnas spv [-api URL] verify <txhash> prove a payment is in the chain
  dnas spv [-api URL] scan <address>  find/prove non-inclusion of an address
  dnas spv [-api URL] balance <addr>  prove an address's balance (state proof)
  dnas spv [-api URL] history <addr>  reconstruct an address's history (light wallet)
  dnas spv [-api URL] wallet ...      persistent light wallet (add/update/status/watch)
  dnas spv [-api URL] wallet -key F new|send <to> <amount>   self-custodial light wallet (signs locally)
  dnas spv [-api URL] wallet -key F sendmany <addr:amt>...    pay many addresses in one transaction
  dnas fastsync [-api URL] [-checkpoint H:HASH]   bootstrap state from a verified snapshot
  dnas miner -api URL -address ADDR   external miner (get template, mine, submit)
  dnas htlc new                       mint a preimage + hash for an atomic swap
  dnas htlc <address|claim|refund>    build hash-time-locked contract spends
  dnas htlc swap ...                  plan a coin-for-asset atomic swap (both legs)
  dnas vault <address|spend>          time-delayed vault: hot key after N, cold key now
  dnas supply [-api URL]              coin supply: minted, burned, circulating
  dnas faucet -address ADDR           ask a testnet/regtest node for coin
  dnas sponsor request|pay            have someone else pay a transaction's fee
  dnas multisig propose|sign|submit   spend FROM an M-of-N multisig account
  dnas anchor add|verify              timestamp a file's hash on the chain
  dnas escrow new|release|refund      2-of-3 buyer/seller/arbiter escrow
  dnas wallet sign|verify             prove you control an address, off-chain
  dnas wallet passphrase              change (or remove) a key file's passphrase
  dnas backup save|list|restore       encrypt the files a re-sync cannot replace
  dnas tx inspect|verify              decode and check a transaction before submitting
  dnas assets [show ID]               what assets exist, and who holds them
  dnas invoice new|watch|pay          ask to be paid, and verify that you were
  dnas db <info|verify|export|import> inspect, check and move a chain store
  dnas peers [list|bans|unban|add|drop]  inspect and manage peers and bans
  dnas stats [-window N]              hashrate, block timing, fee flow, miners
  dnas reorgs                         chain switches this node has lived through
  dnas health                         is this node ready to be relied on?
  dnas version                        print the build version

Node flags:
  -listen ADDR    p2p listen address (default ":3000")
  -advertise ADDR address peers should dial us at (default: -listen)
  -api ADDR       HTTP API address (default ":8080")
  -peers LIST     comma-separated seed peer addresses
  -wallet FILE    wallet key file, created if missing (default "wallet.json")
  -db FILE        blockchain append-only store file (default "chain.db")
  -netkey KEY     pre-shared key for a PRIVATE net; empty (default) = open/permissionless
  -maxpeers N     maximum outbound peer connections (default 8)
  -mempool N      max pending transactions (default 5000)
  -minrelayfee N  base min relay fee, base units per byte; rises with load (default 10)
  -mine           enable mining
  -regtest        regtest mode: mine blocks on demand via POST /generate
  -network NAME   mainnet (default), testnet or regtest — separate chains
  -addrindex      index address -> transactions (serves /address/{a}/history)
  -faucet         give coin away via POST /faucet (testnet/regtest only)
  -sharefactor N  how many times easier a mining share is than a block
  -nodekey FILE   network identity key (default nodekey.json beside -db)
  -loglevel LVL   error | warn | info (default) | debug
  -logjson        one JSON object per log line
  -printconfig    print the effective configuration and exit
  -dandelion      Dandelion++ stem/fluff relay for origin privacy (default true)
  -checkpoints L  finality checkpoints, comma-separated height:hash pairs
  -upgrades L     consensus upgrade activations, comma-separated name:height pairs
  -config FILE    JSON config file (flags override its values)`)
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
  multisig  -threshold M -pubkeys a,b,c   print an M-of-N multisig address
  sign      -m TEXT | -file F   sign a message, proving you hold the address
  verify    -in SIG [-address A]          check such a signature
  passphrase [-remove]      re-encrypt this key file under a new passphrase`)
		return
	}
	// The subcommands that take flags of their own are dispatched before the
	// shared flag set, which knows nothing about them and would reject them. They
	// still honour -o, pulled out of the arguments by hand (see extractFlag).
	switch args[0] {
	case "sign", "verify", "passphrase":
		path, rest := extractFlag(args[1:], "o", "wallet.json")
		switch args[0] {
		case "sign":
			w, err := loadWalletWith(path, walletPassphrase())
			if err != nil {
				log.Fatal(err)
			}
			walletSign(w, rest)
		case "verify":
			// Verification needs no key, so it never opens the wallet file.
			walletVerify(rest)
		case "passphrase":
			walletPassphraseCmd(path, rest)
		}
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
	netKey := fs.String("netkey", cfg.str("netkey", ""), "pre-shared network key for a PRIVATE net (peers must match); empty = open/permissionless")
	maxPeers := fs.Int("maxpeers", cfg.integer("maxpeers", node.DefaultMaxPeers), "maximum outbound peer connections")
	mempoolMax := fs.Int("mempool", cfg.integer("mempool", core.DefaultMempoolSize), "max pending transactions")
	minRelayFee := fs.Int("minrelayfee", cfg.integer("minrelayfee", int(core.DefaultMinRelayFee)), "base minimum relay fee in base units (rises with mempool load; 0 disables)")
	console := fs.Bool("console", cfg.boolean("console", false), "run the interactive console even when stdin is not a terminal (for scripts)")
	prune := fs.Int("prune", cfg.integer("prune", 0), "keep only this many recent block bodies in memory (0 = keep all; minimum "+strconv.Itoa(core.MinPruneKeep)+")")
	webhooks := fs.String("webhook", cfg.str("webhook", ""), "comma-separated URLs to POST every block/tx event to")
	apiRate := fs.Int("apirate", cfg.integer("apirate", int(api.DefaultAPIRate)), "sustained HTTP API requests per second per client (0 disables the limit)")
	apiBurst := fs.Int("apiburst", cfg.integer("apiburst", int(api.DefaultAPIBurst)), "HTTP API requests allowed back to back per client")
	mine := fs.Bool("mine", cfg.boolean("mine", false), "enable mining")
	regtest := fs.Bool("regtest", cfg.boolean("regtest", false), "regtest mode: enable on-demand block generation (POST /generate)")
	network := fs.String("network", cfg.str("network", ""), "network to run on: mainnet, testnet or regtest (default mainnet; -regtest implies regtest)")
	addrIndex := fs.Bool("addrindex", cfg.boolean("addrindex", false), "maintain an address -> transactions index (serves /address/{addr}/history)")
	faucet := fs.Bool("faucet", cfg.boolean("faucet", false), "give coin away via POST /faucet (testnet/regtest only)")
	faucetAmount := fs.String("faucetamount", cfg.str("faucetamount", ""), "faucet payout in DNAS (default 10)")
	faucetCooldown := fs.Int("faucetcooldown", cfg.integer("faucetcooldown", 0), "seconds between faucet payouts to one address or requester (default 60)")
	shareFactor := fs.Int("sharefactor", cfg.integer("sharefactor", 0), "how many times easier a mining share is than a block (default 256)")
	nodeKey := fs.String("nodekey", cfg.str("nodekey", ""), "network identity key file (default: nodekey.json beside -db); NOT the wallet")
	logLevelName := fs.String("loglevel", cfg.str("loglevel", "info"), "log verbosity: error, warn, info or debug")
	logJSON := fs.Bool("logjson", cfg.boolean("logjson", false), "emit one JSON object per log line instead of prose")
	printConfig := fs.Bool("printconfig", false, "print the effective configuration (flags merged over -config) and exit")
	dandelion := fs.Bool("dandelion", cfg.boolean("dandelion", true), "relay new transactions via Dandelion++ stem/fluff (origin privacy)")
	checkpoints := fs.String("checkpoints", cfg.str("checkpoints", ""), "finality checkpoints as comma-separated height:hash pairs")
	upgrades := fs.String("upgrades", cfg.str("upgrades", ""), "consensus upgrade activations as comma-separated name:height pairs (e.g. multioutput:1000)")
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
		node.Infof("checkpoint pinned", "height", h)
	}

	// Schedule consensus upgrades before syncing. Every node on a network must be
	// given the same values: an upgrade activates at a fixed height (a flag day), so
	// nodes that disagree about the height disagree about whether a block is valid.
	for _, u := range parsePeers(*upgrades) {
		name, heightStr, ok := strings.Cut(u, ":")
		h, err := strconv.ParseUint(strings.TrimSpace(heightStr), 10, 64)
		name = strings.TrimSpace(name)
		if !ok || err != nil || name == "" {
			log.Fatalf("bad -upgrades entry %q (want name:height)", u)
		}
		if !core.KnownUpgrade(name) {
			log.Fatalf("unknown upgrade %q (known: %s)", name, strings.Join(core.Upgrades(), ", "))
		}
		core.SetUpgradeHeight(name, h)
		node.Infof("consensus upgrade scheduled", "name", name, "height", h)
	}

	// Logging first, so everything below is emitted at the requested verbosity and
	// in the requested format.
	level, err := node.ParseLevel(*logLevelName)
	if err != nil {
		log.Fatal(err)
	}
	node.SetLogLevel(level)
	node.SetLogJSON(*logJSON)

	// Select the network before anything reads the genesis block or signs
	// anything: the network id is bound into both (see core/network.go). -regtest
	// is kept as the shorthand it has always been.
	netName := *network
	if netName == "" {
		netName = core.MainNet
	}
	if *regtest {
		if netName != core.MainNet && netName != core.RegTest {
			log.Fatalf("-regtest conflicts with -network %s", netName)
		}
		netName = core.RegTest
	}
	if err := core.SetNetwork(netName); err != nil {
		log.Fatal(err)
	}
	*regtest = netName == core.RegTest // on-demand generation follows the network
	node.Infof("network selected", "network", core.NetworkName())
	// A network may isolate itself with a default pre-shared key (regtest does),
	// so a local test node can't accidentally peer with a devnet. An explicit
	// -netkey always wins.
	if *netKey == "" {
		*netKey = core.Network().DefaultNetKey
	}
	if *netKey == "" {
		node.Infof("network is open/permissionless", "note", "encrypted, no shared key; set -netkey for a private net")
	} else {
		node.Infof("network is private", "note", "authenticated by the shared -netkey")
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
		node.Infof("created wallet", "file", *walletPath)
	}
	node.Infof("wallet address", "address", w.Address())

	// The node's NETWORK identity is a separate key from the wallet on purpose:
	// its public key goes to every peer, and a DNAS address is a hash of a public
	// key, so sharing the wallet key hands every peer the address holding the coin
	// (see node/identity.go). It defaults to a file beside the chain.
	identityPath := *nodeKey
	if identityPath == "" {
		identityPath = filepath.Join(filepath.Dir(*dbPath), node.IdentityFile)
	}
	identity, idCreated, err := node.LoadOrCreateIdentity(identityPath, walletPassphrase())
	if err != nil {
		log.Fatal(err)
	}
	if idCreated {
		node.Infof("created node identity", "file", identityPath)
	}

	if *printConfig {
		printEffectiveConfig(effectiveConfig{
			Network: core.NetworkName(), Listen: *listen, Advertise: *advertise, API: *apiAddr,
			Peers: parsePeers(*peersStr), NetKey: *netKey, MaxPeers: *maxPeers,
			Wallet: *walletPath, DB: *dbPath, NodeKey: identityPath,
			Mine: *mine, Regtest: *regtest, Dandelion: *dandelion,
			AddrIndex: *addrIndex, Faucet: *faucet, ShareFactor: *shareFactor,
			MempoolMax: *mempoolMax, MinRelayFee: *minRelayFee,
			APIRate: *apiRate, APIBurst: *apiBurst, Webhooks: parsePeers(*webhooks),
			Prune:    *prune,
			LogLevel: level.String(), LogJSON: *logJSON,
			Checkpoints: parsePeers(*checkpoints), Upgrades: parsePeers(*upgrades),
			APIAuth: os.Getenv("DNAS_API_TOKEN") != "",
		})
		return
	}

	chain, err := core.Open(*dbPath)
	if err != nil {
		log.Fatalf("open chain %s: %v", *dbPath, err)
	}
	node.Infof("chain opened", "height", chain.Height(), "db", *dbPath)
	if *addrIndex {
		chain.EnableAddressIndex() // built once here, maintained as blocks connect
		node.Infof("address index enabled", "serves", "/address/{addr}/history")
	}
	if *prune > 0 {
		keep := chain.EnablePruning(uint64(*prune))
		node.Infof("pruning enabled", "keep_bodies", keep,
			"note", "old bodies, their inclusion proofs and their filters cannot be served")
		if keep != uint64(*prune) {
			node.Warnf("prune raised to the minimum", "asked", *prune, "using", keep,
				"why", "a node must keep the bodies a reorg can reach")
		}
	}

	mp := core.NewMempoolWithPolicy(*mempoolMax, relayFloor)
	if relayFloor > 0 {
		node.Infof("min relay fee set", "per_byte", relayFloor, "note", "base; rises with mempool load")
	}
	n := node.New(node.Config{
		ListenAddr:     *listen,
		AdvertiseAddr:  *advertise,
		Peers:          parsePeers(*peersStr),
		NetKey:         *netKey,
		MaxPeers:       *maxPeers,
		Mine:           *mine,
		Identity:       identity,              // network identity, separate from the wallet
		StateDir:       filepath.Dir(*dbPath), // persist peers/bans/mempool beside the chain
		Regtest:        *regtest,
		Dandelion:      *dandelion,
		ShareFactor:    uint32(*shareFactor),
		Faucet:         *faucet,
		FaucetAmount:   faucetPayout(*faucetAmount),
		FaucetCooldown: time.Duration(*faucetCooldown) * time.Second,
		Webhooks:       parsePeers(*webhooks),
	}, chain, mp, w)
	if *faucet {
		if n.FaucetEnabled() {
			log.Printf("faucet enabled: %s per request, one per %s", core.FormatAmount(n.FaucetAmount()), n.FaucetCooldown())
		} else {
			log.Printf("faucet requested but unavailable on %s", core.NetworkName())
		}
	}
	n.Start()

	srv := api.New(n)
	srv.SetRateLimit(float64(*apiRate), float64(*apiBurst))
	if *apiRate <= 0 {
		node.Infof("API rate limit disabled", "note", "any client may make unlimited requests")
	}
	if srv.AuthEnabled() {
		node.Infof("API writes require a bearer token", "env", "DNAS_API_TOKEN")
	}
	go srv.Start(*apiAddr)

	// Clean shutdown: stop mining, close peers, flush the chain store.
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			node.Infof("shutting down")
			n.Shutdown()
			if err := chain.Close(); err != nil {
				node.Errorf("close chain", "err", err)
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

	// The console runs when there is somebody to type at it. -console forces it
	// on anyway, which is what makes it drivable by a script (and testable): a
	// pipe is not a terminal, so the auto-detection alone leaves the console
	// unreachable to anything but a human.
	if *console || stdinIsTTY() {
		repl(n)
		shutdown() // "quit" from the console, or stdin ran out
	} else {
		select {} // no terminal (e.g. background/demo); wait for a signal
	}
}

// faucetPayout parses the -faucetamount flag (decimal DNAS); an empty value
// leaves the node's default in place.
func faucetPayout(s string) uint64 {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	amount, err := core.ParseAmount(s)
	if err != nil {
		log.Fatalf("bad -faucetamount: %v", err)
	}
	return amount
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
