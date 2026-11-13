# Replay harness: reference adapter for stellar/wallet-backend

The harness takes real corpus entries (XDR emitted by stellar-core, captured on testnet at protocol 30) and runs them through the `stellar/wallet-backend` indexer at commits before and after each fix. For each entry it returns a verdict in the RFP's vocabulary: `pass`, `mishandled` or `dropped`.

It targets the RFP acceptance check for the replay harness: the reference adapter reproduces wallet-backend #729, #731 and #716 on pre-fix commits and passes on the fixed ones. It also reproduces the muxed-destination gap that wallet-backend #792 closed.

## What the adapter does

Input: the entry's ledger, served by the harness mock RPC over `getLedgers` (see below), plus `capture.json` and the expected values from `manifest.json`. The adapter contract is in [ADAPTER.md](ADAPTER.md).

1. Fetches the entry's ledger with go-stellar-sdk's `RPCLedgerBackend`, the backend wallet-backend's ingest uses with `LedgerBackendType` `rpc`, and decodes the full `LedgerCloseMeta` with the Go XDR package pinned by the commit (`github.com/stellar/go-stellar-sdk/xdr`).
2. Extracts the transaction by hash with `ingest.NewLedgerTransactionReaderFromLedgerCloseMeta` and the testnet passphrase.
3. Processes it with wallet-backend's real indexer (`indexer.NewIndexer` + `ProcessLedgerTransactions` over an in-memory `IndexerBuffer`). All processors run: participants, effects, SAC events, token transfer, trustlines, accounts.
4. For every state change and participant, calls the same `Value()` (driver.Valuer) that `BatchCopy` hands to pgx before COPY, then the `Scan()` that reads the column back. This shows which bytes would be stored and what would be read back, without a database.
5. Applies the shape-specific check for the entry:
   - `cap73-sac-created-trustline`: a `TRUSTLINE/ADD` state change must exist for the trustor in `capture.json`.
   - `cap40-signed-payload-signer-max-payload`: a `SIGNER` state change must exist whose `signer_account_id` is the expected `P...` key from the manifest, and the key must survive the round trip.
   - `cap27-muxed-source-and-destination-id-extremes`: the debit of the source base account and the credit of the destination base account must exist, and every stored address must read back.
   - `cap67-sac-transfer-to-muxed-address-max-id`: the debit of the sender and the credit of the destination base account must exist, and the credit must keep `to_muxed_id` = 18446744073709551615. A credit without it is `mishandled`: the payment lands, but the muxed destination it was sent to is lost.

| Verdict | Meaning |
| --- | --- |
| `pass` | The shape is indexed and every stored address reads back intact (or reduced to its base G account, wallet-backend's documented choice for M addresses). |
| `mishandled` | The shape is indexed but a stored value is altered or cannot be read back. |
| `dropped` | The shape never reaches storage: no row is produced, the ledger cannot be decoded or processed, or the row fails to encode (COPY and the persist transaction fail and ingestion stops at that ledger). |

## Files

| File | Content |
| --- | --- |
| `ADAPTER.md` | Adapter interface: what the harness provides, what an adapter outputs, checklist for another indexer. |
| `mockrpc/` | Mock Stellar RPC serving the corpus over `getLedgers`, `getLatestLedger` and `getHealth`, with tests. |
| `go/xdrkitreplay/main.go` | Canonical copy of the adapter. `run.sh` copies it to `internal/xdrkitreplay/main.go` in each checkout, because `internal` packages can only be imported from inside the module. |
| `run.sh` | Starts `mockrpc`, creates one `git worktree` per commit, copies the adapter in and runs it against the mock. |
| `aggregate.py` | Merges the runs in `out/` into `results.json`. |
| `out/<label>-<sha>-<mode>.json` | Full per-commit result with evidence (state changes, per-column round trip). |
| `results.json` | Machine-readable result per (sighting, entry, commit). |
| `sdk-bump-v0.8.0.go.mod.diff` | Exact `go.mod` change used in the bumped-SDK mode. |

## Run

```bash
bash run.sh                      # every commit with its pinned go-stellar-sdk
SDK=v0.8.0 bash run.sh           # same commits, bumping ONLY go-stellar-sdk to v0.8.0
bash run.sh 79ea2acd c152783c    # selected commits
INPUT=file bash run.sh           # adapter reads the .b64 files instead of the mock RPC
python aggregate.py              # writes results.json
(cd mockrpc && go test ./...)    # mock RPC tests
```

No Docker or Postgres needed. Network access is needed the first time for `go mod download`; after that the run is offline.

## Ledger input: mock `getLedgers`

`run.sh` builds `mockrpc`, starts it on `127.0.0.1` with a free port, and passes its URL to the adapter with `-rpc`. The mock serves each corpus `LedgerCloseMeta` as a Stellar RPC `getLedgers` item (`hash`, `sequence`, `ledgerCloseTime`, `headerXdr`, `metadataXdr`), using the request and response types of go-stellar-sdk `protocols/rpc`. The adapter fetches each entry's ledger through `RPCLedgerBackend`, which calls `getHealth` and then `getLedgers`. The field mapping and the mock's limits are in [ADAPTER.md](ADAPTER.md).

`mockrpc`'s tests walk the corpus page by page with go-stellar-sdk's RPC client, check every field against the corpus files and against the close times testnet RPC returned at capture, pin the JSON field names, read every ledger through `RPCLedgerBackend`, and check that invalid requests and non-loopback addresses are refused.

Every commit in `run.sh` gives the same outcome per entry over `getLedgers` as from the files, in both dependency modes. The runs in `out/` record `"ledgerSource": "getLedgers"`.

## Commits

| Label | Commit | Why |
| --- | --- | --- |
| `pre-716` | `79ea2acd1dc65a07166133cc04b12e76db314db0` | Merge of #715 (2026-08-27). #716 was opened while reviewing #715; in this tree `AddressBytea.Value()` silently copies only the first 32 payload bytes. |
| `pre-729` | `2a0a84ea12523b6c92e278c0c84b443041240095` | Parent of the #729 merge. Already includes #693, which makes `AddressBytea.Value()` reject payloads other than 32 bytes. |
| `fix-729` | `2c873353c1f0f3c9b145e25a7d009023ad104a1c` | Merge of #729 (`SignerKeyBytea`). |
| `pre-731` | `764a4f5bcbff5abb0949d93da37f769063013ea0` | Parent of the merge of #734, the PR that closed #731. |
| `fix-731` | `c152783c054c765c8b51fd26ed313fd1ebc1c50c` | Merge of #734. |
| `close-716` | `71625b21f832478aece23c2212ddf458c8a2f134` | Tip of `main` when #716 was closed (2026-09-18T20:09Z). |
| `pre-792` | `a9bac8d2f59f7590d393c3e72e67d95b7cb2fba1` | Base of #792 (2026-10-29). Already pins go-stellar-sdk v0.8.0, so the pinned mode decodes protocol 30. |
| `fix-792` | `09303667545a1effb09c779ff29d271c6c812de3` | Last commit of #792, which records `to_muxed_id` on SAC and classic credits. |
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
| #792 | `cap67-sac-transfer-to-muxed-address-max-id` | `a9bac8d2` | pre-fix | **mishandled**: credit recorded for the base G account, `to_muxed_id` not kept | **mishandled** (same) |
| #792 | `cap67-sac-transfer-to-muxed-address-max-id` | `09303667` | fixed | **pass**: credit carries `to_muxed_id` 18446744073709551615 | **pass** |

On `main` (`3e22515a`, already on v0.8.0) all four entries pass. The six earlier commits, run with the bumped SDK, are also `mishandled` on the `cap67` entry, since they predate #792.

Against the RFP check, in bumped-SDK mode:
- **#731: reproduced.** `dropped` at `764a4f5b`, `pass` at `c152783c`.
- **#729: reproduced.** `dropped` at `2a0a84ea`, `pass` at `2c873353`.
- **#792: reproduced in both modes, with no change at all to wallet-backend.** `mishandled` at `a9bac8d2`, `pass` at `09303667`. The PR description says that without the change "the id is dropped and clients cannot show the muxed destination", and that "the same gap already existed for `M...` destinations".
- **#716: reproduced for signed-payload keys.** `mishandled` at `79ea2acd` with the symptom the issue describes (silent truncation to 32 bytes, unreadable value), `pass` at `2c873353` and `71625b21`. The same entry shows the whole evolution: `mishandled`, then `dropped` (`2a0a84ea`), then `pass`.

## Not reproduced yet

- **#716 with muxed addresses.** Neither muxed entry puts an `M...` value into an address column at any commit. The `cap27` entry is a classic payment, and the classic path reduces source and destination to their G account first. The `cap67` entry sends a SAC `transfer()` to a muxed `ScAddress`; core emits the event with the base G account in the topic and the id in `to_muxed_id`, so the column truncation in #716 is not reached there either. js-stellar-sdk refuses to build a Soroban transaction with a muxed source account ("Soroban transactions cannot use a muxed source account"); whether stellar-core would accept one built by hand was not tested. The remaining candidate is a custom SEP-41 token that emits an `M...` address in its own event topics.
- **Ingestion halt at `pre-729` is inferred, not observed.** The harness checks that `Value()` on `signer_account_id` returns an error, which is the call `StateChangeModel.BatchCopy` makes inside the COPY callback. That this rolls back the persist transaction and that ingestion retries the same ledger comes from the description of PR #729; Postgres was not run.
- **Column scope.** The round trip covers address columns of `state_changes` and participants in `transactions_accounts` / `operations_accounts`. Balance rows are reported as context only.
