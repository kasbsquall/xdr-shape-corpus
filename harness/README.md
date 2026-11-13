# Replay harness: reference adapter for stellar/wallet-backend

The harness takes real corpus entries (XDR emitted by stellar-core, captured on testnet at protocol 30) and runs them through the `stellar/wallet-backend` indexer at commits before and after each fix. For each entry it returns a verdict in the RFP's vocabulary: `pass`, `mishandled` or `dropped`.

It targets the RFP acceptance check for the replay harness: the reference adapter reproduces wallet-backend #729, #731 and #716 on pre-fix commits and passes on the fixed ones.

## What the adapter does

Input: a `corpus/entries/<id>/` folder with `ledger-close-meta.b64`, `capture.json`, and the expected values from `manifest.json`.

1. Decodes the full `LedgerCloseMeta` with the Go XDR package pinned by the commit (`github.com/stellar/go-stellar-sdk/xdr`).
2. Extracts the transaction by hash with `ingest.NewLedgerTransactionReaderFromLedgerCloseMeta` and the testnet passphrase.
3. Processes it with wallet-backend's real indexer (`indexer.NewIndexer` + `ProcessLedgerTransactions` over an in-memory `IndexerBuffer`). All processors run: participants, effects, SAC events, token transfer, trustlines, accounts.
4. For every state change and participant, calls the same `Value()` (driver.Valuer) that `BatchCopy` hands to pgx before COPY, then the `Scan()` that reads the column back. This shows which bytes would be stored and what would be read back, without a database.
5. Applies the shape-specific check for the entry:
   - `cap73-sac-created-trustline`: a `TRUSTLINE/ADD` state change must exist for the trustor in `capture.json`.
   - `cap40-signed-payload-signer-max-payload`: a `SIGNER` state change must exist whose `signer_account_id` is the expected `P...` key from the manifest, and the key must survive the round trip.
   - `cap27-muxed-source-and-destination-id-extremes`: the debit of the source base account and the credit of the destination base account must exist, and every stored address must read back.

| Verdict | Meaning |
| --- | --- |
| `pass` | The shape is indexed and every stored address reads back intact (or reduced to its base G account, wallet-backend's documented choice for M addresses). |
| `mishandled` | The shape is indexed but a stored value is altered or cannot be read back. |
| `dropped` | The shape never reaches storage: no row is produced, the ledger cannot be decoded or processed, or the row fails to encode (COPY and the persist transaction fail and ingestion stops at that ledger). |

## Files

| File | Content |
| --- | --- |
| `go/xdrkitreplay/main.go` | Canonical copy of the adapter. `run.sh` copies it to `internal/xdrkitreplay/main.go` in each checkout, because `internal` packages can only be imported from inside the module. |
| `run.sh` | Creates one `git worktree` per commit, copies the adapter in and runs it. |
| `aggregate.py` | Merges the runs in `out/` into `results.json`. |
| `out/<label>-<sha>-<mode>.json` | Full per-commit result with evidence (state changes, per-column round trip). |
| `results.json` | Machine-readable result per (sighting, entry, commit). |
| `sdk-bump-v0.8.0.go.mod.diff` | Exact `go.mod` change used in the bumped-SDK mode. |

## Run

```bash
bash run.sh                      # every commit with its pinned go-stellar-sdk
SDK=v0.8.0 bash run.sh           # same commits, bumping ONLY go-stellar-sdk to v0.8.0
bash run.sh 79ea2acd c152783c    # selected commits
python aggregate.py              # writes results.json
```

No Docker or Postgres needed. Network access is needed the first time for `go mod download`.

## Commits

| Label | Commit | Why |
| --- | --- | --- |
| `pre-716` | `79ea2acd1dc65a07166133cc04b12e76db314db0` | Merge of #715 (2026-08-27). #716 was opened while reviewing #715; in this tree `AddressBytea.Value()` silently copies only the first 32 payload bytes. |
| `pre-729` | `2a0a84ea12523b6c92e278c0c84b443041240095` | Parent of the #729 merge. Already includes #693, which makes `AddressBytea.Value()` reject payloads other than 32 bytes. |
| `fix-729` | `2c873353c1f0f3c9b145e25a7d009023ad104a1c` | Merge of #729 (`SignerKeyBytea`). |
| `pre-731` | `764a4f5bcbff5abb0949d93da37f769063013ea0` | Parent of the merge of #734, the PR that closed #731. |
| `fix-731` | `c152783c054c765c8b51fd26ed313fd1ebc1c50c` | Merge of #734. |
| `close-716` | `71625b21f832478aece23c2212ddf458c8a2f134` | Tip of `main` when #716 was closed (2026-09-18T20:09Z). |
| `main` | `3e22515a44979fdc3a558fda3991ca2b48e38cef` | Tip of `main` on 2026-11-12, for reference. Already pins go-stellar-sdk v0.8.0. |

#716 has no linked fix PR; GitHub's timeline shows only the cross-reference from #715 and a "completed" close. For the signed-payload producer in #716, the behavior changed with #693 (truncate becomes reject) and then #729 (store the full key), so the #716 fix is measured at `fix-729` and `close-716`.

## Prior finding: the pinned XDR cannot read protocol 30 ledgers

Every commit in the window of these fixes (`79ea2acd` to `71625b21`) pins `github.com/stellar/go-stellar-sdk v0.7.3`. With that version all three entries fail to decode the `LedgerCloseMeta`:

```
decoding LedgerCloseMetaV2: decoding LedgerHeaderHistoryEntry: decoding LedgerHeader:
decoding StellarValue: decoding StellarValueExt: decoding StellarValueType:
'3' is not a valid StellarValueType enum value
```

Value 3 is `STELLAR_VALUE_SIGNED_MS`, the millisecond close-time arm from CAP-0088 that shipped with protocol 30. The envelope, result and v4 `TransactionMeta` of each entry do decode with v0.7.3 (recorded in `evidence.perTransactionXdr` in `out/*-pinned.json`); the break is only in the ledger header. An indexer left on an older XDR stops ingesting entirely at the protocol upgrade.

To measure the fixes, only the XDR dependency was bumped: `SDK=v0.8.0 bash run.sh` runs `go mod edit -require=github.com/stellar/go-stellar-sdk@v0.8.0` in each checkout. That is the minimum version with protocol 30 support and the one `main` pins today. The only direct dependency that changes is go-stellar-sdk; the other lines in the diff are indirect dependencies raised by minimal version selection. No wallet-backend code was changed, and every commit compiled with v0.8.0 unchanged. Since the same change is applied before and after each fix, the difference between them comes from wallet-backend's code.

## Results

Bumped-SDK mode = go-stellar-sdk v0.8.0 on every commit. Pinned mode = each commit's own `go.mod`.

| Sighting | Entry | Commit | Role | Pinned | Bumped SDK |
| --- | --- | --- | --- | --- | --- |
| #731 | `cap73-sac-created-trustline` | `764a4f5b` | pre-fix | dropped (LCM decode) | **dropped**: no `TRUSTLINE/ADD`; `trustline_balances` does get the ADD row with limit i64::MAX |
| #731 | `cap73-sac-created-trustline` | `c152783c` | fixed | dropped (LCM decode) | **pass**: `TRUSTLINE/ADD` and `BALANCE_AUTHORIZATION` for the trustor |
| #729 | `cap40-signed-payload-signer-max-payload` | `2a0a84ea` | pre-fix | dropped (LCM decode) | **dropped**: `signer_account_id` fails in `Value()` ("has a 100-byte key payload; the account_id column stores only 32-byte account/contract keys"), so COPY fails and ingestion stops at that ledger |
| #729 | `cap40-signed-payload-signer-max-payload` | `2c873353` | fixed | dropped (LCM decode) | **pass**: `SignerKeyBytea` stores 101 bytes and returns the full `P...` key |
| #716 | `cap40-signed-payload-signer-max-payload` | `79ea2acd` | pre-fix | dropped (LCM decode) | **mishandled**: 33 bytes stored, reads back as `PCV2KRRAOWNTUUXH7CSB24JBUOIJ5UJ74JZEHGVBBRFC7FXBEY5GUDF6`, which does not decode |
| #716 | `cap40-signed-payload-signer-max-payload` | `2c873353` | fixed | dropped (LCM decode) | **pass** |
| #716 | `cap40-signed-payload-signer-max-payload` | `71625b21` | fixed (close) | dropped (LCM decode) | **pass** |
| #716 | `cap27-muxed-source-and-destination-id-extremes` | `79ea2acd` | pre-fix | dropped (LCM decode) | pass |
| #716 | `cap27-muxed-source-and-destination-id-extremes` | `2c873353` | fixed | dropped (LCM decode) | pass |
| #716 | `cap27-muxed-source-and-destination-id-extremes` | `71625b21` | fixed (close) | dropped (LCM decode) | pass |

On `main` (`3e22515a`, already on v0.8.0) all three entries pass.

Against the RFP check, in bumped-SDK mode:
- **#731: reproduced.** `dropped` at `764a4f5b`, `pass` at `c152783c`.
- **#729: reproduced.** `dropped` at `2a0a84ea`, `pass` at `2c873353`.
- **#716: reproduced for signed-payload keys.** `mishandled` at `79ea2acd` with the symptom the issue describes (silent truncation to 32 bytes, unreadable value), `pass` at `2c873353` and `71625b21`. The same entry shows the whole evolution: `mishandled`, then `dropped` (`2a0a84ea`), then `pass`.

## Not reproduced yet

- **#716 with muxed addresses.** The `cap27` entry is a classic payment, and wallet-backend's classic path reduces source and destination to their G account before any column; no `M...` value reaches an address column at any commit. The muxed producers named in #716 are Soroban operations with a muxed source and muxed `ScAddress` in SEP-41 events. A corpus entry with that shape (for example `ExtendFootprintTtl` with a muxed source account) is the next capture.
- **Ingestion halt at `pre-729` is inferred, not observed.** The harness checks that `Value()` on `signer_account_id` returns an error, which is the call `StateChangeModel.BatchCopy` makes inside the COPY callback. That this rolls back the persist transaction and that ingestion retries the same ledger comes from the description of PR #729; Postgres was not run.
- **Column scope.** The round trip covers address columns of `state_changes` and participants in `transactions_accounts` / `operations_accounts`. Balance rows are reported as context only.
