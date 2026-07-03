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
)

type snapshot struct {
	info    Info
	addr    string
	balance string
	blocks  []Block
	mempool []Tx
}

type (
	tickMsg  time.Time
	stateMsg struct {
		s   snapshot
		err error
	}
	actionMsg string
	// detailMsg carries a one-line status plus a multi-line panel body (used by
	// the multisig and HD-wallet helpers, whose output spans several lines).
	detailMsg struct {
		status string
		detail string
	}
)

type model struct {
	c       *Client
	st      snapshot
	connErr string
	mode    mode
	input   string
	status  string
	detail  string // multi-line result panel (multisig address, HD backup, …)
	w, h    int
}

func fetch(c *Client) tea.Cmd {
	return func() tea.Msg {
		info, err := c.Info()
		if err != nil {
			return stateMsg{err: err}
		}
		s := snapshot{info: info}
		s.addr, _ = c.Address()
		if s.addr != "" {
			s.balance, _ = c.BalanceFmt(s.addr)
		}
		s.blocks, _ = c.Chain()
		s.mempool, _ = c.Mempool()
		return stateMsg{s: s}
	}
}

func tick() tea.Cmd {
	return tea.Tick(1500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Init() tea.Cmd { return tea.Batch(fetch(m.c), tick()) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tickMsg:
		return m, tea.Batch(fetch(m.c), tick())
	case stateMsg:
		if msg.err != nil {
			m.connErr = msg.err.Error()
		} else {
			m.st = msg.s
			m.connErr = ""
		}
		return m, nil
	case actionMsg:
		m.status = string(msg)
		return m, fetch(m.c)
	case detailMsg:
		m.status, m.detail = msg.status, msg.detail
		return m, fetch(m.c)
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
		return m, fetch(m.c)
	case "s":
		m.mode, m.status = modeSend, ""
	case "v":
		m.mode, m.status = modeVerify, ""
	case "x":
		m.mode, m.status, m.detail = modeMultisig, "", ""
	case "h":
		m.mode, m.status, m.detail = modeHD, "", ""
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
		f := strings.Fields(in)
		if len(f) < 2 {
			return func() tea.Msg { return actionMsg("usage: <to-address> <amount> [fee]") }
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
		return func() tea.Msg {
			h, err := c.Send(to, amt, fee, "")
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
		fmt.Fprintf(&b, "%s live  height %s  diff %d  work %s  mempool %d  minfee %s  peers %d  mining %s\n",
			okStyle.Render("●"), okStyle.Render(fmt.Sprint(i.Height)), i.NextDifficulty, i.Work, i.Mempool, fmtAmt(i.MinRelayFee), len(i.Peers), mining)
	}

	fmt.Fprintf(&b, "\n%s %s\n%s %s\n", dimStyle.Render("wallet "), m.st.addr, dimStyle.Render("balance"), okStyle.Render(m.st.balance))

	// Recent blocks (newest first).
	fmt.Fprintln(&b, "\n"+hdrStyle.Render("recent blocks"))
	blocks := m.st.blocks
	for i := len(blocks) - 1; i >= 0 && i > len(blocks)-9; i-- {
		bl := blocks[i]
		fmt.Fprintf(&b, "  %s %s  %d tx  diff %d\n",
			keyStyle.Render(fmt.Sprintf("#%d", bl.Index)), dimStyle.Render(shortHash(bl.Hash)), len(bl.Transactions), bl.Difficulty)
	}

	// Mempool.
	fmt.Fprintln(&b, "\n"+hdrStyle.Render(fmt.Sprintf("mempool (%d)", len(m.st.mempool))))
	for _, tx := range m.st.mempool {
		fmt.Fprintf(&b, "  %s → %s  %s  fee %s\n",
			shortAddr(tx.From), shortAddr(tx.To), fmtAmt(tx.Amount), fmtAmt(tx.Fee))
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
		fmt.Fprintln(&b, keyStyle.Render("send> ")+"<to> <amount> [fee]: "+m.input+"▏")
	case modeVerify:
		fmt.Fprintln(&b, keyStyle.Render("verify> ")+"tx hash: "+m.input+"▏")
	case modeMultisig:
		fmt.Fprintln(&b, keyStyle.Render("multisig> ")+"<threshold> <pubkey-hex>…: "+m.input+"▏")
	case modeHD:
		fmt.Fprintln(&b, keyStyle.Render("hd> ")+"mnemonic to restore (blank = generate): "+m.input+"▏")
	default:
		fmt.Fprintln(&b, dimStyle.Render("[s]end  [v]erify  [m]ine  [x]multisig  [h]d-wallet  [r]efresh  [q]uit"))
	}
	if m.status != "" {
		fmt.Fprintln(&b, okStyle.Render("» "+m.status))
	}
	return b.String()
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
	dnasBin := flag.String("dnas", "dnas", "path to the dnas binary (for -spawn)")
	flag.Parse()

	if *spawn {
		stop, err := spawnNode(*dnasBin, *api)
		if err != nil {
			fmt.Fprintln(os.Stderr, "spawn:", err)
			os.Exit(1)
		}
		defer stop()
	}

	p := tea.NewProgram(model{c: NewClient(*api)}, tea.WithAltScreen())
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
