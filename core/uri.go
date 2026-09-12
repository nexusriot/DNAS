package core

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/nexusriot/DNAS/wallet"
)

// Payment URIs.
//
// `dnas invoice new` has always PRINTED one of these — it is the string a payee
// hands over instead of dictating an address and an amount — but nothing read
// one back, so the format was write-only and every client still asked the payer
// to retype both fields. That is exactly the retyping the URI exists to remove,
// and the field most worth not retyping is the one a typo silently burns coin
// into.
//
// The shape is the one every other coin uses:
//
//	dnas:ADDRESS[?amount=DECIMAL&memo=TEXT&ref=TOKEN]
//
// Building and parsing live together so the two cannot drift: a round trip is a
// test, not a hope.

// URIScheme is the scheme of a DNAS payment URI, without the colon.
const URIScheme = "dnas"

// PaymentURI is a parsed payment request. Only Address is guaranteed present;
// the rest are what the payee chose to pin down.
type PaymentURI struct {
	Address   string // validated: checksum checked, so a typo cannot reach a signature
	Amount    uint64 // base units; 0 = the payer chooses
	Memo      string // what the payment is for; goes on the chain if the payer keeps it
	Reference string // the payee's own record id; NOT carried on the chain
}

// BuildPaymentURI renders a payment request. An empty memo, reference or zero
// amount is simply omitted, so the common "just pay this address" case is the
// bare string.
func BuildPaymentURI(address string, amount uint64, memo, reference string) string {
	q := url.Values{}
	if amount > 0 {
		// Decimal DNAS, not base units: a human reads this, and pasting
		// "250000000" when you meant 2.5 is the mistake the format should not
		// invite. TrimSuffix drops the ticker FormatAmount appends.
		q.Set("amount", strings.TrimSuffix(FormatAmount(amount), " "+Ticker))
	}
	if memo != "" {
		q.Set("memo", memo)
	}
	if reference != "" {
		q.Set("ref", reference)
	}
	uri := URIScheme + ":" + address
	if len(q) > 0 {
		uri += "?" + q.Encode()
	}
	return uri
}

// ParsePaymentURI reads a payment URI. A bare address is accepted too: a payer
// who was handed one rather than a URI should not have to care which they got,
// and every caller here would otherwise need the same two-branch check.
//
// The address is checksum-validated, so a URI that survives this cannot direct
// a payment at a mistyped address. An unknown query parameter is ignored rather
// than refused, so a future field does not break today's clients.
func ParsePaymentURI(s string) (PaymentURI, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return PaymentURI{}, errors.New("empty payment URI")
	}

	rest, hasScheme := cutScheme(s)
	if !hasScheme {
		// A bare address: no query to read, so validate and hand it back. Either
		// spelling is accepted and normalized, so a pasted bech32 address works
		// everywhere a canonical one does and consensus still sees one form.
		canonical, err := wallet.NormalizeAddress(s)
		if err != nil {
			return PaymentURI{}, fmt.Errorf("not a %s: URI or a valid address: %w", URIScheme, err)
		}
		return PaymentURI{Address: canonical}, nil
	}

	addr, query, _ := strings.Cut(rest, "?")
	// The address sits in the opaque part of the URI, so it can still be
	// percent-encoded by a client that escaped the whole string.
	addr, err := url.PathUnescape(addr)
	if err != nil {
		return PaymentURI{}, fmt.Errorf("malformed address in URI: %w", err)
	}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return PaymentURI{}, errors.New("payment URI has no address")
	}
	canonical, err := wallet.NormalizeAddress(addr)
	if err != nil {
		return PaymentURI{}, fmt.Errorf("payment URI address: %w", err)
	}
	out := PaymentURI{Address: canonical}

	if query == "" {
		return out, nil
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		return PaymentURI{}, fmt.Errorf("malformed payment URI parameters: %w", err)
	}
	if a := strings.TrimSpace(q.Get("amount")); a != "" {
		amount, err := ParseAmount(a)
		if err != nil {
			return PaymentURI{}, fmt.Errorf("payment URI amount %q: %w", a, err)
		}
		if amount == 0 {
			// An explicit zero is a request to pay nothing, which is never what
			// the payee meant and would otherwise submit a pointless transaction.
			return PaymentURI{}, fmt.Errorf("payment URI amount %q is zero", a)
		}
		out.Amount = amount
	}
	out.Memo = q.Get("memo")
	out.Reference = q.Get("ref")
	if len(out.Memo) > MaxMemoBytes {
		return PaymentURI{}, fmt.Errorf("payment URI memo is %d bytes, over the %d-byte limit",
			len(out.Memo), MaxMemoBytes)
	}
	return out, nil
}

// IsPaymentURI reports whether s looks like a payment URI, so a caller can tell
// "the user pasted a URI" from "the user typed an address" before parsing.
func IsPaymentURI(s string) bool {
	_, ok := cutScheme(strings.TrimSpace(s))
	return ok
}

// cutScheme strips a case-insensitive "dnas:" prefix, reporting whether one was
// there. Schemes are case-insensitive per RFC 3986, and a URI arriving from a
// browser or a QR reader may well be upper-cased.
func cutScheme(s string) (rest string, ok bool) {
	const prefix = URIScheme + ":"
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	// Tolerate the "dnas://addr" spelling as well as "dnas:addr": people write
	// both, and refusing one of them teaches nothing.
	return strings.TrimPrefix(s[len(prefix):], "//"), true
}
