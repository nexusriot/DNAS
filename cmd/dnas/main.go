// Command dnas runs a node of the DNAS toy cryptocurrency, or manages wallets.
//
//	dnas node   [flags]      run a full node (default if no subcommand)
//	dnas wallet new   [-o]   create a new wallet key file
//	dnas wallet address [-o] print the address of a wallet key file
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/nexusriot/DNAS/api"
	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
	"github.com/nexusriot/DNAS/wallet"
)

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
	case "help", "-h", "--help":
		usage()
	default:
		// Bare flags (e.g. `dnas -mine`) are treated as node flags.
		runNode(args)
	}
}

func usage() {
	fmt.Println(`dnas - a small proof-of-work cryptocurrency

Usage:
  dnas node [flags]          run a node (default)
  dnas wallet new [-o FILE]  create a wallet
  dnas wallet address [-o FILE]

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
  -mine           enable mining`)
}

func runWallet(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: dnas wallet [new|address] [-o FILE]")
		return
	}
	fs := flag.NewFlagSet("wallet", flag.ExitOnError)
	out := fs.String("o", "wallet.json", "wallet key file")
	_ = fs.Parse(args[1:])

	switch args[0] {
	case "new":
		w, err := wallet.New()
		if err != nil {
			log.Fatal(err)
		}
		if err := w.Save(*out); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("wrote %s\naddress: %s\n", *out, w.Address())
	case "address":
		w, err := wallet.Load(*out)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(w.Address())
	default:
		fmt.Println("unknown wallet command:", args[0])
	}
}

func runNode(args []string) {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	listen := fs.String("listen", ":3000", "p2p listen address")
	advertise := fs.String("advertise", "", "address peers should dial us at (default: -listen)")
	apiAddr := fs.String("api", ":8080", "HTTP API address")
	peersStr := fs.String("peers", "", "comma-separated seed peer addresses")
	walletPath := fs.String("wallet", "wallet.json", "wallet key file (created if missing)")
	dbPath := fs.String("db", "chain.json", "blockchain storage file")
	netKey := fs.String("netkey", node.DefaultNetKey, "pre-shared network key (peers must match)")
	maxPeers := fs.Int("maxpeers", node.DefaultMaxPeers, "maximum outbound peer connections")
	mempoolMax := fs.Int("mempool", core.DefaultMempoolSize, "max pending transactions")
	mine := fs.Bool("mine", false, "enable mining")
	_ = fs.Parse(args)

	w, created, err := wallet.LoadOrCreate(*walletPath)
	if err != nil {
		log.Fatalf("wallet: %v", err)
	}
	if created {
		log.Printf("created new wallet %s", *walletPath)
	}
	log.Printf("wallet address: %s", w.Address())

	var chain *core.Blockchain
	if _, statErr := os.Stat(*dbPath); statErr == nil {
		chain, err = core.Load(*dbPath)
		if err != nil {
			log.Fatalf("load chain: %v", err)
		}
		log.Printf("loaded chain height=%d", chain.Height())
	} else {
		chain = core.NewBlockchain()
		log.Printf("new chain (genesis %s)", chain.Tip().Hash[:10])
	}

	mp := core.NewMempoolWithLimit(*mempoolMax)
	n := node.New(node.Config{
		ListenAddr:    *listen,
		AdvertiseAddr: *advertise,
		Peers:         parsePeers(*peersStr),
		NetKey:        *netKey,
		MaxPeers:      *maxPeers,
		Mine:          *mine,
		DBPath:        *dbPath,
	}, chain, mp, w)
	n.Start()

	go api.New(n).Start(*apiAddr)

	if stdinIsTTY() {
		repl(n)
	} else {
		select {} // no terminal (e.g. background/demo); run until killed
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
			fmt.Printf("height=%d diff(next)=%d tip=%s mempool=%d peers=%d\n",
				tip.Index, n.Chain().NextDifficulty(), tip.Hash[:10], n.Mempool().Size(), len(n.PeerAddrs()))
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
