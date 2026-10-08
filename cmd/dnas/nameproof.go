package main

import (
	"fmt"

	"github.com/nexusriot/DNAS/core"
)

// provenName resolves a name and PROVES the answer against a header whose
// proof of work this client has verified itself.
//
// The point of the name service being in consensus is that the resolution can
// be checked. A client that takes a node's word for which address a name
// points at has a phishing surface with extra steps, so this is the default
// path and `-trust` is the opt-out.
//
// Two checks matter and both are easy to leave out:
//
//   - The proof must be about the key WE derived from the name. A node that
//     answered with a valid proof about a different name would otherwise pass:
//     the proof is internally consistent, it is simply about something else.
//   - The proof's state root must belong to a header from the verified chain,
//     not merely be a root the proof folds to.
func provenName(base, name string) (core.Account, error) {
	headers, err := fetchHeaders(base)
	if err != nil {
		return core.Account{}, err
	}
	if _, _, err := verifyHeaderChain(headers); err != nil {
		return core.Account{}, fmt.Errorf("header chain invalid: %w", err)
	}

	var p core.AccountProof
	if err := getJSON(base+"/nameproof/"+name, &p); err != nil {
		return core.Account{}, err
	}
	want := core.NameStateKey(name)
	if p.Address != want {
		return core.Account{}, fmt.Errorf("the node proved a different name than the one asked for")
	}
	if p.BlockIndex >= uint64(len(headers)) || p.StateRoot != headers[p.BlockIndex].StateRoot {
		return core.Account{}, fmt.Errorf("the proof does not reference a verified header")
	}
	valid, present := core.VerifyAccountProof(p, headers[p.BlockIndex].StateRoot)
	if !valid {
		return core.Account{}, fmt.Errorf("the proof does not fold to a verified header")
	}
	if !present {
		// Proven ABSENT, which is a real answer: the name is unregistered, and
		// the chain says so rather than the node declining to find it.
		return core.Account{}, nil
	}
	return p.Account, nil
}
