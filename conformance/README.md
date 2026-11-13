# Conformance runner

Runs the same corpus and sighting checks through the JavaScript, Go, Rust and Python SDKs and reports where they diverge. CI runs it on every push and fails when a current SDK release stops round-tripping a corpus file or a sighting check changes its expected result.

| SDK | Runner | Versions in CI |
| --- | --- | --- |
| js-stellar-sdk | `js/run.mjs` | 17.2.0 (pre-fix tag for #1766), 17.2.1 (pre-fix tag for #1785 and #1789) and 17.4.0 |
| go-stellar-sdk | `go/main.go` | v0.9.0 |
| rs-stellar-xdr | `rust.py`: round trips through `tools/roundtrip`, API checks through `rust/` | 30.0.0 |
| py-stellar-base | `python/run.py` | stellar-sdk 17.0.1 |

Each runner writes `results/<sdk>-<version>.json` with two parts:

- `corpus`: every file in `corpus/manifest.json`, decoded with the SDK's own XDR type, re-encoded and compared byte for byte.
- `vectors`: every check in `sightings.json`, with `defect` (the sighting's bug is present), `ok` (the SDK behaves as the fix requires) or `n/a` (the SDK has no API for that behaviour), plus a short `detail`.

`matrix.py` merges the results into [MATRIX.md](MATRIX.md) and checks the expectations.

## Sightings

A sighting in `sightings.json` takes one of two forms:

- An XDR vector: `file` and `xdrType` name the bytes, and `check` names the behaviour to test on the decoded value. The runner also round-trips the vector.
- An API check: `inputs` holds the values to pass to the SDK as JSON, and `check` names the behaviour. IDs are decimal strings so no JSON parser rounds them; an SDK that takes IDs as native integers runs only the inputs that are canonical decimal integers its integer type can hold.

`expected` states in words what the fix requires, `fixedIn` is the first js-stellar-sdk release with the fix, and `siblings` links the issues filed in other SDKs.

| Check | Sighting | What the SDK must do |
| --- | --- | --- |
| `native-map-conversion-keeps-every-entry` | #1789 | Keep both entries of a map whose keys convert to the same native key, or throw |
| `asset-pair-ordered-by-issuer-bytes` | #1766 | Order two assets with the same code by issuer key bytes, not by `G…` text |
| `scmap-keys-sorted-like-host` | #1766 | Sort map keys like the Soroban host: by type, then by value |
| `muxed-id-encodes-the-id-given` | #1785 | Encode and report the muxed ID given, or throw |
| `offer-id-encodes-the-id-given` | #1785 | Encode the offer ID given, or throw |

## Current result

| Sighting | Vector or check | JS 17.2.0 | JS 17.2.1 | JS 17.4.0 | Go v0.9.0 | Rust 30.0.0 | Python 17.0.1 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| #1789 | `js-1789-map-u32-and-symbol-key.b64` | `defect` | `defect` | `ok` | `n/a` | `n/a` | `ok` |
| #1789 | `js-1789-map-string-and-symbol-key.b64` | `defect` | `defect` | `ok` | `n/a` | `n/a` | `defect` |
| #1766 | `asset-pair-ordered-by-issuer-bytes` | `defect` | `ok` | `ok` | `ok` | `ok` | `ok` |
| #1766 | `scmap-keys-sorted-like-host` | `defect` | `ok` | `ok` | `n/a` | `ok` | `ok` |
| #1785 | `muxed-id-encodes-the-id-given` | `defect` | `defect` | `ok` | `n/a` | `n/a` | `defect` |
| #1785 | `offer-id-encodes-the-id-given` | `defect` | `defect` | `ok` | `ok` | `n/a` | `defect` |

The pre-fix tags reproduce all three js-stellar-sdk sightings, and 17.4.0 passes them. [MATRIX.md](MATRIX.md) has the detail of every cell, including why each `n/a` does not apply.

The Python defects are open issues filed in py-stellar-base, and `matrix.py` expects them until they are fixed: StellarCN/py-stellar-base#1250 (`scval.to_native` drops entries whose keys collide, here string `"1"` and symbol `"1"`) and StellarCN/py-stellar-base#1251 (an offer ID or muxed ID above the 64-bit range wraps, so 2^64 encodes as 0). Python also accepts an offer ID of -1.

All 36 corpus files round-trip byte-identical in JS 17.4.0, Go v0.9.0, Rust 30.0.0 and Python 17.0.1. JS 17.2.0 and 17.2.1 decode the transaction files but not the protocol 30 ledger headers, which they predate.

## Run

```bash
for v in 17.2.0 17.2.1 17.4.0; do
  (cd conformance/js/sdk-$v && npm ci)
  node conformance/js/run.mjs conformance/js/sdk-$v conformance/results/js-$v.json
done
(cd conformance/go && go run . ../.. ../results/go-v0.9.0.json)
cargo build --release --locked --manifest-path tools/roundtrip/Cargo.toml
cargo build --release --locked --manifest-path conformance/rust/Cargo.toml
python conformance/rust.py conformance/results/rust-30.0.0.json
pip install -r conformance/python/requirements.txt  # in a virtual environment
python conformance/python/run.py conformance/results/python-17.0.1.json
python conformance/matrix.py
```

## Next

More vectors from the RFP's list of shapes that tripped one SDK.
