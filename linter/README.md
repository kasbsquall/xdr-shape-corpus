# xdrlint (prototype)

Go analyzer for indexer source code. The RFP asks for three rules; this prototype ships the first one.

| Rule | Mistake it flags | Status |
| --- | --- | --- |
| XDRL001 | Trusting fields stellar-core does not validate (memos, Soroban auth trees, muxed IDs) | planned |
| XDRL002 | Recursive walks over ScVal, ScMap, ScVec or SorobanAuthorizedInvocation without a depth bound | working prototype |
| XDRL003 | Deriving transaction participants from the envelope instead of the ledger meta | planned |

## XDRL002

A function is reported when it takes a recursive XDR type from `github.com/stellar/go-stellar-sdk/xdr` (or the archived `github.com/stellar/go/xdr`), calls itself, and has no integer parameter that could carry a depth bound.

## Evidence (2026-11-12)

- `go test ./...`: the seeded violations in `depthcap/testdata/src/example` (unbounded ScVal walk, unbounded auth-tree walk) are reported; the bounded and non-recursive functions are not.
- Clean run on stellar/wallet-backend at commit 3e22515a44979fdc3a558fda3991ca2b48e38cef (2026-11-12), 40 packages, exit code 0, no findings.
- Positive control inside the same wallet-backend module, using its real go-stellar-sdk XDR types: a temporary file with an unbounded recursive ScVal walk is reported as XDRL002. The file was removed afterwards.

## Run

```bash
go build -o xdrlint ./cmd/xdrlint
cd <indexer-module> && xdrlint ./...
```
