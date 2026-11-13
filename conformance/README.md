# Conformance runner (first slice)

Runs the same corpus through the JavaScript, Go and Rust SDKs and reports where their decoding or encoding diverges. CI runs it on every push and fails when a current SDK release stops round-tripping a corpus file or a sighting check changes its expected result.

| SDK | Runner | Versions in CI |
| --- | --- | --- |
| js-stellar-sdk | `js/run.mjs` | 17.2.1 (pre-fix tag for #1789) and 17.4.0 |
| go-stellar-sdk | `go/main.go` | v0.9.0 |
| rs-stellar-xdr | `rust.py`, through `tools/roundtrip` | 30.0.0 |

Each runner writes `results/<sdk>-<version>.json` with two parts:

- `corpus`: every file in `corpus/manifest.json`, decoded with the SDK's own XDR type, re-encoded and compared byte for byte.
- `vectors`: every sighting in `sightings.json`. A vector is the XDR of a sighting plus the behaviour every SDK must show. Each runner reports `defect` (the sighting's bug is present), `ok` (it behaves as the fix requires) or `n/a` (the SDK has no API for that behaviour).

`matrix.py` merges the results into [MATRIX.md](MATRIX.md) and checks the expectations.

## Current result

| Sighting | JS 17.2.1 | JS 17.4.0 | Go v0.9.0 | Rust 30.0.0 |
| --- | --- | --- | --- | --- |
| js-stellar-sdk#1789 | `defect`: 2 entries in XDR, 1 after `scValToNative` | `ok`: throws | `n/a`: `ScMap` decodes as a slice | `n/a`: no native map conversion |

All 16 corpus files round-trip byte-identical in JS 17.4.0, Go v0.9.0 and Rust 30.0.0. JS 17.2.1 decodes the transaction files but not the protocol 30 ledger headers, which it predates.

The Python sibling of #1789 is filed as StellarCN/py-stellar-base#1250.

## Run

```bash
(cd conformance/js/sdk-17.2.1 && npm ci) && (cd conformance/js/sdk-17.4.0 && npm ci)
node conformance/js/run.mjs conformance/js/sdk-17.2.1 conformance/results/js-17.2.1.json
node conformance/js/run.mjs conformance/js/sdk-17.4.0 conformance/results/js-17.4.0.json
(cd conformance/go && go run . ../.. ../results/go-v0.9.0.json)
cargo build --release --locked --manifest-path tools/roundtrip/Cargo.toml
python conformance/rust.py conformance/results/rust-30.0.0.json
python conformance/matrix.py
```

## Next

js-stellar-sdk#1766 (issuer order) and #1785 (IDs that encode a different ID) as vectors with their pre-fix tags, more vectors from the RFP's list of shapes that tripped one SDK, and Python as a fourth column.
