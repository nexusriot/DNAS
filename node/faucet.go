package node

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// A faucet gives coin away on request. It is what makes a throwaway network
// usable by someone other than whoever mined it: without one, joining a testnet
// means asking a stranger for coin, and every demo starts by mining.
//
// It is refused outright on mainnet — not by policy but by the network
// definition (core.NetworkParams.Faucet), so no flag, config key or API call can
// turn a real chain into a free one. On a network that does allow it, it is still
// off unless the operator asks for it, since it spends the node's own wallet.
//
// Abuse control is deliberately modest, because the coin is worthless by
// construction: one payout per recipient and per requester per cooldown window.

const (
	// DefaultFaucetAmount is what one request pays out when the operator sets no
	// amount: enough to make a handful of transactions, not enough to matter.
	DefaultFaucetAmount = 10 * core.Coin
	// DefaultFaucetCooldown is how long a recipient (and a requester) must wait
	// between payouts.
	DefaultFaucetCooldown = 60 * time.Second
	// maxFaucetKeys bounds the cooldown ledger, so a stream of distinct requesters
	// cannot grow it without limit. Expired entries are swept on each request; this
	// caps what a burst can hold at once.
	maxFaucetKeys = 10_000
)

// faucet tracks when each recipient and requester was last paid.
type faucet struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newFaucet() *faucet { return &faucet{last: map[string]time.Time{}} }

// claim reserves a payout for every key at `now`, or reports how long the
// earliest-eligible key still has to wait. Either all keys are stamped or none
// is, so a refused request does not extend anyone's cooldown.
func (f *faucet) claim(keys []string, cooldown time.Duration, now time.Time) (time.Duration, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, t := range f.last { // sweep expired entries so the ledger stays bounded
		if now.Sub(t) >= cooldown {
			delete(f.last, k)
		}
	}
	for _, k := range keys {
		if t, ok := f.last[k]; ok {
			if wait := cooldown - now.Sub(t); wait > 0 {
				return wait, false
			}
		}
	}
	if len(f.last)+len(keys) > maxFaucetKeys {
		return cooldown, false
	}
	for _, k := range keys {
		f.last[k] = now
	}
	return 0, true
}

// FaucetEnabled reports whether this node will hand out coin: the operator asked
// for it, the network allows it, and the node has a wallet to pay from.
func (n *Node) FaucetEnabled() bool {
	return n.cfg.Faucet && core.Network().Faucet && n.wallet != nil
}

// FaucetAmount is what one request pays out.
func (n *Node) FaucetAmount() uint64 {
	if n.cfg.FaucetAmount > 0 {
		return n.cfg.FaucetAmount
	}
	return DefaultFaucetAmount
}

// FaucetCooldown is how long between payouts to the same recipient or requester.
func (n *Node) FaucetCooldown() time.Duration {
	if n.cfg.FaucetCooldown > 0 {
		return n.cfg.FaucetCooldown
	}
	return DefaultFaucetCooldown
}

// Faucet pays the faucet amount to addr from the node's wallet and broadcasts
// the transaction, returning it. `requester` identifies who asked (the API passes
// the client's IP) and shares the recipient's cooldown, so neither one address
// nor one client can drain the wallet. An empty requester is only rate-limited by
// the recipient address.
func (n *Node) Faucet(addr string) (core.Transaction, error) { return n.FaucetFor(addr, "") }

// FaucetFor is Faucet with an explicit requester identity for rate limiting.
func (n *Node) FaucetFor(addr, requester string) (core.Transaction, error) {
	if !core.Network().Faucet {
		return core.Transaction{}, fmt.Errorf("the faucet is not available on %s", core.NetworkName())
	}
	if !n.cfg.Faucet {
		return core.Transaction{}, errors.New("the faucet is disabled on this node")
	}
	if n.wallet == nil {
		return core.Transaction{}, errors.New("node has no wallet to pay from")
	}
	if err := wallet.ValidateAddress(addr); err != nil {
		return core.Transaction{}, fmt.Errorf("invalid address: %w", err)
	}
	if addr == n.wallet.Address() {
		return core.Transaction{}, errors.New("the faucet cannot pay itself")
	}

	amount := n.FaucetAmount()
	fee := n.faucetFee()
	if have := n.chain.SpendableBalance(n.wallet.Address()); have < amount+fee {
		return core.Transaction{}, fmt.Errorf("faucet is dry: %s spendable, %s needed",
			core.FormatAmount(have), core.FormatAmount(amount+fee))
	}

	keys := []string{"addr:" + addr}
	if requester != "" {
		keys = append(keys, "who:"+requester)
	}
	wait, ok := n.faucet.claim(keys, n.FaucetCooldown(), time.Now())
	if !ok {
		return core.Transaction{}, fmt.Errorf("faucet cooldown: try again in %s", wait.Round(time.Second))
	}

	tx := core.Transaction{
		From:   n.wallet.Address(),
		To:     addr,
		Amount: amount,
		Fee:    fee,
		Nonce:  n.NextNonce(n.wallet.Address()),
		Memo:   "faucet",
	}
	if err := tx.Sign(n.wallet); err != nil {
		return core.Transaction{}, err
	}
	if err := n.SubmitTx(tx); err != nil {
		return core.Transaction{}, err
	}
	return tx, nil
}

// faucetFeeBudget is the transaction size (in bytes) the faucet budgets its fee
// for. A faucet payment is a plain transfer well under this, so the fee clears
// the per-byte rate with room to spare.
const faucetFeeBudget = 500

// faucetFee is what a payout pays. It budgets against the HIGHER of the
// consensus base fee and this node's own relay floor: the two move
// independently — the base fee decays toward MinBaseFee on an idle chain while
// the relay floor stays at its configured base and climbs with mempool occupancy
// — so pricing off the base fee alone produces a transaction the node's own
// mempool then refuses to relay.
func (n *Node) faucetFee() uint64 {
	rate := n.chain.NextBaseFee()
	if floor := n.mempool.MinFee(); floor > rate {
		rate = floor
	}
	return rate * faucetFeeBudget
}
