// Command dnas-tui is a terminal client for a DNAS node's HTTP API: a live
// dashboard (chain status, blocks, mempool, wallet balance) plus send, SPV
// verify, and a mining toggle.
//
// Usage:
//
//	dnas-tui -api localhost:8080          connect to a running node
//	dnas-tui -spawn                       launch a local node and connect to it
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const coin = 100_000_000

// recentBlockCount is how many of the newest blocks the dashboard asks for. The
// panel shows 8; a few spare keep it populated across a reorg.
const recentBlockCount = 12

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	keyStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	hdrStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("244")).Underline(true)
)

type mode int

const (
	modeNormal mode = iota
	modeSend
	modeVerify
	modeMultisig
	modeHD
	modeWatch
)

type snapshot struct {
	info    Info
	addr    string
	balance string
	blocks  []Block
	mempool []Tx
	fees    MempoolStats
}

// watchState follows one transaction from submission to confirmation. A wallet
// otherwise has to re-run a lookup by hand to find out whether a payment landed;
// this keeps the answer on screen and updates it as blocks arrive.
type watchState struct {
	hash   string
	status string
	confs  uint64
	height uint64
}

type (
	tickMsg  time.Time
	stateMsg struct {
		s   snapshot
		err error
	}
	sseMsg     string   // a live event arrived (its type); triggers an immediate refresh
	sseDownMsg struct{} // the event stream ended; fall back to polling
	actionMsg  string
	watchMsg   watchState // the watched transaction's latest status
	// detailMsg carries a one-line status plus a multi-line panel body (used by
	// the multisig and HD-wallet helpers, whose output spans several lines).
	detailMsg struct {
		status string
		detail string
	}
)

type model struct {
	c       *Client
	wallet  localWallet // set when -key is given: payments are signed locally
	st      snapshot
	connErr string
	mode    mode
	input   string
	status  string
	detail  string        // multi-line result panel (multisig address, HD backup, …)
	events  <-chan string // live SSE events (nil once the stream is unavailable)
	watch   *watchState   // a transaction being followed to confirmation (nil = none)
	w, h    int
}

func fetch(c *Client, w localWallet) tea.Cmd {
	return func() tea.Msg {
		info, err := c.Info()
		if err != nil {
			return stateMsg{err: err}
		}
		s := snapshot{info: info}
		// In self-custodial mode the wallet panel is about the LOCAL key. The
		// node's own address is not the user's and showing its balance there would
		// be actively misleading — it is the balance they cannot spend.
		if w.selfCustodial() {
			s.addr = w.address
		} else {
			s.addr, _ = c.Address()
		}
		if s.addr != "" {
			s.balance, _ = c.BalanceFmt(s.addr)
		}
		s.blocks, _ = c.RecentBlocks(recentBlockCount)
		s.mempool, _ = c.Mempool()
		s.fees, _ = c.MempoolStats()
		return stateMsg{s: s}
	}
}

// tick schedules the next poll. When a live event stream is connected the poll
// is only a slow safety net; without one it is the primary refresh.
func (m model) tick() tea.Cmd {
	d := 1500 * time.Millisecond
	if m.events != nil {
		d = 5 * time.Second
	}
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// listen waits for the next SSE event (or the stream closing), if one is open.
func (m model) listen() tea.Cmd {
	ch := m.events
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return sseDownMsg{}
		}
		return sseMsg(ev)
	}
}

func (m model) Init() tea.Cmd { return tea.Batch(fetch(m.c, m.wallet), m.tick(), m.listen()) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tickMsg:
		return m, tea.Batch(fetch(m.c, m.wallet), m.tick())
	case sseMsg:
		// A block/tx/reorg happened: refresh now and keep listening.
		return m, tea.Batch(fetch(m.c, m.wallet), m.listen())
	case sseDownMsg:
		m.events = nil // stream ended; the poll loop takes over
		return m, nil
	case stateMsg:
		if msg.err != nil {
			m.connErr = msg.err.Error()
		} else {
			m.st = msg.s
			m.connErr = ""
		}
		// Every refresh re-checks the watched transaction, so it advances on the same
		// signal (an SSE event or the poll) that moves everything else.
		if m.watch != nil {
			return m, pollWatch(m.c, m.watch.hash)
		}
		return m, nil
	case watchMsg:
		w := watchState(msg)
		m.watch = &w
		return m, nil
	case actionMsg:
		m.status = string(msg)
		return m, fetch(m.c, m.wallet)
	case detailMsg:
		m.status, m.detail = msg.status, msg.detail
		return m, fetch(m.c, m.wallet)
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.mode != modeNormal {
		switch k.Type {
		case tea.KeyEsc:
			m.mode, m.input = modeNormal, ""
		case tea.KeyEnter:
			in, mode := strings.TrimSpace(m.input), m.mode
			m.mode, m.input = modeNormal, ""
			return m, m.submit(mode, in)
		case tea.KeyBackspace:
			if len(m.input) > 0 {
				m.input = m.input[:len(m.input)-1]
			}
		case tea.KeySpace:
			m.input += " "
		default:
			if len(k.Runes) > 0 {
				m.input += string(k.Runes)
			}
		}
		return m, nil
	}

	switch k.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "r":
		return m, fetch(m.c, m.wallet)
	case "s":
		m.mode, m.status = modeSend, ""
	case "v":
		m.mode, m.status = modeVerify, ""
	case "x":
		m.mode, m.status, m.detail = modeMultisig, "", ""
	case "h":
		m.mode, m.status, m.detail = modeHD, "", ""
	case "w":
		m.mode, m.status = modeWatch, ""
	case "c":
		// Stop following a transaction (the panel stays until something replaces it
		// otherwise, which is wrong once the payment is long confirmed).
		m.watch, m.status = nil, "stopped watching"
	case "m":
		on := !m.st.info.Mining
		c := m.c
		return m, func() tea.Msg {
			if err := c.SetMining(on); err != nil {
				return actionMsg("mine: " + err.Error())
			}
			return actionMsg(fmt.Sprintf("mining set to %v", on))
		}
	}
	return m, nil
}

func (m model) submit(md mode, in string) tea.Cmd {
	c := m.c
	switch md {
	case modeSend:
		// A pasted `dnas:` payment URI stands in for "<to> <amount>", carrying
		// both (and a memo) so neither has to be retyped.
		f, uriMemo, err := expandSendInput(strings.Fields(in))
		if err != nil {
			msg := err.Error()
			return func() tea.Msg { return actionMsg(msg) }
		}
		if len(f) < 2 {
			return func() tea.Msg { return actionMsg("usage: <to-address> <amount> [fee]  (or a dnas: URI)") }
		}
		amt, err := parseDNAS(f[1])
		if err != nil {
			return func() tea.Msg { return actionMsg("bad amount") }
		}
		var fee uint64
		if len(f) > 2 {
			if fee, err = parseDNAS(f[2]); err != nil {
				return func() tea.Msg { return actionMsg("bad fee") }
			}
		}
		to := f[0]
		if m.wallet.selfCustodial() {
			// Signed locally by the `dnas` binary; the node only ever sees the signed
			// transaction. The amount is passed through as written so there is one
			// parser for it (the CLI's), not two that could round differently.
			w, api, amount, memo := m.wallet, c.base, f[1], uriMemo
			feeArg := ""
			if len(f) > 2 {
				feeArg = f[2]
			}
			return func() tea.Msg {
				line, err := w.send(api, to, amount, feeArg, memo)
				if err != nil {
					return actionMsg("send failed: " + err.Error())
				}
				return actionMsg(line)
			}
		}
		memo := uriMemo
		return func() tea.Msg {
			h, err := c.Send(to, amt, fee, memo)
			if err != nil {
				return actionMsg("send failed: " + err.Error())
			}
			return actionMsg("submitted " + shortHash(h))
		}
	case modeVerify:
		txh := in
		return func() tea.Msg {
			res, err := c.VerifyTx(txh)
			if err != nil {
				return actionMsg("verify: " + err.Error())
			}
			return actionMsg("SPV: " + res)
		}
	case modeMultisig:
		f := strings.Fields(in)
		if len(f) < 2 {
			return func() tea.Msg { return actionMsg("usage: <threshold> <pubkey-hex> <pubkey-hex> …") }
		}
		threshold, err := strconv.Atoi(f[0])
		if err != nil {
			return func() tea.Msg { return actionMsg("bad threshold") }
		}
		pubkeys := f[1:]
		return func() tea.Msg {
			addr, err := c.MultisigAddress(threshold, pubkeys)
			if err != nil {
				return actionMsg("multisig: " + err.Error())
			}
			detail := fmt.Sprintf("%d-of-%d multisig address:\n  %s\n\nFund it like any address; spend with a signed multisig tx.", threshold, len(pubkeys), addr)
			return detailMsg{status: "multisig address derived", detail: detail}
		}
	case modeWatch:
		if in == "" {
			return func() tea.Msg { return actionMsg("usage: <tx hash>") }
		}
		return pollWatch(c, in)
	case modeHD:
		mnemonic := in // empty -> generate a fresh wallet
		return func() tea.Msg {
			phrase, addrs, err := c.HDWallet(mnemonic, 5)
			if err != nil {
				return actionMsg("hd: " + err.Error())
			}
			var d strings.Builder
			verb := "generated"
			if mnemonic != "" {
				verb = "restored"
			}
			fmt.Fprintf(&d, "BIP39 backup phrase — write it down:\n  %s\n\nfirst %d addresses:\n", phrase, len(addrs))
			for i, a := range addrs {
				fmt.Fprintf(&d, "  [%d] %s\n", i, a)
			}
			return detailMsg{status: "HD wallet " + verb, detail: d.String()}
		}
	}
	return nil
}

// pollWatch asks the node where one transaction stands. It is issued once per
// refresh rather than on a timer of its own, so watching costs no extra polling.
func pollWatch(c *Client, hash string) tea.Cmd {
	return func() tea.Msg {
		st, err := c.TxStatus(hash)
		if err != nil {
			return watchMsg(watchState{hash: hash, status: "error: " + err.Error()})
		}
		return watchMsg(watchState{hash: hash, status: st.Status, confs: st.Confirmations, height: st.Height})
	}
}

func (m model) View() string {
	var b strings.Builder
	fmt.Fprintln(&b, titleStyle.Render("⛓ DNAS")+dimStyle.Render("  terminal client — "+m.c.base))

	if m.connErr != "" {
		fmt.Fprintln(&b, errStyle.Render("● offline: "+m.connErr))
	} else {
		i := m.st.info
		mining := dimStyle.Render("off")
		if i.Mining {
			mining = okStyle.Render("ON")
		}
		fmt.Fprintf(&b, "%s live  height %s  diff %.2f  work %s  mempool %d  basefee %s  minfee %s  peers %d  mining %s\n",
			okStyle.Render("●"), okStyle.Render(fmt.Sprint(i.Height)), i.NextDifficulty, i.Work, i.Mempool, fmtAmt(i.BaseFee), fmtAmt(i.MinRelayFee), len(i.Peers), mining)
	}

	// Which key a payment would come from is not a detail: node-signed means the
	// node's coin, self-custodial means yours. The label says which.
	label := "wallet "
	if m.wallet.selfCustodial() {
		label = "wallet*"
	}
	fmt.Fprintf(&b, "\n%s %s\n%s %s\n", dimStyle.Render(label), m.st.addr, dimStyle.Render("balance"), okStyle.Render(m.st.balance))
	if m.wallet.selfCustodial() {
		fmt.Fprintln(&b, dimStyle.Render("        * signed locally from "+m.wallet.keyFile+"; the node never sees the key"))
	}

	// Recent blocks (newest first). The snapshot already holds only the newest
	// recentBlockCount of them, so this reverses rather than searching.
	fmt.Fprintln(&b, "\n"+hdrStyle.Render("recent blocks"))
	blocks := m.st.blocks
	for i := len(blocks) - 1; i >= 0 && i > len(blocks)-9; i-- {
		bl := blocks[i]
		fmt.Fprintf(&b, "  %s %s  %d tx  diff %.2f\n",
			keyStyle.Render(fmt.Sprintf("#%d", bl.Index)), dimStyle.Render(shortHash(bl.Hash)), len(bl.Transactions), difficultyOf(bl.Bits))
	}

	// Mempool.
	fmt.Fprintln(&b, "\n"+hdrStyle.Render(fmt.Sprintf("mempool (%d)", len(m.st.mempool))))
	for _, tx := range m.st.mempool {
		fmt.Fprintf(&b, "  %s → %s  %s  fee %s\n",
			shortAddr(tx.From), shortAddr(tx.To), fmtAmt(tx.Amount), fmtAmt(tx.Fee))
	}

	// What the queue is paying. Depth alone doesn't say whether a fee will be
	// picked up next block; the distribution of rates does.
	if fees := m.st.fees; fees.Count > 0 {
		fmt.Fprintln(&b, "\n"+hdrStyle.Render("fee rates (base units per byte)"))
		fmt.Fprintf(&b, "  %s  min %d  median %s  max %d  basefee %d  %d bytes queued\n",
			dimStyle.Render("rate"), fees.MinRate, okStyle.Render(fmt.Sprint(fees.MedianRate)),
			fees.MaxRate, fees.BaseFee, fees.Bytes)
		for _, bucket := range fees.Buckets {
			if bucket.Count == 0 {
				continue
			}
			label := fmt.Sprintf("%d-%d", bucket.From, bucket.To)
			if bucket.To == 0 {
				label = fmt.Sprintf("%d+", bucket.From)
			}
			fmt.Fprintf(&b, "  %-9s %-20s %d tx  %d B\n",
				label, bar(bucket.Count, fees.Count, 20), bucket.Count, bucket.Bytes)
		}
	}

	// A transaction being followed from submission to confirmation.
	if m.watch != nil {
		fmt.Fprintln(&b, "\n"+hdrStyle.Render("watching"))
		switch m.watch.status {
		case "confirmed":
			fmt.Fprintf(&b, "  %s %s  in block %d, %d confirmation(s)\n",
				okStyle.Render("✓"), shortHash(m.watch.hash), m.watch.height, m.watch.confs)
		case "pending":
			fmt.Fprintf(&b, "  %s %s  in the mempool, not yet mined\n", keyStyle.Render("…"), shortHash(m.watch.hash))
		case "unknown":
			fmt.Fprintf(&b, "  %s %s  this node has never seen it\n", dimStyle.Render("?"), shortHash(m.watch.hash))
		default:
			fmt.Fprintf(&b, "  %s %s  %s\n", errStyle.Render("!"), shortHash(m.watch.hash), m.watch.status)
		}
		fmt.Fprintln(&b, dimStyle.Render("  press [c] to stop watching"))
	}

	// Result panel (multisig address / HD backup phrase), shown until cleared.
	if m.detail != "" {
		fmt.Fprintln(&b, "\n"+hdrStyle.Render("result"))
		fmt.Fprintln(&b, okStyle.Render(m.detail))
	}

	// Footer: input prompt or help + status.
	b.WriteString("\n")
	switch m.mode {
	case modeSend:
		prompt := "send> "
		if m.wallet.selfCustodial() {
			prompt = "send (signed locally)> "
		}
		fmt.Fprintln(&b, keyStyle.Render(prompt)+"<to> <amount> [fee] or dnas: URI: "+m.input+"▏")
	case modeVerify:
		fmt.Fprintln(&b, keyStyle.Render("verify> ")+"tx hash: "+m.input+"▏")
	case modeMultisig:
		fmt.Fprintln(&b, keyStyle.Render("multisig> ")+"<threshold> <pubkey-hex>…: "+m.input+"▏")
	case modeHD:
		fmt.Fprintln(&b, keyStyle.Render("hd> ")+"mnemonic to restore (blank = generate): "+m.input+"▏")
	case modeWatch:
		fmt.Fprintln(&b, keyStyle.Render("watch> ")+"tx hash to follow: "+m.input+"▏")
	default:
		fmt.Fprintln(&b, dimStyle.Render("[s]end  [v]erify  [w]atch  [m]ine  [x]multisig  [h]d-wallet  [r]efresh  [q]uit"))
	}
	if m.status != "" {
		fmt.Fprintln(&b, okStyle.Render("» "+m.status))
	}
	return b.String()
}

// bar renders a proportional bar of `width` characters for count out of total.
func bar(count, total, width int) string {
	if total <= 0 || count <= 0 {
		return ""
	}
	filled := count * width / total
	if filled == 0 {
		filled = 1 // a non-empty bucket always shows something
	}
	return strings.Repeat("█", filled)
}

func parseDNAS(s string) (uint64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	return uint64(math.Round(f * coin)), nil
}

func fmtAmt(u uint64) string { return fmt.Sprintf("%.8f", float64(u)/coin) }
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}
func shortAddr(a string) string {
	if a == "COINBASE" {
		return "COINBASE"
	}
	return shortHash(a)
}

func main() {
	api := flag.String("api", "localhost:8080", "node HTTP API address")
	spawn := flag.Bool("spawn", false, "launch a local mining-capable node and connect to it")
	dnasBin := flag.String("dnas", "dnas", "path to the dnas binary (for -spawn and for -key)")
	keyFile := flag.String("key", "", "sign payments locally with this key file (self-custodial; the node never sees it)")
	stateFile := flag.String("wallet", "spvwallet.json", "light-wallet state file used with -key")
	flag.Parse()

	if *spawn {
		stop, err := spawnNode(*dnasBin, *api)
		if err != nil {
			fmt.Fprintln(os.Stderr, "spawn:", err)
			os.Exit(1)
		}
		defer stop()
	}

	cl := NewClient(*api)
	m := model{c: cl}
	if *keyFile != "" {
		m.wallet = localWallet{keyFile: *keyFile, binPath: *dnasBin, state: *stateFile}
		// Resolved up front: a missing key file or an unusable binary is reported
		// now, not in the middle of somebody's payment.
		if err := m.wallet.resolve(); err != nil {
			fmt.Fprintln(os.Stderr, "self-custodial mode:", err)
			os.Exit(1)
		}
		if recorded, ok := keyFileAddress(*keyFile); ok && recorded != m.wallet.address {
			fmt.Fprintf(os.Stderr, "self-custodial mode: %s records address %s but %s derived %s\n",
				*keyFile, recorded, *dnasBin, m.wallet.address)
			os.Exit(1)
		}
	}
	// Subscribe to the live event stream if the node supports it; fall back to
	// polling otherwise. The stream is cancelled when the program exits.
	if ch, cancel, err := cl.Subscribe(); err == nil {
		m.events = ch
		defer cancel()
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// spawnNode launches a headless node whose API is at apiAddr, returning a stop
// function. Its logs go to a file in a temp dir so they don't corrupt the UI.
func spawnNode(bin, apiAddr string) (func(), error) {
	dir, err := os.MkdirTemp("", "dnas-tui-node-")
	if err != nil {
		return nil, err
	}
	logf, _ := os.Create(filepath.Join(dir, "node.log"))
	cmd := exec.Command(bin, "node",
		"-api", apiAddr, "-listen", ":0",
		"-db", filepath.Join(dir, "chain.db"),
		"-wallet", filepath.Join(dir, "wallet.json"))
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Give it a moment to bind the API.
	time.Sleep(600 * time.Millisecond)
	return func() {
		_ = cmd.Process.Kill()
		if logf != nil {
			logf.Close()
		}
	}, nil
}
