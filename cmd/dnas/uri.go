package main

import (
	"fmt"
	"strings"

	"github.com/nexusriot/DNAS/core"
)

// Paying a pasted `dnas:` URI.
//
// `dnas invoice new` prints one, and until now nothing read one back: a payer
// handed a URI still had to pick the address and the amount out of it by eye and
// retype both. Retyping the address is the dangerous half — consensus does not
// check recipient checksums (see the ROADMAP), so a typo that happens to keep
// the length is a burn — and it is the half a URI exists to remove.
//
// The expansion happens at the argument level rather than inside send() so that
// exactly one code path signs a payment, whether the recipient arrived as a URI
// or as an address and an amount typed separately.

// expandPaymentURI rewrites a send's positional arguments when the recipient is
// a payment URI. args is [to, amount?, fee?] as typed; the result is the same
// shape with the URI replaced by its address and any amount it carried filled
// in. A memo on the URI is adopted unless the caller passed one of their own.
//
// An amount given BOTH in the URI and on the command line must agree. Silently
// preferring one would mean a payer who mistyped the amount pays the other one
// and believes they paid theirs — and for an invoice, paying the wrong amount is
// the same as not paying at all, because that is what the payee matches on.
func expandPaymentURI(args []string, opts sendOptions) ([]string, sendOptions, error) {
	if len(args) == 0 || !core.IsPaymentURI(args[0]) {
		return args, opts, nil
	}
	uri, err := core.ParsePaymentURI(args[0])
	if err != nil {
		return nil, opts, err
	}

	out := append([]string(nil), args...)
	out[0] = uri.Address

	switch {
	case uri.Amount == 0:
		// The URI names only a payee, so an amount must have been typed.
		if len(out) < 2 {
			return nil, opts, fmt.Errorf("this URI names no amount, so give one: send %s <amount>", uri.Address)
		}
	case len(out) < 2:
		// The usual case: the URI carries the amount, so the payer typed none.
		out = append(out, formatAmountPlain(uri.Amount))
	default:
		typed, err := core.ParseAmount(out[1])
		if err != nil {
			return nil, opts, fmt.Errorf("bad amount %q: %w", out[1], err)
		}
		if typed != uri.Amount {
			return nil, opts, fmt.Errorf(
				"this URI asks for %s but the command says %s; pay the requested amount or drop the argument",
				core.FormatAmount(uri.Amount), core.FormatAmount(typed))
		}
	}

	if uri.Memo != "" && opts.Memo == "" {
		opts.Memo = uri.Memo
	}
	return out, opts, nil
}

// formatAmountPlain renders base units as the decimal DNAS string the send path
// parses back, without the ticker suffix FormatAmount appends.
func formatAmountPlain(units uint64) string {
	return strings.TrimSuffix(core.FormatAmount(units), " "+core.Ticker)
}

// describePaymentURI is the one-line summary shown before a URI is paid, so the
// payer confirms against what the URI actually says rather than against the
// opaque string they pasted.
func describePaymentURI(uri core.PaymentURI) string {
	var b strings.Builder
	if uri.Amount > 0 {
		fmt.Fprintf(&b, "pay %s to %s", core.FormatAmount(uri.Amount), uri.Address)
	} else {
		fmt.Fprintf(&b, "pay %s (amount not specified)", uri.Address)
	}
	if uri.Memo != "" {
		fmt.Fprintf(&b, "\n  for %q", uri.Memo)
	}
	if uri.Reference != "" {
		fmt.Fprintf(&b, "\n  payee reference %s (not carried on the chain)", uri.Reference)
	}
	return b.String()
}
