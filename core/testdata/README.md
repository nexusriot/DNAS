# Golden consensus vectors

`consensus_vectors.json` is a language-neutral corpus of consensus-visible
values: transaction ids, signing preimages, block hashes, state roots, address
derivations, the difficulty encoding, and the validity verdicts that go with
them. An implementation that reproduces every value in this file agrees with the
Go implementation on everything a node must agree on to stay on the same chain.

It exists because the canonical codec ([`core/codec.go`](../codec.go)) makes a
spec *possible* but does not make one *true*. Until a second client exists, this
file is the closest thing to a check on the spec — and when a second client is
written, this is what it should be tested against first, before it is ever
pointed at a network.

## Using it

**From Go** — the corpus is generated and verified by
[`core/vectors_test.go`](../vectors_test.go):

```sh
go test ./core -run TestConsensusVectors           # verify against the current code
go test ./core -run TestConsensusVectors -update   # regenerate after a deliberate change
```

A verification failure means a consensus-visible value moved. That is sometimes
intentional (a deliberate fork) and sometimes a bug. Either way it should be a
decision rather than a surprise, which is the whole point of committing the file.

**From another language** — parse the JSON and check each section against your
own implementation. Nothing in the file depends on Go: every value is a string,
a number, or a hex-encoded byte string.

## Reproducing the keys

Every key in the corpus derives from one fixed BIP39 mnemonic, recorded in the
`mnemonic` field. Derivation is `HMAC-SHA512(seed, "dnas/ed25519" || uint32be(index))`,
truncated to 32 bytes and used as an Ed25519 seed (see
[`wallet/hd.go`](../../wallet/hd.go) — note this is *not* SLIP-0010). Ed25519
signing is deterministic (RFC 8032), so the same key over the same message
yields byte-identical signatures in any correct implementation: the signatures in
this file are reproducible, not merely valid.

## Sections

| Section | What it pins |
|---|---|
| `params` | The consensus constants themselves. Check these first — an implementation that disagrees here will disagree about everything downstream. |
| `amounts` | Decimal DNAS ↔ base units, including the truncation rule (a 9th fractional digit is dropped, not rounded). |
| `addresses` | Public key → address, including the checksum. A wrong checksum implementation lets users burn coin. |
| `script_addresses` | Multisig, HTLC and vault addresses. Each folds its parameters into the address hash, and each is domain-separated from the others. |
| `asset_ids` | Asset id = f(issuer, ticker, nonce), so two issuers can both mint `GOLD`. |
| `block_rewards` | The halving schedule, including where it reaches zero. |
| `targets` | The compact (`nBits`) difficulty encoding and its round trip. A miner that gets this wrong mines against the wrong target. |
| `merkle_roots` | Including the odd-count case where the last node is duplicated — the classic place two implementations diverge. |
| `state_roots` | The account-set commitment, including the empty sentinel and an account holding assets. |
| `headers` | The block-hash preimage **verbatim**, not just its hash, so a mismatch says *where*. |
| `transactions` | The heart of it: for each shape, on each network, the signing preimage, the full canonical encoding, the txid, the byte size, the verification cost, and the sanity verdict. |
| `fee_splits` | How a fee divides into the burned base-fee portion and the miner's tip. |
| `block_filters` | Which addresses a block's compact filter matches. |

## Transactions: the two encodings

Each entry carries two hex strings, and the difference between them matters:

- **`signing_preimage_hex`** is what the *sender* signs. It covers the fields that
  define the transfer, and it includes the **network id**, so the same transfer
  signed on testnet produces different bytes than on mainnet and cannot be
  replayed across chains. Mainnet's id is empty and contributes nothing, which is
  why mainnet encodings are byte-identical to a pre-network-id implementation.
- **`canonical_hex`** is the full encoding, signing preimage plus the
  authorization fields. `txid = sha256(canonical_hex)` and `size = len(canonical_hex)/2`.
  Size is fee-bearing, so an implementation that encodes differently charges
  different fees and will reject blocks other nodes accept.

The corpus covers each shape on all three networks (`mainnet`, `testnet`,
`regtest`): plain transfer, memo + height window, multi-output, asset issue,
asset transfer, fee-sponsored, coinbase, and an unsigned transaction.

`sanity` is `""` when `CheckTxSanity` accepts the transaction and its error
string otherwise. Match the *verdict*; the exact wording is not consensus.

## What it does not cover

Being explicit, because a corpus that looks exhaustive and is not is worse than
one that admits its edges:

- **No chain-level validation.** Nothing here exercises fork choice, reorg
  handling, difficulty retargeting over a real window, coinbase maturity, or the
  base fee responding to load. Those need a chain, not a fixture.
- **No P2P.** The wire protocol, handshake and sync are not represented.
- **No negative-space proof.** The corpus shows that these inputs produce these
  outputs. It cannot show that no *other* input is mishandled — that is what
  fuzzing and a second implementation are for.
- **The state root is a sorted-leaf Merkle fold**, so it proves account
  membership but not absence. If that construction changes, every value in
  `state_roots` and every `headers` entry changes with it.
