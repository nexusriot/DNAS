#!/usr/bin/env bash
# Demo of an on-chain ASSET-for-COIN atomic swap, built from two hash-time-locked
# contracts that share one hash. Alice holds a native asset and wants coin; Bob
# holds coin and wants the asset. Neither has to trust the other:
#
#   1. alice mints a preimage and publishes only its hash;
#   2. alice funds the ASSET leg, claimable by bob with the preimage;
#   3. bob funds the COIN leg, claimable by alice with the preimage;
#   4. alice claims the coin leg — which PUBLISHES the preimage on-chain;
#   5. bob reads it out of that transaction and claims the asset.
#
# If either walks away, both legs refund after their timeouts. The coin leg times
# out FIRST, so the party holding the secret cannot let their own leg refund and
# still claim the other's — `dnas htlc swap` refuses any other ordering.
#
# Run: ./scripts/swap-demo.sh   (edit the 191xx ports below if they are taken)
set -uo pipefail
cd "$(dirname "$0")/.."

BIN=$(mktemp -d)/dnas
go build -o "$BIN" ./cmd/dnas

DATA=$(mktemp -d)
mkdir -p "$DATA/node"
echo "data dir: $DATA"
cleanup() { pkill -f "$BIN" 2>/dev/null || true; }
trap cleanup EXIT

API=localhost:19180
get() { curl -s "$1" | grep -oP "\"$2\":\s*\"?\K[^\",}]+" || true; }
wait_api() { for _ in $(seq 1 50); do curl -sf "$1/info" >/dev/null 2>&1 && return; sleep 0.2; done; }
height() { get "$API/info" height; }
balance() { get "$API/balance/$1" balance; }
asset_of() { curl -s "$API/account/$1" | grep -oP "\"$2\":\s*\K[0-9]+" || echo 0; }
wait_height() { while [ "$(height)" -lt "$1" ]; do sleep 0.5; done; }
wait_balance() { while [ "$(balance "$1")" -lt "$2" ]; do sleep 0.5; done; }
wait_asset() { while [ "$(asset_of "$1" "$2")" -lt "$3" ]; do sleep 0.5; done; }

run() { ( cd "$DATA/node" && exec "$BIN" node -listen :19100 -api :19180 -mine -db chain.db -wallet w.json </dev/null ); }
run >"$DATA/node.log" 2>&1 &
wait_api "$API"

"$BIN" wallet new -o "$DATA/alice.json" >/dev/null
"$BIN" wallet new -o "$DATA/bob.json"   >/dev/null
ALICE=$("$BIN" wallet address -o "$DATA/alice.json"); ALICE_PUB=$("$BIN" wallet pubkey -o "$DATA/alice.json")
BOB=$("$BIN"   wallet address -o "$DATA/bob.json");   BOB_PUB=$("$BIN"   wallet pubkey -o "$DATA/bob.json")
echo "alice (has the asset, wants coin): $ALICE"
echo "bob   (has coin, wants the asset): $BOB"

echo; echo "waiting for the node's mining wallet to have spendable (matured) coins..."
wait_height 5

FEE=10000000        # 0.1 DNAS
FUND=2000000000     # 20 DNAS, enough for fees and the trade
COIN_AMOUNT=1000000000  # 10 DNAS, the price
ASSET_AMOUNT=500

echo; echo "-> funding alice and bob from the mining node"
curl -s -X POST "$API/send" -d "{\"to\":\"$ALICE\",\"amount\":$FUND,\"fee\":$FEE}" >/dev/null
wait_balance "$ALICE" "$FUND"
curl -s -X POST "$API/send" -d "{\"to\":\"$BOB\",\"amount\":$FUND,\"fee\":$FEE}" >/dev/null
wait_balance "$BOB" "$FUND"
echo "   alice: $(get "$API/balance/$ALICE" balance_fmt)   bob: $(get "$API/balance/$BOB" balance_fmt)"

echo; echo "-> alice issues 500 GOLD (all of it goes into the trade)"
"$BIN" spv -api "$API" wallet -f "$DATA/alice-spv.json" -key "$DATA/alice.json" issue GOLD 500 >/dev/null
sleep 3
ASSET=$(curl -s "$API/account/$ALICE" | grep -oP '"assets":\{"\K[^"]+')
if [ -z "$ASSET" ]; then echo "issuance did not confirm; see $DATA/node.log"; exit 1; fi
echo "   asset id: $ASSET  (alice holds $(asset_of "$ALICE" "$ASSET"))"

echo; echo "===================== deriving both legs ====================="
read PRE HASH < <("$BIN" htlc new | grep -oP ':\s*\K[0-9a-f]+' | tr '\n' ' ')
COIN_TO=$(( $(height) + 500 ))
ASSET_TO=$(( COIN_TO + 500 ))   # the asset leg must outlive the coin leg
"$BIN" htlc swap -hash "$HASH" -asset-owner "$ALICE_PUB" -coin-owner "$BOB_PUB" \
  -asset "$ASSET" -asset-amount "$ASSET_AMOUNT" -coin-amount 10 \
  -coin-timeout "$COIN_TO" -asset-timeout "$ASSET_TO" | head -6

ASSET_LEG=$("$BIN" htlc address -hash "$HASH" -recipient "$BOB_PUB"   -sender "$ALICE_PUB" -timeout "$ASSET_TO")
COIN_LEG=$( "$BIN" htlc address -hash "$HASH" -recipient "$ALICE_PUB" -sender "$BOB_PUB"   -timeout "$COIN_TO")

echo; echo "===================== funding ====================="
echo "-> alice funds the asset leg with $ASSET_AMOUNT $ASSET, plus coin for its claim fee"
"$BIN" spv -api "$API" wallet -f "$DATA/alice-spv.json" -key "$DATA/alice.json" \
  -asset "$ASSET" send "$ASSET_LEG" "$ASSET_AMOUNT" >/dev/null
wait_asset "$ASSET_LEG" "$ASSET" "$ASSET_AMOUNT"
"$BIN" spv -api "$API" wallet -f "$DATA/alice-spv.json" -key "$DATA/alice.json" \
  send "$ASSET_LEG" 1 >/dev/null
wait_balance "$ASSET_LEG" 1
echo "   asset leg holds $(asset_of "$ASSET_LEG" "$ASSET") $ASSET and $(get "$API/balance/$ASSET_LEG" balance_fmt)"

echo "-> bob funds the coin leg with the price plus its claim fee"
"$BIN" spv -api "$API" wallet -f "$DATA/bob-spv.json" -key "$DATA/bob.json" \
  send "$COIN_LEG" 11 >/dev/null
wait_balance "$COIN_LEG" "$COIN_AMOUNT"
echo "   coin leg holds  $(get "$API/balance/$COIN_LEG" balance_fmt)"

echo; echo "===================== settling ====================="
echo "-> alice claims the coin leg, publishing the preimage on-chain"
"$BIN" htlc claim -api "$API" -wallet "$DATA/alice.json" \
  -hash "$HASH" -sender "$BOB_PUB" -timeout "$COIN_TO" -preimage "$PRE" -to "$ALICE" -fee "$FEE"
sleep 3
echo "   alice: $(get "$API/balance/$ALICE" balance_fmt)"

echo "-> bob reads the preimage off the chain and claims the asset"
"$BIN" htlc claim -api "$API" -wallet "$DATA/bob.json" \
  -hash "$HASH" -sender "$ALICE_PUB" -timeout "$ASSET_TO" -preimage "$PRE" \
  -asset "$ASSET" -to "$BOB" -fee "$FEE"
wait_asset "$BOB" "$ASSET" "$ASSET_AMOUNT"

echo; echo "===================== result ====================="
echo "alice started with 20 DNAS and $ASSET_AMOUNT of the asset; bob with 20 DNAS and none."
echo "alice: $(get "$API/balance/$ALICE" balance_fmt), $(asset_of "$ALICE" "$ASSET") asset (traded it away)"
echo "bob:   $(get "$API/balance/$BOB" balance_fmt), $(asset_of "$BOB" "$ASSET") asset (paid 10 DNAS for it)"
echo
echo "done — the asset and the coin changed hands atomically, with no escrow and"
echo "no trust: either both claims happen, or both legs refund after their timeouts."
