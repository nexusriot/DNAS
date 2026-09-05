package main

import (
	"errors"
	"fmt"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// Fee-bumping and cancelling a stuck payment.
//
// Replace-by-fee has been in consensus and in the mempool from early on: a
// transaction at the same (sender, nonce) paying a strictly higher fee replaces
// the one queued. Nothing could USE it. Not the light wallet, not the TUI, not
// the node's REPL — the only way to bump a payment was to hand-build a
// transaction at the right nonce, which is exactly the situation a wallet exists
// to avoid.
//
// Two operations cover it:
//
//   - BUMP re-sends the same transfer at a higher fee. Same recipient, same
//     amount, same nonce; only the fee moves. It is what you want when a payment
//     is right but priced too low to be mined.
//   - CANCEL replaces it with a payment to YOURSELF at the same nonce, which
//     consumes the nonce and voids the original. There is no "delete a
//     transaction" operation in an account ledger — the nonce is the slot, and
//     the only way to take it back is to fill it with something else.
//
// Neither is a guarantee. If the original has already been mined there is
// nothing to replace, and both refuse rather than sending a second payment on
// top of the first — which is the failure mode that makes a naive "just re-send
// it" bump dangerous.

// bumpTarget is the pending transaction a bump or cancel will replace, once it
// has been confirmed to be replaceable.
type bumpTarget struct {
	Tx     core.Transaction
	Status string
}

// findReplaceable looks a transaction up and reports whether it can still be
// replaced. A confirmed transaction cannot: its nonce is spent, and re-sending
// at that nonce would be rejected, while re-sending at the NEXT nonce would pay
// the recipient twice.
func findReplaceable(base, txHash string) (bumpTarget, error) {
	var res struct {
		Status string           `json:"status"`
		Tx     core.Transaction `json:"tx"`
		Height uint64           `json:"height"`
	}
	if err := getJSON(base+"/tx/"+txHash, &res); err != nil {
		return bumpTarget{}, fmt.Errorf("look up %s: %w", short(txHash), err)
	}
	switch res.Status {
	case "pending":
		return bumpTarget{Tx: res.Tx, Status: res.Status}, nil
	case "confirmed":
		return bumpTarget{}, fmt.Errorf(
			"%s is already confirmed in block %d — there is nothing left to replace",
			short(txHash), res.Height)
	default:
		return bumpTarget{}, fmt.Errorf("this node has never seen %s", short(txHash))
	}
}

// buildBump re-signs a pending transfer with a higher fee, keeping its nonce so
// it replaces rather than follows. Pure, so it is unit-tested directly.
func buildBump(w *wallet.Wallet, old core.Transaction, newFee uint64) (core.Transaction, error) {
	if old.From != w.Address() {
		return core.Transaction{}, fmt.Errorf("this key holds %s but the transaction is from %s",
			short(w.Address()), short(old.From))
	}
	if newFee <= old.Fee {
		return core.Transaction{}, fmt.Errorf(
			"a replacement must pay strictly more: the queued transaction pays %s",
			core.FormatAmount(old.Fee))
	}
	// Everything the sender authorized is carried over, so this is the SAME
	// payment at a new price — not a new one.
	tx := core.Transaction{
		From:      old.From,
		To:        old.To,
		Amount:    old.Amount,
		Outputs:   old.Outputs,
		AssetID:   old.AssetID,
		Fee:       newFee,
		Nonce:     old.Nonce,
		Expiry:    old.Expiry,
		LockUntil: old.LockUntil,
		Memo:      old.Memo,
	}
	if old.Issue != nil {
		return core.Transaction{}, errors.New("an asset issuance cannot be bumped (its id is bound to its nonce)")
	}
	if err := tx.Sign(w); err != nil {
		return core.Transaction{}, err
	}
	return tx, nil
}

// buildCancel replaces a pending transfer with a self-payment at the same nonce,
// consuming the nonce so the original can never be mined. The amount is zero:
// the fee is the whole cost, and the coin never leaves the sender.
func buildCancel(w *wallet.Wallet, old core.Transaction, newFee uint64) (core.Transaction, error) {
	if old.From != w.Address() {
		return core.Transaction{}, fmt.Errorf("this key holds %s but the transaction is from %s",
			short(w.Address()), short(old.From))
	}
	if newFee <= old.Fee {
		return core.Transaction{}, fmt.Errorf(
			"a replacement must pay strictly more: the queued transaction pays %s",
			core.FormatAmount(old.Fee))
	}
	tx := core.Transaction{
		From:   old.From,
		To:     old.From, // to itself: the nonce is spent, the coin stays put
		Amount: 0,
		Fee:    newFee,
		Nonce:  old.Nonce,
		Memo:   "cancel",
	}
	if err := tx.Sign(w); err != nil {
		return core.Transaction{}, err
	}
	return tx, nil
}

// bumpFee is the fee a bump uses when the operator names none: enough above the
// original to clear the "strictly higher" rule with room for the relay floor
// having risen since.
func bumpFee(old uint64, perByte uint64) uint64 {
	suggested := old * 2
	if floor := perByte * 1000; suggested < floor {
		suggested = floor
	}
	if suggested <= old {
		suggested = old + 1
	}
	return suggested
}

// runWalletBump implements `dnas spv wallet -key F bump|cancel <txhash> [fee]`.
func (sw *SPVWallet) bumpOrCancel(base, keyFile string, args []string, cancel bool, save func()) {
	verb := "bump"
	if cancel {
		verb = "cancel"
	}
	if len(args) < 1 {
		fmt.Printf("usage: dnas spv -api URL wallet -key FILE %s <txhash> [fee]\n", verb)
		return
	}
	w, _, err := wallet.LoadOrCreateEncrypted(keyFile, walletPassphrase())
	if err != nil {
		fmt.Println("key error:", err)
		return
	}
	target, err := findReplaceable(base, args[0])
	if err != nil {
		fmt.Println(err)
		return
	}

	fee := bumpFee(target.Tx.Fee, feePerByte(base))
	if len(args) > 1 {
		if fee, err = core.ParseAmount(args[1]); err != nil {
			fmt.Println("bad fee:", err)
			return
		}
	}

	var tx core.Transaction
	if cancel {
		tx, err = buildCancel(w, target.Tx, fee)
	} else {
		tx, err = buildBump(w, target.Tx, fee)
	}
	if err != nil {
		fmt.Println(err)
		return
	}
	if err := postJSON(base+"/tx", tx); err != nil {
		fmt.Println("submit:", err)
		return
	}
	sw.recordSent(w.Address(), tx.Nonce)
	save()
	if cancel {
		fmt.Printf("cancelled %s: nonce %d now spends %s to itself (fee %s)\n",
			short(args[0]), tx.Nonce, short(w.Address()), core.FormatAmount(fee))
	} else {
		fmt.Printf("bumped %s: same payment at nonce %d, fee %s -> %s\n",
			short(args[0]), tx.Nonce, core.FormatAmount(target.Tx.Fee), core.FormatAmount(fee))
	}
	fmt.Printf("new transaction: %s\n", tx.Hash())
}
