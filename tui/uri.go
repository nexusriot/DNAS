package main

import (
	"fmt"
	"net/url"
	"strings"
)

// Pasting a `dnas:` payment URI into the send prompt.
//
// A URI is exactly what someone is handed when they are asked to pay, and the
// send prompt is where they would paste it — so it accepts one in place of
// "<to> <amount>". The URI carries the amount and the memo, so pasting it is
// also the version that cannot mistype either.
//
// This parser is deliberately its own copy rather than an import of
// core.ParsePaymentURI: the TUI speaks only the HTTP API and imports no DNAS
// package (see the repo README), which is what lets it build as a standalone
// module. The format is four fields of text and a round-trip test pins it, so
// the duplication is cheap; the amount is still handed to the CLI as WRITTEN
// for a self-custodial send, so no second amount parser ever reaches consensus.

// paymentURI is a parsed `dnas:` payment request.
type paymentURI struct {
	address string
	amount  string // decimal DNAS exactly as written, or "" if unspecified
	memo    string
}

// isPaymentURI reports whether s carries the dnas: scheme.
func isPaymentURI(s string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "dnas:")
}

// parsePaymentURI reads "dnas:ADDRESS?amount=…&memo=…". The address is returned
// as-is: the node validates its checksum on the way in, and the TUI has no
// wallet package to check it against.
func parsePaymentURI(s string) (paymentURI, error) {
	s = strings.TrimSpace(s)
	if !isPaymentURI(s) {
		return paymentURI{}, fmt.Errorf("not a dnas: URI")
	}
	rest := strings.TrimPrefix(s[len("dnas:"):], "//")
	addr, query, _ := strings.Cut(rest, "?")
	addr, err := url.PathUnescape(strings.TrimSpace(addr))
	if err != nil {
		return paymentURI{}, fmt.Errorf("malformed address in URI")
	}
	if addr == "" {
		return paymentURI{}, fmt.Errorf("URI has no address")
	}
	out := paymentURI{address: addr}
	if query == "" {
		return out, nil
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		return paymentURI{}, fmt.Errorf("malformed URI parameters")
	}
	out.amount = strings.TrimSpace(q.Get("amount"))
	out.memo = q.Get("memo")
	return out, nil
}

// expandSendInput rewrites the send prompt's fields when the first one is a
// payment URI, so the rest of the send path sees the "<to> <amount> [fee]" it
// already understands. A memo carried by the URI is returned alongside.
//
// An amount typed after a URI that already names one is refused rather than
// silently overriding it: the payee matches on the amount, so paying a
// different one is the same as not paying.
func expandSendInput(fields []string) (out []string, memo string, err error) {
	if len(fields) == 0 || !isPaymentURI(fields[0]) {
		return fields, "", nil
	}
	uri, err := parsePaymentURI(fields[0])
	if err != nil {
		return nil, "", err
	}
	out = append([]string{uri.address}, fields[1:]...)
	switch {
	case uri.amount == "":
		if len(out) < 2 {
			return nil, "", fmt.Errorf("this URI names no amount; type one after it")
		}
	case len(out) < 2:
		out = append(out, uri.amount)
	default:
		// Both given: they have to agree. Compare as written first, then
		// numerically, so "2.5" and "2.50" are not treated as a conflict.
		if out[1] != uri.amount {
			typed, errT := parseDNAS(out[1])
			asked, errA := parseDNAS(uri.amount)
			if errT != nil || errA != nil || typed != asked {
				return nil, "", fmt.Errorf("this URI asks for %s, not %s", uri.amount, out[1])
			}
		}
	}
	// Put the URI's amount back in canonical form so both send paths agree on
	// what was requested.
	if uri.amount != "" {
		out[1] = uri.amount
	}
	return out, uri.memo, nil
}
