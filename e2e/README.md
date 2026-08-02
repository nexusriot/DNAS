# e2e

Black-box functional tests. They start the real `dnas` binary, talk to it over
its HTTP API, and run the CLI against it — the same way a user would. Nothing
here imports a DNAS package, so a break in the *product* (wire format, CLI
output, startup flags, persistence) fails these tests even when every unit test
still passes.

```sh
make e2e          # against a binary built from the working tree
make e2e-docker   # the same suite, isolated in a container (needs only Docker)
```

They are behind the `e2e` build tag, so `make test` and `go test ./core/...`
never pick them up: they spawn processes, bind sockets, and take ~15 s.

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

`make e2e-docker` adds a fourth layer: the binary is compiled and the suite runs
entirely inside the container, so the host needs no Go toolchain and every
listener lives in the container's own network namespace. See
[Dockerfile](Dockerfile); [.dockerignore](../.dockerignore) keeps local state
(`chain.db`, `wallet.json`) out of the build context.

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

## Adding a test

Use the harness in [harness_test.go](harness_test.go): `startNode` for a node
(`nodeOpts` covers seed peers, an API token, and reusing a data directory for
restart tests), then `generate`, `send`, `getJSON`, `post`, and `cli` to drive
it. `waitFor` polls anything asynchronous — never sleep on a fixed duration for
propagation.

Assert on behaviour a user could observe. The CLI prints its errors and still
exits 0, so check the output text (`mustContain`) rather than the exit status.
