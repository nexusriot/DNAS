# e2e

Black-box functional tests. They start the real `dnas` binary, talk to it over
its HTTP API, and run the CLI against it — the same way a user would. Nothing
here imports a DNAS package, so a break in the *product* (wire format, CLI
output, startup flags, persistence) fails these tests even when every unit test
still passes.

```sh
make e2e          # against a binary built from the working tree
make e2e-docker   # the same suite, hermetically, in a container (needs only Docker)
```

They are behind the `e2e` build tag, so `make test` and `go test ./core/...`
never pick them up: they spawn processes, bind sockets, and take ~20 s.

Most commands print their errors and still exit 0, so assertions are on the
output text (`mustContain`). A few deliberately exit **non-zero** — `dnas health`
when a node is not ready, `dnas peers unban` on an unknown key, a bad
`-loglevel` — and those are driven with `cliAllowFail`, which returns the exit
status so the test can require it.

## Isolation

Every test is self-contained and leaves nothing behind:

- **Ports** are drawn from the kernel per node (`127.0.0.1:0` probe), so nothing
  collides with a service already on the host — including a real DNAS node.
- **State** lives in a `t.TempDir()` per node: its own `chain.db`, wallet, and
  persisted peers/bans/mempool. No test touches `$HOME` or the repo.
- **Chain** is regtest, so blocks are instant (no retargeting) and are mined only
  on demand via `POST /generate` — heights are deterministic, never a race with a
  background miner.
- **Nodes** are stopped with SIGTERM through `t.Cleanup`, which also exercises the
  graceful-shutdown path.

That is enough for the suite to be *repeatable* on a developer's machine. It is
not enough for it to be *hermetic*: run it on the host and the result still
depends on which Go toolchain is installed, what the module proxy serves, and
what the network lets through.

## Hermetic runs

`make e2e-docker` closes that gap. The [Dockerfile](Dockerfile) compiles both the
`dnas` binary and the test binary from this tree at image build time, and the
runtime image holds nothing else — no toolchain, no sources, no build cache:

| Input | How it is pinned |
|-------|------------------|
| Toolchain | base images pinned by digest, so every machine compiles with the same bytes |
| Dependencies | `GOPROXY=off`, `GOTOOLCHAIN=local` — a stray dependency fails the build instead of being downloaded |
| Test binary | compiled in the build stage (`go test -c`), so a run executes only what the build produced and Go's test cache cannot serve a stale result |
| Network | the run gets `--network none`: nodes talk over the container's own loopback, and nothing can reach a host, peer or proxy outside it |
| Filesystem | `--read-only` root plus a `tmpfs` for `TMPDIR`; `$HOME` points into the read-only root, so a test that writes there fails loudly |
| Privileges | non-root user, `--cap-drop ALL`, `--security-opt no-new-privileges` |

[.dockerignore](../.dockerignore) keeps local state out of the build context, so
the image is a function of the source and nothing else. That means the chain and
the soft state (`chain.db`, `peers.json`) — a "fresh" node in the container must
not start on somebody else's — and it also means **key material**: `wallet.json`,
the node identity `nodekey.json`, and a `dnas-backup.json` bundle have no
business inside an image. The signing flows' working files
(`multisig-spend.json`, `escrow.json`, an `invoice.json`) and the light-client
caches are excluded for the same reason plus a simpler one: nothing here
compiles them. The build needs no JSON from the repository root at all, so the
root `*.json` is excluded wholesale.

That the build fetches nothing is checkable, not just claimed — with the base
images already pulled:

```sh
docker build --network none --no-cache -f e2e/Dockerfile -t dnas-e2e .
```

Useful variations:

```sh
make e2e-docker E2E_ARGS='-test.run TestReorg'   # one test
make e2e-docker E2E_IMAGE=dnas-e2e:pr-42         # tag the image
make e2e-docker-shell                            # poke around inside the image
```

Bumping the toolchain is deliberate: change the tag in the Dockerfile, then
`docker pull golang:<tag>` and
`docker image inspect golang:<tag> --format '{{index .RepoDigests 0}}'`, and
paste the digest back. The digests are multi-arch manifest lists, so amd64 and
arm64 hosts resolve the same pin.

## What is covered

| Test | What would break it |
|------|---------------------|
| `TestNodeStartsAtGenesis` | non-deterministic genesis — two nodes could never agree on a chain |
| `TestMinePayAndConfirm` | the payment path, and `/tx/{hash}` reporting pending → confirmed |
| `TestUnknownTransactionIsNotFound` | a lookup inventing a result for an unknown id |
| `TestSupplyAccountingHolds` | coin created or destroyed outside the subsidy and the burn |
| `TestChainSurvivesRestart` | persistence, plus the tx index and burn total being rebuilt on replay |
| `TestAPITokenGuardsWrites` | an unauthenticated write reaching a token-protected node |
| `TestEventStreamPushesBlocks` | the SSE stream going silent |
| `TestTwoNodesConverge` | peer discovery, headers-first sync, or fork choice |
| `TestTransactionRelaysBetweenNodes` | transaction gossip, or a peer refusing another's block |
| `TestReorgReturnsOrphanedPaymentToMempool` | a payment vanishing with the branch that lost a reorg |
| `TestSPVVerifiesAPayment` | the header/proof wire format a light client depends on |
| `TestSPVRefusesToProveAnUnknownTransaction` | SPV "proving" something that was never mined |
| `TestSPVScanFindsAndClearsBlocks` | compact filters, including provable non-inclusion |
| `TestSPVHistoryReconstructsTransfers` | light-wallet history reconstruction |
| `TestFastSyncBootstrapsFromSnapshot` | snapshot verification against the header state root |
| `TestExternalMinerProducesABlock` | the `/blocktemplate` → `/submitblock` mining protocol |
| `TestNetworkSeparation` | a chain store or a client being usable on the wrong network |
| `TestMempoolReconciliationOnJoin` | a late-joining node never learning a pending payment |
| `TestAddressHistoryIndex` | the address index missing a payment, or paging the wrong way round |
| `TestAddressHistoryUnavailableWithoutTheIndex` | "no index" being reported as "no history" |
| `TestMinerSubmitsShares` | a share the node rejects — i.e. the miner and node disagreeing structurally |
| `TestFaucetHandsOutCoinOverHTTP` | the faucet not paying, or its cooldown not holding |
| `TestFaucetRefusedWithoutTheFlag` | a node giving coin away without being asked to |
| `TestFaucetCLIFundsAWallet` | the `dnas faucet` client path |
| `TestVaultColdKeySweepsImmediately` | a hot key spending early, or the cold key unable to rescue |
| `TestSponsoredTransfer` | the two-party fee-sponsorship flow, and a broke sender being unable to pay |
| `TestDBVerifyExportImport` | `dnas db` mis-reading, mis-verifying, or losing a chain on a round trip |
| `TestNodeIdentityIsNotTheWallet` | a node publishing its wallet address to every peer via its identity key |
| `TestSPVHeaderCache` | the light client re-downloading the whole header chain on every command |
| `TestPagedReadEndpoints` | a bulk read serializing the whole chain into one response |
| `TestPeersAndBansCLI` | peers/bans becoming unobservable or unmanageable — and a node connecting to itself |
| `TestStatsAndHealthAndReorgsCLI` | hashrate/timing/reorg reporting, and `health` failing to exit non-zero when unready |
| `TestBumpAndCancelCLI` | replace-by-fee being unreachable from a client, or a bump paying twice |
| `TestStructuredLogging` | `-logjson`/`-loglevel` silently not applying |
| `TestPrintConfigShowsTheResolvedSettings` | `-printconfig` starting the node, or hiding a resolved default |
| `TestMultisigSpendEndToEnd` | a funded multisig account being unspendable, or a stranger/duplicate signature being accepted |
| `TestEscrowReleaseEndToEnd` | one party moving escrowed coin alone, or a shared role passing as a 2-of-3 |
| `TestAnchorEndToEnd` | an unmined or altered file verifying as anchored |
| `TestInvoiceLifecycleEndToEnd` | an unpaid or 1-confirmation invoice being reported as settled |
| `TestWalletMessageSigningEndToEnd` | a forged address claim or a changed message verifying |
| `TestWalletPassphraseRotationEndToEnd` | a rotation losing the key, or an encrypted file failing unhelpfully |
| `TestBackupEndToEnd` | a backup that omits a key, includes the chain, or clobbers live files on restore |
| `TestTxInspectEndToEnd` | `tx verify` passing a transaction the node would refuse |
| `TestAssetRegistryEndToEnd` | an asset id with no way to learn what it is, or an unknown id printing as blank |
| `TestSendOptionsEndToEnd` | memo/expiry/lock-until not reaching the wire, or a dead-on-arrival expiry being signed |
| `TestPruningNodeReportsWhatItHolds` | `-prune` not being applied, or its floor not being enforced |
| `TestConsoleAnswersTheReadCommands` | the console losing a command, or `-console` not forcing the prompt |

## Adding a test

Use the harness in [harness_test.go](harness_test.go): `startNode` for a node
(`nodeOpts` covers seed peers, an API token, and reusing a data directory for
restart tests), then `generate`, `send`, `getJSON`, `post`, and `cli` to drive
it. `waitFor` polls anything asynchronous — never sleep on a fixed duration for
propagation.

Assert on behaviour a user could observe. Most CLI commands print their errors
and still exit 0, so check the output text (`mustContain`); for the ones whose
exit status is part of the contract (`dnas health`, `anchor verify`,
`invoice watch`, `tx verify`) use `cliAllowFail` and assert on both. `cliEnv`
passes a secret through the environment (a passphrase must not land in shell
history or `ps`), and `cliStdin` drives the console.

Two limits of the harness worth knowing before writing a test:

- **Regtest cannot mint a deep chain in one call.** Block timestamps advance a
  second each and `MaxFutureDrift` is 120 seconds, so a single `/generate` stops
  around 120 blocks in — a node refuses its own block as too far in the future.
  Anything needing more (pruning's 132-body floor, say) belongs in the unit
  suites, where the chain is built directly.
- **This module imports no DNAS package**, so a consensus constant used in an
  assertion has to be written out — and will not follow a change to core. Prefer
  asserting on what the binary *reports* over hard-coding a number.
