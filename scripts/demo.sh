#!/usr/bin/env bash
# End-to-end demo of a 3-node DNAS network. It shows:
#   - authenticated + encrypted P2P (a wrong-netkey node is rejected)
#   - peer discovery (node3 only knows node1, but finds node2 by gossip)
#   - a signed transfer confirming on every node, with heights converging
# Run: ./scripts/demo.sh   (edit the 190xx ports below if they are taken)
set -uo pipefail
cd "$(dirname "$0")/.."

BIN=$(mktemp -d)/dnas
go build -o "$BIN" ./cmd/dnas

DATA=$(mktemp -d)
mkdir -p "$DATA/n1" "$DATA/n2" "$DATA/n3" "$DATA/bad"
echo "data dir: $DATA"

# $BIN is a unique temp path, so pkill -f matches only our node processes.
cleanup() { pkill -f "$BIN" 2>/dev/null || true; }
trap cleanup EXIT

get() { curl -s "$1" | grep -oP "\"$2\":\s*\"?\K[^\",}]+" || true; }
wait_api() { for _ in $(seq 1 50); do curl -sf "$1/info" >/dev/null 2>&1 && return; sleep 0.2; done; }

# Poll until all three nodes agree on the tip hash (they briefly fork while
# racing on equal-difficulty blocks; a new block breaks the tie).
wait_converge() {
  for _ in $(seq 1 60); do
    local t1 t2 t3
    t1=$(get localhost:19080/info tip); t2=$(get localhost:19081/info tip); t3=$(get localhost:19082/info tip)
    if [ -n "$t1" ] && [ "$t1" = "$t2" ] && [ "$t2" = "$t3" ]; then return; fi
    sleep 0.5
  done
}

# exec so the backgrounded PID ($!) is the binary itself, not a wrapper shell.
run() { ( cd "$1" && shift && exec "$BIN" node "$@" </dev/null ); }

# node1: miner, no peers.  (stdin from /dev/null -> headless, no REPL)
run "$DATA/n1" -listen :19000 -api :19080 -mine -db chain.json -wallet w.json >"$DATA/n1.log" 2>&1 &
N1=$!
wait_api localhost:19080

# node2: connects to node1.
run "$DATA/n2" -listen :19001 -api :19081 -peers localhost:19000 -mine -db chain.json -wallet w.json >"$DATA/n2.log" 2>&1 &
N2=$!
wait_api localhost:19081

# node3: a non-mining observer that knows ONLY node1 -- it must discover node2
# by gossip. (Kept non-mining so the demo has a stable two-miner network.)
run "$DATA/n3" -listen :19002 -api :19082 -peers localhost:19000 -db chain.json -wallet w.json >"$DATA/n3.log" 2>&1 &
N3=$!
wait_api localhost:19082

A1=$(get localhost:19080/address address)
A3=$(get localhost:19082/address address)
echo "node1: $A1"
echo "node3: $A3"

echo; echo "== (4) auth: a node with the wrong -netkey is rejected =="
run "$DATA/bad" -listen :19003 -api :19083 -peers localhost:19000 -netkey WRONG-KEY -db c.json -wallet w.json >"$DATA/bad.log" 2>&1 &
BAD=$!
sleep 3
if grep -qi "handshake" "$DATA/bad.log"; then
  echo "  rejected (handshake failed): $(grep -i handshake "$DATA/bad.log" | head -1)"
else
  echo "  (no handshake failure logged)"
fi
kill "$BAD" 2>/dev/null; BAD=

echo; echo "mining + discovering for ~12s..."
sleep 12

echo; echo "== (3) discovery: node3 was seeded with node1 only =="
echo "  node3 peers now: $(curl -s localhost:19082/peers)"

echo; echo "== node1 sends 3 DNAS (fee 0.1) to node3 =="
SEND=$(curl -s -X POST localhost:19080/send -d "{\"to\":\"$A3\",\"amount\":300000000,\"fee\":10000000}")
echo "  $SEND"
TXH=$(echo "$SEND" | grep -oP '"hash":"\K[^"]+')
echo "waiting for it to be mined and for the nodes to converge..."
sleep 6
wait_converge

echo; echo "== node3 balance seen by each node (should agree) =="
echo "  via node1: $(get localhost:19080/balance/$A3 balance_fmt)"
echo "  via node2: $(get localhost:19081/balance/$A3 balance_fmt)"
echo "  via node3: $(get localhost:19082/balance/$A3 balance_fmt)"

echo; echo "== (2) heights + cumulative work (converged) =="
for n in 19080 19081 19082; do
  echo "  :$n height=$(get localhost:$n/info height) work=$(get localhost:$n/info work)"
done

echo; echo "== (expiry) a transaction that expired at height 1 is refused =="
echo "  $(curl -s -X POST localhost:19080/send -d "{\"to\":\"$A3\",\"amount\":100000000,\"expiry\":1}")"

echo; echo "== (SPV) light client verifies the transfer with a merkle proof =="
PROOF=$(curl -s "localhost:19082/proof/$TXH")
IDX=$(echo "$PROOF" | grep -oP '"block_index":\K[0-9]+')
HDR=$(curl -s "localhost:19082/header/$IDX")
if command -v python3 >/dev/null 2>&1; then
  # A light client trusts only block headers. Verify the header's PoW, then
  # fold the merkle proof and check it reproduces that header's merkle root.
  python3 -c '
import sys, json, hashlib
proof = json.loads(sys.argv[1]); hdr = json.loads(sys.argv[2]); leaf = sys.argv[3]
def sha(x): return hashlib.sha256(x.encode()).hexdigest()
hs = "{}|{}|{}|{}|{}|{}".format(hdr["index"], hdr["timestamp"], hdr["prev_hash"], hdr["merkle_root"], hdr["difficulty"], hdr["nonce"])
pow_ok = sha(hs) == hdr["hash"] and hdr["hash"].startswith("0" * hdr["difficulty"])
h = leaf
for s in proof["proof"]:
    h = sha(h + s["hash"]) if s["right"] else sha(s["hash"] + h)
print("  header %d PoW valid: %s" % (hdr["index"], pow_ok))
print("  merkle proof folds to header root: %s" % (h == hdr["merkle_root"]))
print("  => tx PROVEN in block %d (%s confirmations)" % (proof["block_index"], proof["confirmations"]) if pow_ok and h == hdr["merkle_root"] else "  => VERIFICATION FAILED")
' "$PROOF" "$HDR" "$TXH"
else
  echo "  (python3 not found; raw proof: $PROOF)"
fi
