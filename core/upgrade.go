package core

import "sync"

// Consensus upgrades let a rule change activate at a fixed block height, so the
// network adopts new rules on a coordinated flag-day instead of an uncoordinated
// hard fork. Activation heights are configuration (like checkpoints, §finality):
// set identically on every node at startup via SetUpgradeHeight; an unset upgrade
// is never active. A validation rule guards itself with IsUpgradeActive(name,
// blockHeight), so blocks below the activation height keep the old rule and blocks
// at/after it enforce the new one.
//
// Height activation is the mechanism every rule here ultimately runs on, but it
// is no longer the only way a height gets set. A rule may instead be put to a
// miner vote: a BIP9 deployment claims a bit of the block header's Version field,
// and once a window of blocks signals it the chain itself fixes the activation
// height and installs it here (see versionbits.go). Validation is unchanged
// either way — a rule asks IsUpgradeActive and never learns which route set the
// number.

// UpgradeDustLimit, once active, rejects coin transfers below DustThreshold — a
// worked example of a height-activated consensus rule.
const UpgradeDustLimit = "dustlimit"

// UpgradeMultiOutput, once active, allows multi-recipient transfers
// (Transaction.Outputs). Until then a transaction carrying outputs is rejected,
// so the whole network starts accepting them at the same height instead of some
// nodes treating a block as valid while others reject it. Because the outputs are
// encoded only when present (see codec.go), enabling the rule changes no existing
// transaction's id and no stored chain's validity.
const UpgradeMultiOutput = "multioutput"

// UpgradeVault, once active, allows spends authorized by a time-delayed vault
// script (Transaction.Vault). Until then a transaction carrying one is rejected,
// so no node accepts a block the rest of the network would refuse. Vault-
// authorized spends encode only when the script is present (see codec.go), so
// scheduling this changes no existing transaction id and no stored chain.
const UpgradeVault = "vault"

// UpgradeFeeSponsor, once active, allows a third party to pay a transaction's
// fee (Transaction.FeePayer). It is height-activated for the same reason as the
// others: the rule changes who must be able to afford the fee, so every node has
// to start applying it at the same block.
const UpgradeFeeSponsor = "feesponsor"

// UpgradeCheckedAddresses, once active, requires every address a transaction
// names — sender, recipient, each multi-output recipient, and the fee payer — to
// be a well-formed, checksummed DNAS address.
//
// Until this activates, consensus checks only that addresses are not absurdly
// long. Every client validates recipients before signing, but that is a
// convention, not a rule: a buggy or malicious client can put anything in `To`,
// and the coin lands on a state key nobody holds the private key for. Burned,
// permanently, with no way to tell it from a deliberate burn.
//
// It is height-activated like the others because it is a tightening: blocks
// below the activation height keep the old rule, so an existing chain still
// replays. What it CANNOT do is recover coin already sent to a malformed
// address — those state entries stay where they are.
const UpgradeCheckedAddresses = "checkedaddresses"

// UpgradeUniqueCoinbase, once active, requires a block's coinbase to carry the
// block's own height in its Nonce field (Bitcoin's BIP34).
//
// Without it a coinbase commits only (recipient, amount), so two blocks paying
// the same miner the same subsidy produce the SAME txid. The transaction index
// works around the duplication by resolving to the first occurrence, but the
// consequence is real: an inclusion proof for such a coinbase can only point at
// one of the blocks, and "which block paid this" has no answer.
//
// Binding the height makes every coinbase distinct, because no two blocks in a
// chain share a height. It is height-activated because it is a consensus change
// in both directions — below the activation a coinbase Nonce must be 0, at and
// above it must equal the height — so an existing chain replays unchanged.
const UpgradeUniqueCoinbase = "uniquecoinbase"

// UpgradeAssetOps, once active, allows an asset's issuer to mint more of it or
// to burn units it holds (Transaction.AssetOp).
//
// Until it activates, an asset's supply is fixed at issuance and nothing can
// change it. That is a real restriction rather than an oversight — a holder of a
// fixed-supply asset knows the issuer cannot dilute them — so turning it on is a
// change to what every holder is trusting, and it gets a flag day like the rest.
const UpgradeAssetOps = "assetops"

// UpgradeNonFungible, once active, allows an issuance to carry a metadata
// commitment (AssetIssue.Metadata), which makes the asset non-fungible: its
// supply is exactly one and no mint or burn may ever change it.
//
// Height-activated like the rest because it is a loosening that changes what a
// block may contain: until it activates, an issuance carrying metadata is
// rejected, so no node accepts a block the others would refuse. The field is
// encoded only when present (see codec.go), so scheduling it changes no
// existing transaction's id and no stored chain's validity.
//
// The uniqueness is enforced where it can be: supply one, and permanently
// fixed. What the token POINTS AT is a hash, and whether those bytes still
// exist anywhere is not something a chain can promise.
const UpgradeNonFungible = "nonfungible"

// UpgradeScript, once active, allows script-authorized spends
// (Transaction.Script): a small, bounded stack language for spending
// conditions, in place of the hard-coded multisig / HTLC / vault shapes.
//
// Height-activated like the rest, and for the strongest version of the usual
// reason: until it activates, the funds in a script address are spendable by
// nobody, so turning it on changes what a block may contain in a way every
// node must agree about at the same height. The field encodes only when
// present, so no existing transaction's id moves.
const UpgradeScript = "script"

// UpgradeNameService, once active, allows name-service operations
// (Transaction.Name): registering, renewing, transferring and releasing
// human-readable names that resolve to addresses.
//
// The records live in the account state, so they are committed by the block's
// state root and a light client can PROVE both that a name resolves to an
// address and that a name is unclaimed. That is the point of putting it in
// consensus: a name service you have to trust a server about is a phishing
// surface with extra steps.
//
// Height-activated like the rest. It is a loosening — a transaction carrying a
// name operation is rejected until the activation height — so every node
// starts accepting them at the same block, and an existing chain replays
// unchanged because the field encodes only when present.
const UpgradeNameService = "nameservice"

// UpgradeFilterCommit, once active, commits each block's compact-filter header
// in the block header itself (Header.FilterRoot), and makes the filter's
// SipHash key derive from the block's PrevHash and MerkleRoot rather than from
// its own hash.
//
// Those two halves are one change because the first is impossible without the
// second. A BIP158 filter is keyed by the hash of the block it describes; put
// the filter's commitment inside that same hash and the definition eats itself.
// Bitcoin resolves the circularity by not committing filters at all, which is
// why BIP157 non-inclusion rests on asking several peers and hoping. Re-keying
// on fields that are fixed BEFORE the nonce is searched breaks the cycle, and
// then the commitment is just another field in the proof-of-work preimage.
//
// What it buys: "this address appears in no block between X and Y" becomes a
// statement backed by the chain's own work, rather than by the honesty of the
// node that answered. That was the last trust assumption left in the light
// client (DESIGN §21).
//
// The committed chain starts AT the activation height — the parent of the first
// committed block has no FilterRoot to extend — so a client verifies filters
// from the activation height forward and falls back to the uncommitted
// filter-header chain below it.
const UpgradeFilterCommit = "filtercommit"

// UpgradeHistoryRoot, once active, commits a Merkle Mountain Range over every
// previous block hash in the header (Header.HistoryRoot).
//
// A header chain already proves ancestry, but only by holding all of it: to
// know that block 10 is an ancestor of block 1,000,000 you need the 999,990
// headers in between. An MMR is an append-only accumulator whose root commits
// to the whole history, so the same fact takes a proof logarithmic in the
// chain's length. See mmr.go.
//
// Like the filter commitment, the accumulator starts at the activation height:
// the range covers block hashes from the activation height forward, so a client
// anchored on a header at or above it can verify ancestry without the prefix.
const UpgradeHistoryRoot = "historyroot"

var (
	upgradesMu sync.RWMutex
	upgrades   = map[string]uint64{}
)

// knownUpgrades is every upgrade this build understands. An operator scheduling
// one by name is told immediately if it is misspelled, rather than running with a
// rule that silently never activates — which on a network where the others did
// activate means being forked off it.
var knownUpgrades = []string{UpgradeDustLimit, UpgradeMultiOutput, UpgradeVault,
	UpgradeFeeSponsor, UpgradeCheckedAddresses, UpgradeUniqueCoinbase, UpgradeAssetOps,
	UpgradeFilterCommit, UpgradeHistoryRoot, UpgradeNonFungible, UpgradeNameService, UpgradeScript}

// Upgrades lists the upgrade names this build understands.
func Upgrades() []string { return append([]string(nil), knownUpgrades...) }

// KnownUpgrade reports whether name is an upgrade this build understands.
func KnownUpgrade(name string) bool {
	for _, u := range knownUpgrades {
		if u == name {
			return true
		}
	}
	return false
}

// SetUpgradeHeight schedules an upgrade to activate at the given block height.
// Call it at startup, before syncing, with the same values on every node.
func SetUpgradeHeight(name string, height uint64) {
	upgradesMu.Lock()
	defer upgradesMu.Unlock()
	upgrades[name] = height
}

// ClearUpgradeHeight unschedules an upgrade, so it is never active again until
// something reschedules it. A miner vote that a reorg has undone withdraws its
// activation height this way (see versionbits.go).
func ClearUpgradeHeight(name string) {
	upgradesMu.Lock()
	defer upgradesMu.Unlock()
	delete(upgrades, name)
}

// ClearUpgrades removes all scheduled upgrades (used by tests).
func ClearUpgrades() {
	upgradesMu.Lock()
	defer upgradesMu.Unlock()
	upgrades = map[string]uint64{}
}

// IsUpgradeActive reports whether the named upgrade is in force at blockHeight
// (i.e. blockHeight ≥ its scheduled activation height). An unscheduled upgrade is
// never active.
func IsUpgradeActive(name string, blockHeight uint64) bool {
	upgradesMu.RLock()
	defer upgradesMu.RUnlock()
	h, ok := upgrades[name]
	return ok && blockHeight >= h
}

// ScheduledUpgrades returns every upgrade with an activation height, by name.
//
// It exists for the clients that must reproduce a header's proof-of-work
// preimage: the scheduled commitments are part of that preimage from their
// activation height onward, so a verifier that does not know the schedule
// computes the wrong hash. A node publishes this (GET /info) and the browser
// verifier installs it before checking anything.
//
// Learning the schedule from a node is not a trust decision. A node that
// reports the wrong heights only makes its own headers fail to verify.
func ScheduledUpgrades() map[string]uint64 {
	upgradesMu.RLock()
	defer upgradesMu.RUnlock()
	out := make(map[string]uint64, len(upgrades))
	for name, height := range upgrades {
		out[name] = height
	}
	return out
}
