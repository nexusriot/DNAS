package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// receiver is a fake webhook: it records what it is sent and can be told to
// fail, which is how the retry behaviour is tested.
type receiver struct {
	mu     sync.Mutex
	events []invoiceEvent
	fail   bool
	srv    *httptest.Server
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var ev invoiceEvent
		_ = json.NewDecoder(req.Body).Decode(&ev)
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.fail {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		r.events = append(r.events, ev)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) setFailing(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = v
}

func (r *receiver) got() []invoiceEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]invoiceEvent(nil), r.events...)
}

// invoiceDir writes one invoice into a fresh directory and returns both.
func invoiceDir(t *testing.T, inv invoiceFile) string {
	t.Helper()
	dir := t.TempDir()
	if err := writeInvoice(filepath.Join(dir, inv.Reference+".json"), inv); err != nil {
		t.Fatal(err)
	}
	return dir
}

// serveTestInvoice is a well-formed invoice. The address is a real one because
// readInvoice validates it — an invoice naming an address nobody can be paid at
// is exactly what that check exists to refuse.
func serveTestInvoice() invoiceFile {
	return invoiceFile{
		Version: invoiceFileVersion, Network: core.NetworkName(),
		Address: serveTestAddress, Amount: 5 * core.Coin, Reference: "ref1", FromHeight: 1,
	}
}

var serveTestAddress = func() string {
	w, err := wallet.New()
	if err != nil {
		panic(err)
	}
	return w.Address()
}()

// paid is a scanner that always reports the given payment.
func paid(p invoicePayment, tip uint64) invoiceScanner {
	return func(invoiceFile) (invoicePayment, uint64, error) { return p, tip, nil }
}

func TestStatusReflectsWhatWasReceived(t *testing.T) {
	inv := serveTestInvoice()
	cases := []struct {
		name string
		p    invoicePayment
		want string
	}{
		{"nothing", invoicePayment{}, invoiceUnpaid},
		{"some", invoicePayment{Received: core.Coin}, invoicePartial},
		{"all but not deep enough", invoicePayment{Received: 5 * core.Coin}, invoicePending},
		{"settled", invoicePayment{Received: 5 * core.Coin, Settled: 5 * core.Coin}, invoiceSettled},
		{"overpaid", invoicePayment{Received: 9 * core.Coin, Settled: 9 * core.Coin}, invoiceSettled},
		{"expired", invoicePayment{Expired: true}, invoiceExpired},
		// Settlement wins over expiry: money that arrived deep enough was paid,
		// whatever the invoice's deadline says.
		{"settled at the deadline", invoicePayment{Settled: 5 * core.Coin, Expired: true}, invoiceSettled},
	}
	for _, c := range cases {
		if got := statusOf(inv, c.p); got != c.want {
			t.Errorf("%s: status = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDaemonNotifiesOnceWhenAnInvoiceSettles(t *testing.T) {
	inv := serveTestInvoice()
	dir := invoiceDir(t, inv)
	rec := newReceiver(t)
	st := newInvoiceState(core.NetworkName())

	scan := paid(invoicePayment{Received: inv.Amount, Settled: inv.Amount, Confirmations: 3}, 42)
	if n := invoiceServePass(scan, dir, rec.srv.URL, st, rec.srv.Client()); n == 0 {
		t.Fatal("a settlement reported no change")
	}
	events := rec.got()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Event != "invoice.settled" || ev.Reference != "ref1" {
		t.Errorf("event = %+v", ev)
	}
	if ev.Settled != inv.Amount || ev.Height != 42 {
		t.Errorf("event carries settled=%d height=%d, want %d/42", ev.Settled, ev.Height, inv.Amount)
	}

	// A second pass over the same settled invoice must not notify again.
	invoiceServePass(scan, dir, rec.srv.URL, st, rec.srv.Client())
	invoiceServePass(scan, dir, rec.srv.URL, st, rec.srv.Client())
	if got := len(rec.got()); got != 1 {
		t.Fatalf("the same settlement was reported %d times", got)
	}
}

// The property this daemon exists for: a webhook that was down does not cost a
// payment. Delivery is retried on every pass until the receiver acknowledges.
func TestDaemonRetriesUntilTheWebhookAcknowledges(t *testing.T) {
	inv := serveTestInvoice()
	dir := invoiceDir(t, inv)
	rec := newReceiver(t)
	rec.setFailing(true)
	st := newInvoiceState(core.NetworkName())
	scan := paid(invoicePayment{Received: inv.Amount, Settled: inv.Amount}, 10)

	for i := 0; i < 3; i++ {
		invoiceServePass(scan, dir, rec.srv.URL, st, rec.srv.Client())
	}
	if got := len(rec.got()); got != 0 {
		t.Fatalf("a failing receiver recorded %d events", got)
	}
	if st.Records["ref1"].Notified {
		t.Fatal("marked as notified although the receiver never acknowledged it")
	}

	rec.setFailing(false)
	invoiceServePass(scan, dir, rec.srv.URL, st, rec.srv.Client())
	if got := len(rec.got()); got != 1 {
		t.Fatalf("after the receiver came back, got %d events, want 1", got)
	}
	if !st.Records["ref1"].Notified {
		t.Error("an acknowledged delivery was not recorded")
	}
}

// And the memory survives the process: a restart must not re-report a payment
// the previous run already delivered.
func TestDaemonDoesNotRepeatAfterARestart(t *testing.T) {
	inv := serveTestInvoice()
	dir := invoiceDir(t, inv)
	rec := newReceiver(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	scan := paid(invoicePayment{Received: inv.Amount, Settled: inv.Amount}, 7)

	st := newInvoiceState(core.NetworkName())
	invoiceServePass(scan, dir, rec.srv.URL, st, rec.srv.Client())
	if err := st.save(statePath); err != nil {
		t.Fatal(err)
	}
	if len(rec.got()) != 1 {
		t.Fatalf("first run delivered %d events", len(rec.got()))
	}

	// A brand-new daemon, reading the file the old one left.
	restarted, err := loadInvoiceState(statePath, core.NetworkName())
	if err != nil {
		t.Fatal(err)
	}
	invoiceServePass(scan, dir, rec.srv.URL, restarted, rec.srv.Client())
	if got := len(rec.got()); got != 1 {
		t.Fatalf("a restart re-reported the payment (%d events total)", got)
	}
}

// A reorg can undo a settlement. The invoice then has to become notifiable
// again, or a payment that came back would be silently swallowed.
func TestDaemonCanReportASettlementAgainIfItIsUndone(t *testing.T) {
	inv := serveTestInvoice()
	dir := invoiceDir(t, inv)
	rec := newReceiver(t)
	st := newInvoiceState(core.NetworkName())

	settled := paid(invoicePayment{Received: inv.Amount, Settled: inv.Amount}, 20)
	invoiceServePass(settled, dir, rec.srv.URL, st, rec.srv.Client())
	if len(rec.got()) != 1 {
		t.Fatalf("first settlement delivered %d events", len(rec.got()))
	}

	// The reorg: the payment is back to merely received.
	undone := paid(invoicePayment{Received: inv.Amount}, 19)
	invoiceServePass(undone, dir, rec.srv.URL, st, rec.srv.Client())
	if st.Records["ref1"].Status != invoicePending {
		t.Fatalf("status after the reorg = %q, want %q", st.Records["ref1"].Status, invoicePending)
	}
	if st.Records["ref1"].Notified {
		t.Fatal("still marked notified after the settlement was undone")
	}

	// It settles again, and is reported again.
	invoiceServePass(settled, dir, rec.srv.URL, st, rec.srv.Client())
	if got := len(rec.got()); got != 2 {
		t.Fatalf("re-settlement produced %d events in total, want 2", got)
	}
}

func TestDaemonReportsExpiry(t *testing.T) {
	inv := serveTestInvoice()
	inv.ExpiresHeight = 30
	dir := invoiceDir(t, inv)
	rec := newReceiver(t)
	st := newInvoiceState(core.NetworkName())

	invoiceServePass(paid(invoicePayment{Expired: true}, 31), dir, rec.srv.URL, st, rec.srv.Client())
	events := rec.got()
	if len(events) != 1 || events[0].Event != "invoice.expired" {
		t.Fatalf("got %+v, want one invoice.expired", events)
	}
}

// An invoice for a different network must never be matched against this chain:
// reporting a testnet payment as a mainnet one is reporting money that does not
// exist.
func TestDaemonSkipsInvoicesForAnotherNetwork(t *testing.T) {
	inv := serveTestInvoice()
	inv.Network = "some-other-network"
	dir := invoiceDir(t, inv)
	rec := newReceiver(t)
	st := newInvoiceState(core.NetworkName())

	invoiceServePass(paid(invoicePayment{Settled: inv.Amount}, 5), dir, rec.srv.URL, st, rec.srv.Client())
	if got := len(rec.got()); got != 0 {
		t.Fatalf("an invoice for another network produced %d events", got)
	}
	if _, tracked := st.Records["ref1"]; tracked {
		t.Error("an invoice for another network was recorded")
	}
}

func TestStateRefusesAFileForAnotherNetwork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := newInvoiceState("testnet")
	if err := st.save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInvoiceState(path, "mainnet"); err == nil {
		t.Fatal("testnet state was accepted on mainnet")
	}
	if _, err := loadInvoiceState(path, "testnet"); err != nil {
		t.Fatalf("its own network was refused: %v", err)
	}
	// A missing file is a first run, not a failure.
	fresh, err := loadInvoiceState(filepath.Join(t.TempDir(), "nope.json"), "testnet")
	if err != nil {
		t.Fatalf("a first run failed: %v", err)
	}
	if len(fresh.Records) != 0 {
		t.Error("a first run started with records")
	}
}

// An invoice directory in a real shop collects stray files; one of them must not
// stop the daemon.
func TestLoadInvoiceDirSkipsWhatIsNotAnInvoice(t *testing.T) {
	dir := t.TempDir()
	good := serveTestInvoice()
	if err := writeInvoice(filepath.Join(dir, "good.json"), good); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"notes.txt":      "shopping list",
		"broken.json":    "{not json",
		"state.json.tmp": "{}",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}

	invs, errs := loadInvoiceDir(dir)
	if len(invs) != 1 || invs[0].Reference != good.Reference {
		t.Fatalf("loaded %d invoices: %+v", len(invs), invs)
	}
	if len(errs) != 1 {
		t.Errorf("reported %d problems, want 1 (the malformed json)", len(errs))
	}
}

// Anything but a 2xx means the receiver has not recorded the payment, so it must
// be treated as a failure and retried.
func TestPostTreatsANonSuccessAsUndelivered(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusBadRequest, http.StatusFound} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		err := postInvoiceEvent(srv.Client(), srv.URL, invoiceEvent{Reference: "r"})
		srv.Close()
		if err == nil {
			t.Errorf("status %d was treated as delivered", code)
		}
	}
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ok.Close()
	if err := postInvoiceEvent(ok.Client(), ok.URL, invoiceEvent{Reference: "r"}); err != nil {
		t.Errorf("202 was treated as a failure: %v", err)
	}
}

// With no webhook configured the daemon still tracks state and must not log the
// same settlement on every pass forever.
func TestDaemonWithoutAWebhookStillSettlesOnce(t *testing.T) {
	inv := serveTestInvoice()
	dir := invoiceDir(t, inv)
	st := newInvoiceState(core.NetworkName())
	scan := paid(invoicePayment{Received: inv.Amount, Settled: inv.Amount}, 3)

	first := invoiceServePass(scan, dir, "", st, http.DefaultClient)
	if first == 0 {
		t.Fatal("the first settlement reported no change")
	}
	if second := invoiceServePass(scan, dir, "", st, http.DefaultClient); second != 0 {
		t.Fatalf("a settled invoice kept reporting changes (%d)", second)
	}
}
