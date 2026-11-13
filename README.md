# Stellar XDR Shape Corpus

A language-neutral corpus of rare but valid Stellar ledger shapes, captured from the network as raw XDR, with the values a correct decoder must produce. Around it: a replay harness that serves the corpus over a mock RPC `getLedgers` endpoint to a real indexer and reports `pass`, `mishandled` or `dropped` per shape, a Go linter for indexer code, and the outline of an indexer-hardening guide.

This repository is the working prototype for the SCF RFP *XDR/CAP-Coverage Audit Tool and Indexer-Hardening Guide*. Everything here runs today; what is still planned is marked as planned.

## Status (2026-11-13)

| Piece | State |
| --- | --- |
| Manifest schema | Draft 0.1, JSON Schema 2020-12 ([schema/manifest.schema.json](schema/manifest.schema.json)) |
| Corpus | 9 entries captured on testnet at protocol 30, each with envelope, result, `TransactionMeta` v4 and the full `LedgerCloseMeta`; none synthetic |
| Byte check | Every file decodes and re-encodes byte-identical with rs-stellar-xdr 30.0.0; checked in CI |
| stellar-core goldens | Every `--check-lcm` golden at a pinned core commit decodes byte-identical, and each entry's shapes are reported against what core emits: 13 agree, 1 elsewhere, 18 with no golden ([below](#stellar-core---check-lcm-goldens)) |
| Replay harness | Reference adapter for `stellar/wallet-backend`; reproduces #731, #729, #716 (signed-payload) and the muxed-destination gap closed by #792 on pre-fix commits, and passes on the fixed ones. Second adapter for `stellar/stellar-etl`: 6 entries pass, 3 reproduce known issues ([harness/etl/README.md](harness/etl/README.md)). Both run in CI |
| Generator | `cap40` and `cap27` regenerate on a local stellar-core under the testnet and mainnet passphrases and match the committed entries; in CI ([tools/generator/](tools/generator/)) |
| Conformance runner | Every corpus file and six sighting checks through js-stellar-sdk, go-stellar-sdk, rs-stellar-xdr and py-stellar-base in CI; js-stellar-sdk#1766, #1785 and #1789 reproduce on their pre-fix tags and pass on 17.4.0 ([conformance/MATRIX.md](conformance/MATRIX.md)) |
| Linter | `xdrlint`, rule XDRL002 working; XDRL001 and XDRL003 are outside the current scope, and the guide checklist covers all three by hand |
| Guide | Outline ([guide/OUTLINE.md](guide/OUTLINE.md)) |

## Corpus entries

| Entry | CAP | Testnet ledger | Transaction | What it exercises |
| --- | --- | --- | --- | --- |
| `cap40-signed-payload-signer-max-payload` | CAP-0040 | 5699703 | `68e7d549…` | Signed-payload signer with a 64-byte payload (the maximum), added through SetOptions |
| `cap27-muxed-source-and-destination-id-extremes` | CAP-0027 | 5699707 | `fae7b0f2…` | Muxed source with id 18446744073709551615 (u64 max) and muxed destination with id 0 |
| `cap73-sac-created-trustline` | CAP-0073 | 5699714 | `dab291a4…` | Trustline created by the Stellar Asset Contract `trust()` call, with no ChangeTrust operation and no contract event |
| `cap67-sac-transfer-to-muxed-address-max-id` | CAP-0067 | 5701159 | `713e56b3…` | SAC `transfer()` to a muxed `ScAddress` with id u64 max; the event topic carries the base G account and the id travels in `to_muxed_id` |
| `cap38-pool-share-issuers-ordered-by-key-bytes` | CAP-0038 | 5704246 | `a1787e27…` | Pool-share trustline and deposit for two `PAIR` assets whose issuers sort one way by `G…` text and the other way by key bytes; core puts them in byte order |
| `cap46-scmap-keys-collide-after-native-conversion` | CAP-0046 | 5704261 | `99f258dc…` | `ScMap` with keys u32 `1`, string `"1"` and symbol `"1"`, emitted by core in the invocation argument, the return value and a contract event |
| `cap46-nested-authorization-contract-calls-sac` | CAP-0046 | 5704270 | `de6c1555…` | Authorization entry signed by an account other than the source, with `pay` at the root and the native SAC `transfer` as its sub-invocation |
| `cap21-vectors-at-max-length` | CAP-0021, CAP-0040 | 5704274 | `55b54517…` | 100 operations (the maximum), each ManageData with a 64-character name and 64-byte value, a 28-byte text memo and two extra signers (the maximum) with 64-byte signed payloads |
| `cap06-manage-buy-offer-update-by-id` | CAP-0006 | 5704280 | `10ac5cc1…` | ManageBuyOffer that updates an offer by the ID core assigned; an ordinary in-range ID, since the network only emits IDs it allocated |

The two CAP-0046 entries call a minimal probe contract ([tools/contracts/probe](tools/contracts/probe/src/lib.rs)) deployed to testnet for the capture. Out-of-range muxed and offer IDs, the other half of the RFP's ID shape, cannot be emitted by the network; they are covered as SDK API checks in the conformance runner.

Each entry lives in `corpus/entries/<id>/`. [corpus/manifest.json](corpus/manifest.json) indexes them: CAPs, protocol version, network and passphrase, where the bytes came from, the shapes covered, the sightings that motivated the entry, sha256 of every file, and the expected values as JSON Pointers into the rs-stellar-xdr JSON form. The manifest is generated by [tools/build_manifest.py](tools/build_manifest.py), which reads every expected value back from the bytes and refuses to write if one does not match. Besides the shape values, each entry has seven `expectedLedger` values in its `LedgerCloseMeta`: ledger sequence, protocol 30, close time in seconds and the CAP-0088 millisecond close time, the envelope's place in the CAP-0042 transaction set (classic phase or the CAP-0063 parallel Soroban phase), the transaction hash at its apply position and the fee charged. Each is checked against something read separately from the LCM bytes: the RPC capture, the entry's own envelope and result files, or the rule that the millisecond close time divided by 1000 is the close time. The build also checks that the `LedgerCloseMeta` (from `getLedgers`) embeds the entry's envelope and result unchanged, and records how its meta compares with `meta.b64` (from `getTransaction`) in `checks.lcmMetaVsEntryMeta`. Meta is not part of consensus, and the two differ today: in three Soroban entries only the `core_metrics` `invoke_time_nsecs` diagnostic value differs, and in `cap38-pool-share-issuers-ordered-by-key-bytes` operation 1 (the pool deposit) has the same eight changes in both, but its first two `state`/`updated` pairs, the trustlines for the pool's two assets, come in swapped order. An indexer that compares or deduplicates meta by bytes or by change position sees two versions of the same transaction.

## Replay harness

[harness/](harness/) runs the corpus through wallet-backend's real indexer at the commits before and after each fix. Ledgers reach it through a mock RPC `getLedgers` endpoint that only listens on localhost; [harness/ADAPTER.md](harness/ADAPTER.md) is the contract for plugging in another indexer. No Docker or Postgres: the adapter calls the same `Value()` and `Scan()` that the COPY path uses, so it shows which bytes would be stored and what reads back. Full method, commits and per-commit evidence are in [harness/README.md](harness/README.md).

| Sighting | Pre-fix commit | Fixed commit |
| --- | --- | --- |
| wallet-backend#731 | `dropped` at `764a4f5b` | `pass` at `c152783c` |
| wallet-backend#729 | `dropped` at `2a0a84ea` | `pass` at `2c873353` |
| wallet-backend#716 (signed payload) | `mishandled` at `79ea2acd` | `pass` at `2c873353` and `71625b21` |
| wallet-backend#792 (`to_muxed_id` on a muxed destination) | `mishandled` at `a9bac8d2` | `pass` at `09303667` |

A note on method: every commit in that window pins go-stellar-sdk v0.7.3, which cannot decode a protocol 30 ledger header (`STELLAR_VALUE_SIGNED_MS`, CAP-0088). The go-stellar-sdk v0.8.0 changelog says versions before it cannot decode those headers, so this is expected and is not reported as a finding. The fixes were measured by bumping only that dependency to v0.8.0, on both sides of each fix.

[harness/etl/](harness/etl/) is the second adapter. It runs every corpus ledger, served by the same mock `getLedgers`, through stellar-etl's own export functions at a pinned commit and checks the JSON lines and Parquet structs they produce. 6 entries pass. `cap73` is `dropped` and `cap67` `mishandled` in the `effects` table, both stellar-etl#460 (open); `cap46-scmap-keys-collide-after-native-conversion` is `mishandled` in the legacy `parameters_decoded` column, which #330 complemented with an exact column. CI fails if any outcome changes.

## Generator

[tools/generator/](tools/generator/) rebuilds entries on a local stellar-core 30.0.0 (image pinned by digest, standalone validator, port on 127.0.0.1 only, memory, CPU and pid limits, keys generated in memory), once under the testnet passphrase and once under the mainnet passphrase, with no network contacted. `compare.py` checks each regenerated entry against the committed one: shapes, union arms and every expected value, skipping only values that depend on generated keys, ledger or time. Today it covers `cap40` and `cap27`: 108 checks match, 6 are skipped, 0 differ, and the transaction hash only verifies under its own passphrase. CI repeats it on every corpus change.

## stellar-core `--check-lcm` goldens

stellar-core's `--check-lcm` mode ([stellar-core#5435](https://github.com/stellar/stellar-core/pull/5435)) re-runs core's own unit tests and compares the `LedgerCloseMeta` they produce with golden files checked into core under `test-lcm-current/`. It takes no outside ledger as input, and a testnet ledger can never be byte-equal to a golden made on core's test network, so [tools/check_lcm_goldens.py](tools/check_lcm_goldens.py) checks what agreement can mean for captured entries: every golden at a pinned core commit (866 files, 15,306 ledgers, protocol 30) decodes and re-encodes byte-identical with the same rs-stellar-xdr 30.0.0 the corpus uses, and each manifest shape is searched for in core's protocol 30 golden transactions with the same predicate that must match the entry's own transaction. A shape `agrees` when core emits it at the same place in the meta, is `elsewhere` when core emits it only at other places, is `no-golden` when core's CI does not exercise it yet, and is `no-predicate` until a predicate is written. The run fails on any decode, round-trip, index or tx-hash pairing error, or when an entry's own transaction lacks a shape it lists. Results are committed in [corpus/check-lcm.json](corpus/check-lcm.json) and copied into each entry's `checks.checkLcmGolden` by `tools/build_manifest.py`; CI ([check-lcm.yml](.github/workflows/check-lcm.yml)) fails if that file differs from a fresh run. Locally: `cargo build --release --locked --manifest-path tools/lcm-goldens/Cargo.toml && python tools/check_lcm_goldens.py --write && python tools/build_manifest.py`. Current results (2026-11-13), 32 shapes over 9 entries: 13 agree, 1 elsewhere, 18 no golden.

| Entry | agrees | elsewhere | no-golden |
| --- | --- | --- | --- |
| `cap40-signed-payload-signer-max-payload` | 1 (`set-options-signer`) | 0 | 2 |
| `cap27-muxed-source-and-destination-id-extremes` | 0 | 0 | 4 |
| `cap73-sac-created-trustline` | 3 | 0 | 0 |
| `cap67-sac-transfer-to-muxed-address-max-id` | 3 | 0 | 1 (`muxed-id-max-u64`) |
| `cap38-pool-share-issuers-ordered-by-key-bytes` | 2 | 0 | 1 (issuer order) |
| `cap46-scmap-keys-collide-after-native-conversion` | 1 (`contract-event-map-data`) | 0 | 3 |
| `cap46-nested-authorization-contract-calls-sac` | 3 | 0 | 0 |
| `cap21-vectors-at-max-length` | 0 | 0 | 6 |
| `cap06-manage-buy-offer-update-by-id` | 0 | 1 (`offer-id-in-envelope`: the goldens carry a non-zero offer ID only in ManageSellOffer) | 1 |

## Run it

```bash
# Byte round trip and manifest check (Rust + Python)
cargo build --release --locked --manifest-path tools/roundtrip/Cargo.toml
pip install "jsonschema>=4.18,<5"
python tools/build_manifest.py --check
python tools/check_schema.py

# Linter tests (Go)
cd linter && go test ./...

# Capture new entries on testnet (Node, ephemeral keys funded by friendbot)
cd tools && npm ci && node capture-testnet.mjs cap40
```

## Layout

| Path | Content |
| --- | --- |
| `schema/` | Manifest JSON Schema |
| `corpus/` | Entries and manifest |
| `tools/capture-testnet.mjs` | Builds, submits and captures each shape on testnet |
| `tools/contracts/probe/` | Minimal Soroban contract the capture script deploys for the map and nested-authorization entries |
| `tools/roundtrip/` | `xdr-roundtrip`: decode, re-encode and JSON output with rs-stellar-xdr |
| `tools/build_manifest.py`, `tools/check_schema.py` | Manifest generation and validation |
| `harness/` | Replay harness, the wallet-backend reference adapter and the stellar-etl adapter (`harness/etl/`) |
| `tools/generator/` | Regenerates entries on a local stellar-core under both passphrases and compares them with the corpus |
| `conformance/` | Conformance runner for JS, Go, Rust and Python, sighting checks and the divergence matrix |
| `linter/` | `xdrlint` (go/analysis) |
| `guide/` | Indexer-hardening guide outline |

## License

Apache-2.0. See [LICENSE](LICENSE).
