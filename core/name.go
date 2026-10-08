package core

import (
	"errors"
	"fmt"
	"strings"
)

// A name service: human-readable names that resolve to addresses, committed in
// the state root like everything else.
//
// Why it is in consensus rather than beside it. A name is only useful if a
// payer can be SURE it resolves to the address the payee meant — a name service
// you have to trust a server about is a phishing surface with extra steps. The
// state trie already proves both membership and absence against a header the
// proof of work covers, so putting names in it means a light client can verify
// "this name belongs to this address" and "this name is unclaimed" without
// trusting whoever answered. Nothing else here can offer that: the asset
// registry, by contrast, is derived state no header commits, which is exactly
// why asset mint authority is proven by id derivation rather than a lookup.
//
// Names live in the same account map as balances, under a key no address can
// occupy. Two things keep them apart:
//
//   - The key is prefixed with a NUL byte, which `validTicker`-style address
//     rules never permit.
//   - The trie hashes a name key under a DIFFERENT domain tag (see
//     trieKeyFor), so even a crafted address string cannot land on a name's
//     slot in the trie. Prefixing alone would be a string-comparison defence;
//     the domain tag makes it a cryptographic one.
//
// Registration is a lease, not a freehold. A name expires, and an expired name
// may be claimed by anyone. A permanent namespace is a namespace that is
// squatted once and then dead, and there is no authority here to appeal to.

const (
	// nameKeyPrefix marks a state entry as a name record rather than an
	// account. The leading NUL is deliberate: it cannot appear in an address.
	nameKeyPrefix = "\x00name:"

	// MinNameLen and MaxNameLen bound a name. The lower bound exists because
	// very short names are the ones worth squatting and the upper because the
	// name is committed state every node carries forever.
	MinNameLen = 3
	MaxNameLen = 32

	// NameLeasePeriod is how many blocks a registration or renewal lasts.
	NameLeasePeriod = 100_000

	// MaxNameLeases bounds how many periods may be bought at once, so a lease
	// cannot be extended past the point where the expiry arithmetic is
	// meaningful.
	MaxNameLeases = 10
)

// Name operations.
const (
	NameOpRegister = "register" // claim an unowned or expired name
	NameOpRenew    = "renew"    // extend a name you own
	NameOpTransfer = "transfer" // hand a name to another address
	NameOpRelease  = "release"  // give a name up before it expires
)

// NameOp is a registration, renewal, transfer or release, carried on a
// transaction.
type NameOp struct {
	Op   string `json:"op"`
	Name string `json:"name"`
	// Target is the address a register or transfer points the name at. Empty on
	// a register means the sender.
	Target string `json:"target,omitempty"`
	// Periods is how many lease periods to buy, for register and renew.
	// Zero is read as one.
	Periods uint32 `json:"periods,omitempty"`
}

// IsNameOp reports whether a transaction carries a name operation.
func (t Transaction) IsNameOp() bool { return t.Name != nil }

// isEmpty reports whether an account entry holds nothing at all. Used to drop
// a released name record rather than store a husk of one.
func (a Account) isEmpty() bool {
	return a.Balance == 0 && a.Nonce == 0 && len(a.Assets) == 0 && a.Owner == "" && a.Expires == 0
}

// nameStateKey is where a name's record lives in the account map.
func nameStateKey(name string) string { return nameKeyPrefix + name }

// isNameKey reports whether a state key is a name record rather than an account.
func isNameKey(k string) bool { return strings.HasPrefix(k, nameKeyPrefix) }

// nameFromKey recovers the name from a state key.
func nameFromKey(k string) string { return strings.TrimPrefix(k, nameKeyPrefix) }

// ValidName checks a name's shape.
//
// Lowercase alphanumerics and hyphens only, and not at the ends. The
// restriction is about CONFUSION rather than tidiness: a namespace that admits
// uppercase, or Unicode, admits two names that render identically and resolve
// differently, which turns the whole feature into a way to be paid by mistake.
func ValidName(name string) error {
	if len(name) < MinNameLen || len(name) > MaxNameLen {
		return fmt.Errorf("a name must be %d..%d characters, got %d", MinNameLen, MaxNameLen, len(name))
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(name)-1 {
				return errors.New("a name may not start or end with a hyphen")
			}
			if name[i-1] == '-' {
				return errors.New("a name may not contain two hyphens in a row")
			}
		default:
			return errors.New("a name may contain only lowercase letters, digits and hyphens")
		}
	}
	return nil
}

// Validate checks a name operation's shape, independent of chain state.
func (n *NameOp) Validate() error {
	if n == nil {
		return errors.New("missing name operation")
	}
	if err := ValidName(n.Name); err != nil {
		return err
	}
	switch n.Op {
	case NameOpRegister, NameOpRenew:
		if n.Periods > MaxNameLeases {
			return fmt.Errorf("at most %d lease periods at a time, got %d", MaxNameLeases, n.Periods)
		}
	case NameOpTransfer:
		if n.Target == "" {
			return errors.New("a transfer needs a target address")
		}
	case NameOpRelease:
		if n.Target != "" || n.Periods != 0 {
			return errors.New("a release takes no target or period")
		}
	default:
		return fmt.Errorf("unknown name operation %q", n.Op)
	}
	if n.Target != "" && isNameKey(n.Target) {
		return errors.New("a name cannot be pointed at a name record")
	}
	return nil
}

// periods is the lease length an operation buys, with zero read as one.
func (n *NameOp) periods() uint64 {
	if n.Periods == 0 {
		return 1
	}
	return uint64(n.Periods)
}

// NameRecord is what a name resolves to.
type NameRecord struct {
	Name    string `json:"name"`
	Owner   string `json:"owner"`
	Expires uint64 `json:"expires"`
}

// nameRecordOf reads a name record out of a state entry.
func nameRecordOf(name string, acc Account) NameRecord {
	return NameRecord{Name: name, Owner: acc.Owner, Expires: acc.Expires}
}

// heldAt reports whether a record is still in force at a height. An expired
// name is unowned: anyone may claim it.
func (r NameRecord) heldAt(height uint64) bool {
	return r.Owner != "" && r.Expires > height
}

// applyNameOp applies a registration, renewal, transfer or release.
//
// The fee is paid in coin like every other operation, and the name record is
// written into the same state the balances live in, so it is committed by the
// block's state root and provable to a light client.
func applyNameOp(state map[string]Account, tx Transaction, height, reserve, payerReserve uint64, set func(string, Account)) error {
	op := tx.Name
	if err := op.Validate(); err != nil {
		return err
	}

	sender := state[tx.From]
	if tx.Nonce != sender.Nonce {
		return fmt.Errorf("bad nonce for %s: got %d, want %d", tx.From, tx.Nonce, sender.Nonce)
	}
	fee := senderFee(tx)
	need := fee + reserve
	if need < fee {
		return errors.New("fee+reserve overflow")
	}
	if sender.Balance < need {
		return fmt.Errorf("insufficient spendable balance for %s: have %d, need fee %d (%d immature)",
			tx.From, sender.Balance, fee, reserve)
	}

	key := nameStateKey(op.Name)
	rec := nameRecordOf(op.Name, state[key])

	switch op.Op {
	case NameOpRegister:
		// An expired name is unowned, which is the whole point of the lease: a
		// namespace nobody can reclaim is a namespace that is squatted once.
		if rec.heldAt(height) {
			if rec.Owner != tx.From {
				return fmt.Errorf("name %q is held by %s until block %d", op.Name, rec.Owner, rec.Expires)
			}
			return fmt.Errorf("name %q is already yours until block %d (use renew)", op.Name, rec.Expires)
		}
		target := op.Target
		if target == "" {
			target = tx.From
		}
		rec = NameRecord{Name: op.Name, Owner: target, Expires: height + op.periods()*NameLeasePeriod}
	case NameOpRenew:
		if !rec.heldAt(height) {
			return fmt.Errorf("name %q is not currently held, so there is nothing to renew", op.Name)
		}
		if rec.Owner != tx.From {
			return fmt.Errorf("name %q is held by %s, not %s", op.Name, rec.Owner, tx.From)
		}
		// Renewal extends from the CURRENT expiry, not from now, so renewing
		// early is never a way to lose time.
		rec.Expires += op.periods() * NameLeasePeriod
	case NameOpTransfer:
		if !rec.heldAt(height) {
			return fmt.Errorf("name %q is not currently held", op.Name)
		}
		if rec.Owner != tx.From {
			return fmt.Errorf("name %q is held by %s, not %s", op.Name, rec.Owner, tx.From)
		}
		rec.Owner = op.Target
	case NameOpRelease:
		if !rec.heldAt(height) {
			return fmt.Errorf("name %q is not currently held", op.Name)
		}
		if rec.Owner != tx.From {
			return fmt.Errorf("name %q is held by %s, not %s", op.Name, rec.Owner, tx.From)
		}
		rec = NameRecord{Name: op.Name}
	}

	sender.Balance -= fee
	sender.Nonce++
	set(tx.From, sender)

	if rec.Owner == "" {
		// A released name is DELETED from state rather than stored empty, so the
		// state root of a chain where a name was claimed and released is the same
		// as one where it never was. An empty record left behind would be
		// permanent state that commits to nothing.
		set(key, Account{})
	} else {
		set(key, Account{Owner: rec.Owner, Expires: rec.Expires})
	}
	return chargeSponsor(state, tx, payerReserve, set)
}

// LookupName resolves a name against the current tip.
//
// The second return says whether the name is currently held. A name that has
// expired reports false with the stale record, so a caller can tell "never
// registered" from "lapsed last week" — which matters when deciding whether to
// warn somebody that the address they used to pay has been reclaimed.
func (bc *Blockchain) LookupName(name string) (NameRecord, bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	acc, ok := bc.state[nameStateKey(name)]
	if !ok {
		return NameRecord{Name: name}, false
	}
	rec := nameRecordOf(name, acc)
	return rec, rec.heldAt(bc.blocks[len(bc.blocks)-1].Index)
}

// Names lists every name record the chain holds, newest expiry last.
func (bc *Blockchain) Names() []NameRecord {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	height := bc.blocks[len(bc.blocks)-1].Index
	var out []NameRecord
	for k, acc := range bc.state {
		if !isNameKey(k) {
			continue
		}
		rec := nameRecordOf(nameFromKey(k), acc)
		if !rec.heldAt(height) {
			continue
		}
		out = append(out, rec)
	}
	sortNameRecords(out)
	return out
}

// NameStateKey is the state key a light client proves a name against, so
// `/stateproof` can answer for a name exactly as it does for an address.
func NameStateKey(name string) string { return nameStateKey(name) }

func sortNameRecords(rs []NameRecord) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].Name < rs[j-1].Name; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}
