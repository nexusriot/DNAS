package main

import (
	"os"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// channelPair builds both sides of a channel from the same two keys, which is
// what the protocol does: each party derives the same 2-of-2 independently.
func channelPair(t *testing.T) (funder, receiver *wallet.Wallet, fc, rc *channelFile) {
	t.Helper()
	funder, _ = wallet.New()
	receiver, _ = wallet.New()

	const capacity = 10 * core.Coin
	const fee = core.Coin / 1000
	const expiry = 1000

	var err error
	fc, err = newChannel(roleFunder, funder, nil, receiver.PublicKeyHex(), capacity, fee, expiry)
	if err != nil {
		t.Fatal(err)
	}
	rc, err = newChannel(roleReceiver, receiver, nil, funder.PublicKeyHex(), capacity, fee, expiry)
	if err != nil {
		t.Fatal(err)
	}
	return funder, receiver, fc, rc
}

// Both parties must derive the SAME channel from the same keys, or they are not
// talking about the same account.
func TestBothPartiesDeriveTheSameChannel(t *testing.T) {
	_, _, fc, rc := channelPair(t)

	if fc.Address != rc.Address {
		t.Fatalf("funder sees %s, receiver sees %s", fc.Address, rc.Address)
	}
	if fc.Funder != rc.Funder || fc.Receiver != rc.Receiver {
		t.Fatalf("the two sides disagree about who is who:\n funder file: %s -> %s\n receiver file: %s -> %s",
			fc.Funder, fc.Receiver, rc.Funder, rc.Receiver)
	}
	if fc.Role == rc.Role {
		t.Error("both files claim the same role")
	}
	// The script's key order must match too, or the two produce different
	// signing bytes for the same logical transaction.
	if strings.Join(fc.PubKeys, ",") != strings.Join(rc.PubKeys, ",") {
		t.Errorf("scripts differ: %v vs %v", fc.PubKeys, rc.PubKeys)
	}
}

func TestChannelRefusesASelfChannel(t *testing.T) {
	w, _ := wallet.New()
	if _, err := newChannel(roleFunder, w, nil, w.PublicKeyHex(), core.Coin, 1, 100); err == nil {
		t.Fatal("a channel with itself was allowed")
	}
	if _, err := newChannel(roleFunder, w, nil, "not-a-key", core.Coin, 1, 100); err == nil {
		t.Fatal("a channel with an unparseable key was allowed")
	}
	other, _ := wallet.New()
	if _, err := newChannel(roleFunder, w, nil, other.PublicKeyHex(), 10, 10, 100); err == nil {
		t.Fatal("a channel whose fee equals its capacity was allowed")
	}
}

// The single rule that cannot be relaxed: a funder who broadcasts before holding
// a countersigned refund has handed the receiver a veto over their own money.
func TestFundingIsRefusedWithoutACountersignedRefund(t *testing.T) {
	funder, receiver, fc, _ := channelPair(t)

	// Freshly opened: the funder has signed their own half and nothing more.
	refund := fc.buildRefund()
	if err := signChannelTx(&refund, funder); err != nil {
		t.Fatal(err)
	}
	fc.Refund = &refund
	if fc.refundArmed() {
		t.Fatal("a refund with only the funder's signature was reported as armed")
	}
	if err := checkRefund(fc, refund); err == nil {
		t.Fatal("a refund the receiver never signed passed verification")
	}

	// Once the receiver countersigns it, it is usable.
	if err := signChannelTx(&refund, receiver); err != nil {
		t.Fatal(err)
	}
	if err := checkRefund(fc, refund); err != nil {
		t.Fatalf("a properly countersigned refund was refused: %v", err)
	}
	fc.Refund = &refund
	if !fc.refundArmed() {
		t.Fatal("a countersigned refund was not reported as armed")
	}
}

// A refund that returns the money somewhere else, or unlocks at a height the
// funder did not agree to, is worse than no refund at all — it looks like safety.
func TestRefundVerificationCatchesATamperedRefund(t *testing.T) {
	funder, receiver, fc, _ := channelPair(t)
	thief, _ := wallet.New()

	sign := func(tx *core.Transaction) {
		t.Helper()
		if err := signChannelTx(tx, funder); err != nil {
			t.Fatal(err)
		}
		if err := signChannelTx(tx, receiver); err != nil {
			t.Fatal(err)
		}
	}

	cases := map[string]func(*core.Transaction){
		"pays someone else": func(tx *core.Transaction) { tx.Outputs[0].To = thief.Address() },
		"wrong amount":      func(tx *core.Transaction) { tx.Outputs[0].Amount = 1 },
		"no time lock":      func(tx *core.Transaction) { tx.LockUntil = 0 },
		"later unlock":      func(tx *core.Transaction) { tx.LockUntil = fc.Expiry + 5000 },
		"different nonce":   func(tx *core.Transaction) { tx.Nonce = 7 },
		"extra recipient": func(tx *core.Transaction) {
			tx.Outputs = append(tx.Outputs, core.Output{To: thief.Address(), Amount: 1})
		},
	}
	for name, mutate := range cases {
		tx := fc.buildRefund()
		mutate(&tx)
		sign(&tx)
		if err := checkRefund(fc, tx); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The security property of a unidirectional channel: only the receiver can
// complete a settlement, so the funder cannot claw a payment back by publishing
// an old one — they cannot publish any of them.
func TestOnlyTheReceiverCanCompleteASettlement(t *testing.T) {
	funder, receiver, fc, _ := channelPair(t)

	tx, err := fc.buildSettlement(3 * core.Coin)
	if err != nil {
		t.Fatal(err)
	}
	if err := signChannelTx(&tx, funder); err != nil {
		t.Fatal(err)
	}
	if len(tx.Signatures) != 1 {
		t.Fatalf("a funder-signed settlement carries %d signatures", len(tx.Signatures))
	}
	// One of two is not enough for a 2-of-2, so the funder holds something they
	// cannot broadcast.
	if err := tx.VerifySignature(); err == nil {
		t.Fatal("a half-signed 2-of-2 spend verified")
	}
	if err := signChannelTx(&tx, receiver); err != nil {
		t.Fatal(err)
	}
	if err := tx.VerifySignature(); err != nil {
		t.Fatalf("a fully-signed settlement did not verify: %v", err)
	}
}

// Every transaction that can spend the channel uses the same nonce. That is what
// makes an old settlement harmless: at most one of them can ever confirm.
func TestEveryChannelTransactionSharesOneNonce(t *testing.T) {
	_, _, fc, _ := channelPair(t)

	txs := []core.Transaction{fc.buildRefund()}
	for _, paid := range []uint64{0, core.Coin, 5 * core.Coin, fc.spendable()} {
		tx, err := fc.buildSettlement(paid)
		if err != nil {
			t.Fatal(err)
		}
		txs = append(txs, tx)
	}
	for i, tx := range txs {
		if tx.Nonce != channelNonce {
			t.Errorf("transaction %d uses nonce %d, want %d", i, tx.Nonce, channelNonce)
		}
		if tx.From != fc.Address {
			t.Errorf("transaction %d spends %s, not the channel", i, tx.From)
		}
	}
}

// A settlement must distribute exactly what the channel holds, or coin is either
// invented or stranded.
func TestSettlementsDistributeTheWholeChannel(t *testing.T) {
	_, _, fc, _ := channelPair(t)

	for _, paid := range []uint64{0, 1, core.Coin, fc.spendable() - 1, fc.spendable()} {
		tx, err := fc.buildSettlement(paid)
		if err != nil {
			t.Fatalf("paid=%d: %v", paid, err)
		}
		var total, toReceiver uint64
		for _, o := range tx.Outputs {
			total += o.Amount
			if o.Amount == 0 {
				t.Errorf("paid=%d: a zero-amount output, which consensus would reject", paid)
			}
			if o.To == fc.Receiver {
				toReceiver += o.Amount
			}
		}
		if total != fc.spendable() {
			t.Errorf("paid=%d: distributes %d, the channel holds %d", paid, total, fc.spendable())
		}
		if toReceiver != paid {
			t.Errorf("paid=%d: the receiver gets %d", paid, toReceiver)
		}
	}
	if _, err := fc.buildSettlement(fc.spendable() + 1); err == nil {
		t.Error("a settlement paying more than the channel holds was built")
	}
}

// What the receiver checks before believing a payment. Each of these is a way the
// funder could otherwise hand over something that looks like money.
func TestReceiverRejectsABadSettlement(t *testing.T) {
	funder, _, fc, rc := channelPair(t)
	thief, _ := wallet.New()

	good, err := fc.buildSettlement(2 * core.Coin)
	if err != nil {
		t.Fatal(err)
	}
	if err := signChannelTx(&good, funder); err != nil {
		t.Fatal(err)
	}
	if err := checkSettlement(rc, good, 2*core.Coin); err != nil {
		t.Fatalf("a valid settlement was refused: %v", err)
	}

	cases := map[string]func(*core.Transaction){
		"pays a third party": func(tx *core.Transaction) { tx.Outputs[0].To = thief.Address() },
		"time-locked":        func(tx *core.Transaction) { tx.LockUntil = 5000 },
		"wrong nonce":        func(tx *core.Transaction) { tx.Nonce = 9 },
		"wrong fee":          func(tx *core.Transaction) { tx.Fee = 1 },
		"short-changes":      func(tx *core.Transaction) { tx.Outputs[0].Amount /= 2 },
		"not the channel":    func(tx *core.Transaction) { tx.From = thief.Address() },
		"no script":          func(tx *core.Transaction) { tx.Multisig = nil },
	}
	for name, mutate := range cases {
		tx := good
		tx.Outputs = append([]core.Output(nil), good.Outputs...)
		mutate(&tx)
		if err := checkSettlement(rc, tx, 2*core.Coin); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// An unsigned settlement is a promise with nothing behind it.
	unsigned, _ := fc.buildSettlement(2 * core.Coin)
	if err := checkSettlement(rc, unsigned, 2*core.Coin); err == nil {
		t.Error("an unsigned settlement was accepted")
	}
}

// A channel only moves one way. A settlement paying less than the one already
// held is not a payment, and accepting it would throw money away.
func TestReceiverRefusesToGoBackwards(t *testing.T) {
	funder, _, fc, rc := channelPair(t)

	first, _ := fc.buildSettlement(5 * core.Coin)
	if err := signChannelTx(&first, funder); err != nil {
		t.Fatal(err)
	}
	if err := checkSettlement(rc, first, 5*core.Coin); err != nil {
		t.Fatal(err)
	}
	rc.Paid, rc.Settlement = 5*core.Coin, &first

	lower, _ := fc.buildSettlement(2 * core.Coin)
	if err := signChannelTx(&lower, funder); err != nil {
		t.Fatal(err)
	}
	err := checkSettlement(rc, lower, 2*core.Coin)
	if err == nil {
		t.Fatal("a settlement paying less than the running total was accepted")
	}
	if !strings.Contains(err.Error(), "one way") {
		t.Errorf("unhelpful error: %v", err)
	}

	// The same amount again is fine — it is a resend, not a reduction.
	same, _ := fc.buildSettlement(5 * core.Coin)
	if err := signChannelTx(&same, funder); err != nil {
		t.Fatal(err)
	}
	if err := checkSettlement(rc, same, 5*core.Coin); err != nil {
		t.Errorf("a repeat of the current settlement was refused: %v", err)
	}
}

// The whole point: many payments, one transaction on the chain.
func TestManyPaymentsProduceOneChainTransaction(t *testing.T) {
	funder, receiver, fc, rc := channelPair(t)
	fc.Funded = "funded"

	var latest core.Transaction
	running := uint64(0)
	for i := 0; i < 50; i++ {
		running += core.Coin / 10
		tx, err := fc.buildSettlement(running)
		if err != nil {
			t.Fatal(err)
		}
		if err := signChannelTx(&tx, funder); err != nil {
			t.Fatal(err)
		}
		if err := checkSettlement(rc, tx, running); err != nil {
			t.Fatalf("payment %d refused: %v", i, err)
		}
		rc.Paid, rc.Settlement = running, &tx
		latest = tx
	}

	// Only the last one is ever broadcast, and it needs one more signature.
	if err := signChannelTx(&latest, receiver); err != nil {
		t.Fatal(err)
	}
	if err := latest.VerifySignature(); err != nil {
		t.Fatalf("the closing transaction did not verify: %v", err)
	}
	if err := core.CheckTxSanity(latest); err != nil {
		t.Fatalf("the closing transaction is malformed: %v", err)
	}
	var toReceiver uint64
	for _, o := range latest.Outputs {
		if o.To == rc.Receiver {
			toReceiver = o.Amount
		}
	}
	if toReceiver != running {
		t.Fatalf("the close pays %s after 50 payments totalling %s",
			core.FormatAmount(toReceiver), core.FormatAmount(running))
	}
}

// The channel file is an offline-signed artefact, so it must carry the network
// it is for — a signature made on one chain means nothing on another.
func TestChannelFileRoundTripsWithItsNetwork(t *testing.T) {
	_, _, fc, _ := channelPair(t)
	path := t.TempDir() + "/channel.json"

	if err := writeChannel(path, fc); err != nil {
		t.Fatal(err)
	}
	back, err := readChannel(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if back.Network != core.NetworkName() {
		t.Errorf("network = %q, want %q", back.Network, core.NetworkName())
	}
	if back.Address != fc.Address || back.Capacity != fc.Capacity || back.Expiry != fc.Expiry {
		t.Errorf("round trip changed the channel:\n got %+v\nwant %+v", back, fc)
	}
	if back.Funder != fc.Funder || back.Receiver != fc.Receiver {
		t.Error("round trip changed who is who")
	}
}

func TestChannelFileRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"not json":      "{",
		"empty object":  "{}",
		"wrong version": `{"version": 99, "address": "dnasx", "pubkeys": ["a","b"]}`,
		"no address":    `{"version": 1, "pubkeys": ["a","b"]}`,
		"one key":       `{"version": 1, "address": "dnasx", "pubkeys": ["a"]}`,
	} {
		path := dir + "/" + strings.ReplaceAll(name, " ", "-") + ".json"
		if err := writeFileString(path, body); err != nil {
			t.Fatal(err)
		}
		if _, err := readChannel(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// spendable is what every amount in the protocol is measured against, so an
// off-by-one here would strand or invent coin in every settlement.
func TestSpendableReservesTheClosingFee(t *testing.T) {
	c := &channelFile{Capacity: 100, Fee: 7}
	if got := c.spendable(); got != 93 {
		t.Errorf("spendable = %d, want 93", got)
	}
	// A fee larger than the capacity cannot go negative.
	broke := &channelFile{Capacity: 5, Fee: 10}
	if got := broke.spendable(); got != 0 {
		t.Errorf("spendable = %d on an underfunded channel, want 0", got)
	}
}

func TestIsAlreadySignedRecognizesTheHarmlessCase(t *testing.T) {
	_, _, fc, _ := channelPair(t)
	w, _ := wallet.New()
	tx := fc.buildRefund()

	// A key that is not a member is a real error, not the harmless one.
	err := signChannelTx(&tx, w)
	if err == nil {
		t.Fatal("a non-member signed")
	}
	if isAlreadySigned(err) {
		t.Errorf("a non-member error was treated as harmless: %v", err)
	}
}

func writeFileString(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
