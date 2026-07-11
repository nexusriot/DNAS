#!/usr/bin/env bash
# Demo of hash-time-locked contracts (HTLCs) — the primitive behind cross-chain
# atomic swaps. On a single mining node it shows both spend paths:
#   - CLAIM:  the recipient spends by revealing a preimage (published on-chain).
#   - REFUND: the sender reclaims the coins, but only once the timeout height is
#             reached (a refund submitted earlier waits, then mines itself).
# Run: ./scripts/htlc-demo.sh   (edit the 190xx ports below if they are taken)
set -uo pipefail
cd "$(dirname "$0")/.."

BIN=$(mktemp -d)/dnas
go build -o "$BIN" ./cmd/dnas

DATA=$(mktemp -d)
mkdir -p "$DATA/node"
echo "data dir: $DATA"
cleanup() { pkill -f "$BIN" 2>/dev/null || true; }
trap cleanup EXIT

API=localhost:19080
get() { curl -s "$1" | grep -oP "\"$2\":\s*\"?\K[^\",}]+" || true; }
wait_api() { for _ in $(seq 1 50); do curl -sf "$1/info" >/dev/null 2>&1 && return; sleep 0.2; done; }
height() { get "$API/info" height; }
balance() { get "$API/balance/$1" balance; }
wait_height() { while [ "$(height)" -lt "$1" ]; do sleep 0.5; done; }
wait_balance() { while [ "$(balance "$1")" -lt "$2" ]; do sleep 0.5; done; }

run() { ( cd "$DATA/node" && exec "$BIN" node -listen :19000 -api :19080 -mine -db chain.db -wallet w.json </dev/null ); }
run >"$DATA/node.log" 2>&1 &
wait_api "$API"

# Two user wallets: alice (recipient/claimer) and bob (sender/refunder).
"$BIN" wallet new -o "$DATA/alice.json" >/dev/null
"$BIN" wallet new -o "$DATA/bob.json"   >/dev/null
ALICE=$("$BIN" wallet address -o "$DATA/alice.json"); ALICE_PUB=$("$BIN" wallet pubkey -o "$DATA/alice.json")
BOB=$("$BIN"   wallet address -o "$DATA/bob.json");   BOB_PUB=$("$BIN"   wallet pubkey -o "$DATA/bob.json")
echo "alice (recipient): $ALICE"
echo "bob   (sender):    $BOB"

echo; echo "waiting for the node's mining wallet to have spendable (matured) coins..."
wait_height 5   # ~4 coinbases so at least one has matured past CoinbaseMaturity

FEE=10000000    # 0.1 DNAS

echo; echo "===================== CLAIM path ====================="
# A far-future timeout: the refund branch never opens, so only the claim works.
read PRE HASH < <("$BIN" htlc new | grep -oP ':\s*\K[0-9a-f]+' | tr '\n' ' ')
TC=$(( $(height) + 1000 ))
HC=$("$BIN" htlc address -hash "$HASH" -recipient "$ALICE_PUB" -sender "$BOB_PUB" -timeout "$TC")
echo "contract address: $HC   (hash=${HASH:0:16}… timeout=$TC)"

echo "-> node funds the contract with 10 DNAS"
curl -s -X POST "$API/send" -d "{\"to\":\"$HC\",\"amount\":1000000000,\"fee\":$FEE}" >/dev/null
wait_balance "$HC" 1000000000
echo "   contract balance: $(get "$API/balance/$HC" balance_fmt)"

echo "-> alice claims by revealing the preimage"
"$BIN" htlc claim -api "$API" -wallet "$DATA/alice.json" \
  -hash "$HASH" -sender "$BOB_PUB" -timeout "$TC" -preimage "$PRE" -to "$ALICE" -fee "$FEE"
wait_balance "$ALICE" 1
echo "   alice balance:    $(get "$API/balance/$ALICE" balance_fmt)"
echo "   contract drained: $(get "$API/balance/$HC" balance_fmt)"

echo; echo "===================== REFUND path ====================="
read PRE2 HASH2 < <("$BIN" htlc new | grep -oP ':\s*\K[0-9a-f]+' | tr '\n' ' ')
TR=$(( $(height) + 3 ))   # timeout only a few blocks away
HR=$("$BIN" htlc address -hash "$HASH2" -recipient "$ALICE_PUB" -sender "$BOB_PUB" -timeout "$TR")
echo "contract address: $HR   (refund opens at height $TR)"

echo "-> node funds the contract with 8 DNAS"
curl -s -X POST "$API/send" -d "{\"to\":\"$HR\",\"amount\":800000000,\"fee\":$FEE}" >/dev/null
wait_balance "$HR" 800000000
echo "   contract balance: $(get "$API/balance/$HR" balance_fmt)"

echo "-> bob submits a refund BEFORE the timeout (height $(height) < $TR): it should wait"
"$BIN" htlc refund -api "$API" -wallet "$DATA/bob.json" \
  -hash "$HASH2" -recipient "$ALICE_PUB" -timeout "$TR" -to "$BOB" -fee "$FEE"
sleep 3
echo "   bob balance while locked: $(get "$API/balance/$BOB" balance_fmt) (mempool: $(get "$API/info" mempool))"

echo "-> waiting for the chain to reach the timeout height $TR ..."
wait_height "$TR"
wait_balance "$BOB" 1
echo "   bob balance after timeout: $(get "$API/balance/$BOB" balance_fmt)"
echo "   contract drained:          $(get "$API/balance/$HR" balance_fmt)"

echo; echo "done — claim (hashlock) and refund (timelock) both settled."
