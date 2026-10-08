# Browser verification bundle

Build artifacts, not sources. `make wasm` puts two files here:

- `dnas.wasm` — [cmd/wasmverify](../../cmd/wasmverify) compiled for
  `js/wasm`. It exports the consensus verification primitives (header hash and
  proof-of-work, merkle inclusion, state/account proofs, ancestry proofs,
  committed filters) under a global `dnas` object.
- `wasm_exec.js` — Go's runtime shim, copied verbatim from
  `$(go env GOROOT)/lib/wasm/`. It must match the toolchain that built the
  `.wasm`, which is why it is copied by the build rather than vendored.

Both are gitignored. This README is committed so the `go:embed` of this
directory succeeds in a tree where `make wasm` has not been run — the explorer
then reports that the verifier is unavailable instead of silently falling back
to a second implementation. There is deliberately no JavaScript fallback: the
whole point of the bundle is that there is exactly one implementation of these
rules, and every copy this project has ever kept outside `core` has rotted.
