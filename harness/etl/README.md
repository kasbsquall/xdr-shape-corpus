# Replay harness: adapter for stellar/stellar-etl

This adapter runs the corpus through the transform and export code of [stellar/stellar-etl](https://github.com/stellar/stellar-etl), the pipeline that feeds SDF's Hubble dataset, and reports `pass`, `mishandled` or `dropped` per entry with the same vocabulary as the wallet-backend adapter ([../README.md](../README.md), [../ADAPTER.md](../ADAPTER.md)).

Result on 2026-11-13: 6 entries pass, 2 are `mishandled` and 1 is `dropped`. All three non-pass results reproduce issues that are already open or were already addressed upstream (#460, #330). None is a new stellar-etl defect. One more observation is new as far as this search went: stellar-etl did not compile with Go 1.27.4 on Windows, and the same check should stop it on Linux ([below](#go-1274-cannot-build-stellar-etl-on-windows-linux-inferred)).

## Pinned versions

| Item | Value |
| --- | --- |
| stellar-etl | `2b24ec89ddce7fc0beb7795de982c93e6895eaf3` (tip of `master`, committed 2026-11-11) |
| go-stellar-sdk | `v0.8.1-0.20261106081502-e31e3b146d00` (stellar-etl's `go.mod`) |
| go-stellar-xdr-json | `v0.0.0-20261030210117-c68093a10a8a` (stellar-etl's `go.mod`) |
| Go toolchain | `go1.27.3`, the version in stellar-etl's `go.mod` and its `docker/Dockerfile` (`FROM golang:1.27.3`) |
| C compiler | needed for cgo; go-stellar-xdr-json links a prebuilt static Rust library |

## How the adapter works

[go/xdrkit_replay.go](go/xdrkit_replay.go) is compiled inside stellar-etl's own `cmd` package (run.sh copies it to `cmd/xdrkit_replay.go` in the checkout), because the per-ledger export functions it calls are unexported. For each manifest entry it:

1. Gets the entry's `LedgerCloseMeta` from `mockrpc` over `getLedgers`, through go-stellar-sdk's `RPCLedgerBackend` (`PrepareRange` over that one ledger, then `GetLedger`). Without `--rpc` it reads `ledger-close-meta.b64`. Both modes give the same outcome and detail on every entry.
2. Calls stellar-etl's own per-ledger export functions with a real output file: `processTransactions`, `processLedgerTransaction`, `processOperations`, `processEffects`, `processContractEvents`, `processTokenTransfers` and `processTrades` (`cmd/export_*.go`). Each one runs the `internal/input` reader and the `internal/transform` function and writes JSON lines with `ExportEntry`, exactly as the export commands do. The adapter reads the lines back with `json.Decoder.UseNumber`, so every integer is compared as written.
3. Runs the ledger-entry path: `input.ExtractBatch` (what `input.StreamChanges` calls per batch) over a one-ledger backend, then the same `Transform*` dispatch as `cmd/export_ledger_entry_changes.go` lines 111-272, with every output type enabled, written with `ExportEntry`. This dispatch is re-implemented in the adapter, call for call, because it lives inside the cobra `Run` closure, which builds a GCS backend.
4. For rows that the export also writes to Parquet, records the value of `ToParquet()`, which is what `WriteParquet` hands to the Parquet writer.
5. Keeps the rows of the entry's transaction (by `transaction_hash`, by `transaction_id`, by operation ID, or for `ledger_transaction` by `tx_envelope`). Ledger-entry rows have no transaction column, so it keeps those that mention an identifier from `capture.json` or a source account of the transaction.
6. Compares the fields that matter for the entry's shapes with `manifest.json` and `capture.json`.

Each check is `exact`, `altered` or `missing`. A missing check makes the entry `dropped`, an altered one makes it `mishandled`, and the entry is `pass` only when every check is exact. `perExport` in each result gives the verdict per table, so a table that keeps a shape stays visible even when another one loses it.

Differences from a production run:

- **Ledger source.** stellar-etl reads ledgers from a GCS datastore or from captive core (`internal/utils/main.go`, `CreateLedgerBackend`) and has no RPC backend. Both hand the export functions an `xdr.LedgerCloseMeta`, which is the only input used from step 2 on. So the ledger reaches stellar-etl's code through go-stellar-sdk's RPC backend, which stellar-etl does not use.
- **No BigQuery or GCS.** The checks read the JSON lines stellar-etl writes. How BigQuery types those columns is not checked here.
- **Parquet** is checked as the `ToParquet()` struct. No Parquet file is written or read back.
- **`StrictExport` is off**, so every export reports attempts and failures. No export logged an error or a failure on any corpus ledger (recorded per entry under `exports`).

## What is checked

| Entry | Checks |
| --- | --- |
| `cap40-signed-payload-signer-max-payload` | `signers.signer` and weight, Parquet `Signer`, `operations` set_options `signer_key`, `effects` signer_created `public_key`: all equal to the full `P…` key with its 64-byte payload |
| `cap27-muxed-source-and-destination-id-extremes` | `transactions.account_muxed`; `operations.source_account_muxed`, `from_muxed`, `from_muxed_id` = 18446744073709551615, `to_muxed`, `to_muxed_id` = 0, `amount`; the same ids in the Parquet `OperationDetails` string; `effects.address_muxed` on credit and debit; `token_transfers.to_muxed` and `to_muxed_id` |
| `cap73-sac-created-trustline` | `trustlines` row for the trustor with issuer, `trust_line_limit` = 9223372036854775807 (JSON and Parquet) and `ledger_entry_change` = created; a `trustline_created` effect |
| `cap67-sac-transfer-to-muxed-address-max-id` | `token_transfers` from, to, `to_muxed`, `to_muxed_id` = 18446744073709551615, `amount_raw`; `contract_events.data_decoded` `to_muxed_id` u64 and the topic address; `operations` `parameters_json_decoded` `to` argument; `effects` account_credited address and `address_muxed` |
| `cap38-pool-share-issuers-ordered-by-key-bytes` | `liquidity_pools` pool ID (hex and `L…`), `asset_a_issuer` / `asset_b_issuer` in key-byte order; pool-share `trustlines` row; `operations` change_trust and liquidity_pool_deposit pool ID and reserve order; `effects` liquidity_pool_deposited pool ID |
| `cap46-scmap-keys-collide-after-native-conversion` | `contract_events.data_decoded` map equal to the manifest value (three entries); the map argument in `operations` `parameters_json_decoded` and in `parameters_decoded` |
| `cap46-nested-authorization-contract-calls-sac` | `transactions.tx_envelope` and `ledger_transaction.tx_envelope` byte-equal to `envelope.b64` (the only place the authorization tree is kept); operation `contract_id` and function `pay`; `token_transfers` row for the nested SAC transfer from the authorizer to the recipient |
| `cap21-vectors-at-max-length` | `transactions.operation_count` = 100, 28-byte `memo`, both `extra_signers` (JSON and Parquet); 100 operation rows; name and 64-byte value of operation 100; 100 `data_created` effects |
| `cap06-manage-buy-offer-update-by-id` | `operations` manage_buy_offer `offer_id` = 971392; `offers` row with that `offer_id`, seller and `ledger_entry_change` = updated |

For every entry the adapter also records whether `ledger_transaction.tx_envelope`, `ledger_transaction.tx_meta` and `transactions.tx_envelope` are byte-equal to the corpus files, and compares `token_transfers.amount` with `amount_raw`.

## Run

```bash
bash harness/etl/run.sh                     # mockrpc on 127.0.0.1, getLedgers
INPUT=file bash harness/etl/run.sh          # read ledger-close-meta.b64 instead
ENTRY=cap73-sac-created-trustline bash harness/etl/run.sh   # one entry, to a temp dir
ETL_GO=go1.27.4 bash harness/etl/run.sh     # reproduces the build failure below
```

The script shallow-fetches stellar-etl at the pinned commit into `tools-local/src/stellar-etl` if it is not there, copies the adapter in and runs it with `GOTOOLCHAIN=go1.27.3`. The first run needs network for the fetch, `go mod download` and the go1.27.3 toolchain. Nothing listens anywhere except `mockrpc` on 127.0.0.1.

Output: `out/<entry>.json`, with the facts, the per-table verdicts and the matching rows exactly as written (JSON and `ToParquet()`), plus `results.json`, the summary.

## Results

stellar-etl `2b24ec89`, go1.27.3, ledgers over `getLedgers`:

| Entry | Outcome | Stage | Tables |
| --- | --- | --- | --- |
| `cap40-signed-payload-signer-max-payload` | pass | | signers, operations, effects exact |
| `cap27-muxed-source-and-destination-id-extremes` | pass | | transactions, operations, effects, token_transfers exact |
| `cap73-sac-created-trustline` | **dropped** | effects | trustlines exact; effects has no row for the transaction |
| `cap67-sac-transfer-to-muxed-address-max-id` | **mishandled** | effects | token_transfers, contract_events, operations exact; effects credit has `address_muxed` null |
| `cap38-pool-share-issuers-ordered-by-key-bytes` | pass | | liquidity_pools, trustlines, operations, effects exact |
| `cap46-scmap-keys-collide-after-native-conversion` | **mishandled** | operations | contract_events and `parameters_json_decoded` exact; legacy `parameters_decoded` prints all three keys as `1` |
| `cap46-nested-authorization-contract-calls-sac` | pass | | envelope byte-equal; operations and token_transfers exact; no decoded column for the authorization tree |
| `cap21-vectors-at-max-length` | pass | | transactions, operations, effects exact |
| `cap06-manage-buy-offer-update-by-id` | pass | | operations, offers exact |

All integers wider than 53 bits in these entries come through exactly in the JSON lines and in `ToParquet()`: the u64-max muxed IDs (as JSON integers in operation details, as strings in `token_transfers.to_muxed_id` and as XDR-JSON strings in `contract_events`), `trust_line_limit` = i64::MAX and every `amount_raw`.

### `cap73-sac-created-trustline`: dropped in effects

Observed: the trustline created by the SAC `trust()` call is in the `trustlines` export (account, issuer, limit 9223372036854775807, `ledger_entry_change` 0). `processEffects` writes no row at all for the transaction, and logs no error.

Cause, at `2b24ec89`: `internal/transform/effects.go` lines 124-135. For `InvokeHostFunction` the only effects come from contract events (`addInvokeHostFunctionEffects`), and `trust()` emits none. The comment there says so ("For now, the only effects are related to the events themselves").

Known: [stellar/stellar-etl#460](https://github.com/stellar/stellar-etl/issues/460), open since 2026-07-04, describes exactly this ("SAC trust() → trustline_created … The transform emits nothing for these"). This is the same kind of gap as wallet-backend#731 in this harness: the trustline is in the state table and absent from the history table.

Reproduce: `ENTRY=cap73-sac-created-trustline bash harness/etl/run.sh`, then look at `exports.effects.rowsForEntry` (empty) and `exports.trustlines.rowsForEntry` in the output file.

### `cap67-sac-transfer-to-muxed-address-max-id`: mishandled in effects

Observed: `token_transfers` keeps `to_muxed` = `MBQUFX…6XDY` and `to_muxed_id` = `18446744073709551615`, `contract_events.data_decoded` keeps the u64, and the `parameters_json_decoded` `to` argument is the M address. The `account_credited` effect has `address` = the base G account, `address_muxed` = null and no muxed ID in `details`. For the classic payment to an M address in `cap27`, the same table does keep `address_muxed`.

Cause, at `2b24ec89`: `internal/transform/effects.go` lines 1374-1379 add the credit with `null.String{}` as the muxed address. The parsed event cannot provide one anyway. go-stellar-sdk `support/contractevents/utils.go` (`eventAmount`, at the pinned SDK) reads only `amount` out of a CAP-67 data map, and `TransferEvent` (`transfer.go`) has no muxed field.

Known: [stellar/stellar-etl#460](https://github.com/stellar/stellar-etl/issues/460), second item ("SAC transfer/mint to muxed destinations … the transform always writes null"). It is the effects-table counterpart of wallet-backend#792.

Reproduce: `ENTRY=cap67-sac-transfer-to-muxed-address-max-id bash harness/etl/run.sh`, then `context["effects.account_credited"]` in the output file.

### `cap46-scmap-keys-collide-after-native-conversion`: mishandled in the legacy `parameters_decoded`

Observed: `contract_events.data_decoded` and `operations.details.parameters_json_decoded` keep the three keys `{"u32":1}`, `{"string":"1"}` and `{"symbol":"1"}` with values 10, 20 and 30. In the same operation row, `parameters_decoded` for that argument is `{"type": "Map", "value": "[{1 10} {1 20} {1 30}]"}`, so a reader of that column cannot tell the three keys apart. `details_json` holds the same map as `details` (`operation.go` lines 97 and 103), so it carries the same pair of columns.

Cause, at `2b24ec89`: `internal/transform/operation.go` line 2244, where `serializeParameters` sets `value` to `param.String()`, the Go `fmt` rendering of the `ScVal`. It is called at line 1093.

Known: [stellar/stellar-etl#330](https://github.com/stellar/stellar-etl/issues/330), closed 2025-06-24, which added the XDR-JSON columns `parameters_json` / `parameters_json_decoded` as the full rendering. `parameters_decoded` is still written next to them. Rated `mishandled` because that column is still exported and is lossy for this shape. The exact value is in the same row.

Reproduce: `ENTRY=cap46-scmap-keys-collide-after-native-conversion bash harness/etl/run.sh`, then `context` in the output file.

## `token_transfers.amount` precision (#454)

Observed: in 6 of the 18 `token_transfers` rows for the corpus transactions, the `amount` float64 is not the closest double to `amount_raw / 10^7`. Every case is a fee row: raw `100` is written as `0.000009999999999999999`, `200` as `0.000019999999999999998`, `7060` as `0.0007059999999999999` and `-4805` as `-0.00048049999999999997`. `amount_raw` is exact in every row. The list is in `results.json` under `amountPrecision`.

Cause, at `2b24ec89`: `internal/transform/token_transfer.go` lines 57-58 (and the same pattern up to line 78) compute `ParseFloat(amount) * 0.0000001`. Elsewhere, `utils.ConvertStroopValueToReal` (`internal/utils/main.go` lines 85-88) divides through `big.Rat` and gives `1e-05` for 100. The same stroop value can therefore get a different last digit in `token_transfers` than in operation details or effects.

Known: [stellar/stellar-etl#454](https://github.com/stellar/stellar-etl/issues/454), open, which cites the same `100 → 9.999999999999999e-06` from the unit test golden. Here it is reproduced on real ledgers. The corpus holds no amount above 2^53 stroops, so the larger loss #454 describes is not exercised.

## Go 1.27.4 cannot build stellar-etl on Windows (Linux inferred)

Observed on Windows amd64, with go1.27.4 and `CGO_ENABLED=1`:

```
github.com/stellar/go-stellar-xdr-json/xdrjson: disallowed LTO section ".llvmbc" in #cgo LDFLAGS: …\go-stellar-xdr-json@v0.0.0-20261030210117-c68093a10a8a\xdrjson\libs\x86_64-pc-windows-gnu\libxdrjson.a
```

`internal/transform/contract_events.go` imports `xdrjson`, so the transform package and every export command fail to build. go1.27.3 builds the same tree, and that is the toolchain this adapter uses.

Cause: go1.27.4's `cmd/go` scans every library under a `-L` directory named in a `#cgo LDFLAGS` line and rejects archives with LTO sections (`src/cmd/go/internal/work/security.go` in go1.27.4: `isDisallowedLTOSection` at lines 728-734, PE/COFF check at line 1119, ELF check at line 1211). The go1.27.3 toolchain has no such check. go-stellar-xdr-json's `xdrjson/conversion.go` lines 9-13 point `-L` at its prebuilt `libs/<triple>/` directories, and the archives there carry `.llvmbc` sections. A scan with the same predicate finds them in 227 of 352 members of the Windows archive, 292 of 409 of `x86_64-unknown-linux-gnu` and 285 of 503 of `aarch64-unknown-linux-gnu`.

Observed vs inferred: the failure was observed on Windows only. That Linux builds fail the same way is inferred from the archive scan above and from the ELF branch of the same check. stellar-rpc hit the same check on Linux with its own Rust libraries and fixed it in [stellar/stellar-rpc#1190](https://github.com/stellar/stellar-rpc/pull/1190) (merged 2026-11-12), which links them only after stripping the bitcode sections. That PR attributes the check to Go 1.26.11 and 1.27.4. As of 2026-11-13, go-stellar-xdr-json's latest commit is the `c68093a10a8a` that stellar-etl pins, and searches of the stellar-etl and go-stellar-xdr-json issues and PRs for "LTO", "llvmbc" and "1.27.4" found nothing. Today stellar-etl builds with 1.27.3 (go.mod, Dockerfile, CI `go-version-file`), so the break shows up when it moves to 1.27.4.

Reproduce: `ETL_GO=go1.27.4 bash harness/etl/run.sh`.

## Side observation about the corpus, not about stellar-etl

For 4 entries (`cap38`, both `cap46` and `cap73`), `ledger_transaction.tx_meta` differs from the corpus `meta.b64`, with the same length. stellar-etl is not the cause. The `meta.b64` bytes do not appear inside `ledger-close-meta.b64` for those entries, while for the other 5 they do. For the three Soroban entries the two metas are equal once diagnostic events are removed. In `cap38` the pool deposit's first two `state`/`updated` pairs come in swapped order. The manifest records this per entry as `checks.lcmMetaVsEntryMeta` (see the repository README). stellar-etl writes the meta that is inside the LedgerCloseMeta.

## Files

| File | Content |
| --- | --- |
| `go/xdrkit_replay.go` | The adapter (package `cmd` of stellar-etl) |
| `run.sh` | Fetches stellar-etl at the pinned commit, starts `mockrpc`, copies the adapter in and runs it |
| `out/<entry>.json` | Per entry: outcome, `perExport`, facts (`want`, `got`, `status`, `knownIssue`), context and the matching rows of every export (JSON lines and `ToParquet()`) |
| `results.json` | Summary per entry, versions, ledger source and the `amountPrecision` check |
