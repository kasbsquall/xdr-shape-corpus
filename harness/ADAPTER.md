# Adapter interface

An adapter connects one indexer to the replay harness. The harness serves the corpus ledgers and the expected values. The adapter runs them through the indexer's own code and reports, for each corpus entry, whether the shape came through intact.

`go/xdrkitreplay` is the reference adapter, for `stellar/wallet-backend`. Everything below describes what that adapter and `mockrpc` do today.

## What the harness provides

### Ledgers over `getLedgers`

`mockrpc` serves every `LedgerCloseMeta` listed in `corpus/manifest.json` as a Stellar RPC endpoint (JSON-RPC 2.0 over HTTP POST).

```bash
cd harness/mockrpc
go run . -corpus ../../corpus            # prints its URL, e.g. http://127.0.0.1:57208
go run . -corpus ../../corpus -addr 127.0.0.1:8000
```

It listens on a loopback IP only and refuses any other `-addr`. The URL is the first line of stdout. It needs no network.

Methods: `getLedgers`, `getLatestLedger`, `getHealth`. Requests and responses use the types in go-stellar-sdk `protocols/rpc` (v0.9.0), the package stellar-rpc's own `getLedgers` handler uses, so field names and JSON encoding match a real server:

| Field | Value served |
| --- | --- |
| request `startLedger` | first sequence wanted; must be within `oldestLedger`..`latestLedger` |
| request `pagination.cursor` | sequence of the last ledger already received; the page starts after it. Cannot be combined with `startLedger` |
| request `pagination.limit` | default 50, max 200 (stellar-rpc's `default-ledgers-limit` and `max-ledgers-limit` defaults) |
| request `xdrFormat` | `base64` or empty. `json` is rejected |
| `ledgers[].hash` | ledger hash, hex |
| `ledgers[].sequence` | ledger sequence |
| `ledgers[].ledgerCloseTime` | `scpValue.closeTime` in seconds, as a JSON string |
| `ledgers[].headerXdr` | the LCM's `LedgerHeaderHistoryEntry`, base64 |
| `ledgers[].metadataXdr` | the corpus `ledger-close-meta.b64` file, unchanged |
| `latestLedger`, `oldestLedger` | highest and lowest corpus ledger |
| `latestLedgerCloseTime`, `oldestLedgerCloseTime` | their close times, as JSON numbers |
| `cursor` | sequence of the last ledger in the page; on an empty page, the request cursor |

Differences from a real RPC server: it holds only the corpus ledgers, so sequences are sparse (a page holds the next corpus ledgers at or after the start, not every ledger in between), and it has no other methods.

Memory: at startup it keeps one index row per ledger (sequence, close time, file path). Each request reads and decodes at most `limit` ledger files.

### Expected values

`corpus/manifest.json`, one object per entry:

- `id`: entry name, also the folder `corpus/entries/<id>/`.
- `source.ledger`: the ledger sequence to request.
- `source.txHash`: the transaction the entry is about.
- `network.passphrase`: needed to hash and read the transactions.
- `expected.values`: JSON pointers into the entry's XDR with the decoded value each must have.
- `sightings`: the upstream issues and PRs this entry reproduces.

`corpus/entries/<id>/capture.json` adds capture details some checks use (for example `trustor`, `from`, `destinationAccount`, `muxedDestinationId`).

## What an adapter outputs

One JSON file per indexer build, written to `harness/out/<label>-<sha>-<mode>.json`:

| Field | Content |
| --- | --- |
| `label` | short name of the build (`pre-729`, `fix-729`, ...) |
| `vcsRevision` | full commit of the indexer |
| `module`, `goStellarSdk`, `goStellarSdkReplace` | build details the reference adapter reads from Go build info |
| `ledgerSource` | `getLedgers` or `file` |
| `entries[]` | one result per corpus entry |

Each entry result:

| Field | Content |
| --- | --- |
| `id` | manifest entry id |
| `outcome` | `pass`, `mishandled` or `dropped` |
| `stage` | where the outcome was decided. Reference adapter: `decode-ledger-close-meta`, `ingest-reader`, `indexer`, `state_changes`, `persist-encode`, `persist-roundtrip` |
| `detail` | one sentence a person can read |
| `evidence` | free-form data behind the outcome (rows produced, values compared) |
| `storedValues[]` | each address-like value the indexer would store: `where`, `column`, `goType`, `in`, `out`, `storedBytes`, `status` (`exact`, `reduced-to-base`, `altered`, `undecodable`, `encode-error`, `scan-error`), `error` |

`aggregate.py` merges these files into `results.json`. It reads `label`, `vcsRevision`, `goStellarSdk`, `ledgerSource` and, per entry, `id`, `outcome`, `stage` and `detail`.

## Outcomes

| Outcome | Meaning |
| --- | --- |
| `pass` | The shape is indexed and every stored value reads back intact (or reduced to its base G account, wallet-backend's documented choice for M addresses). |
| `mishandled` | The shape is indexed but a stored value is altered, lost or cannot be read back. |
| `dropped` | The shape never reaches storage: no row is produced, the ledger cannot be decoded or processed, or the row fails to encode. |

An indexer failure is a result: if the indexer cannot decode the ledger, the entry is `dropped`. The adapter exits with an error only when the harness side fails, for example the RPC cannot be reached or a ledger is missing.

## How the reference adapter does it

For each manifest entry:

1. Fetches ledger `source.ledger` with go-stellar-sdk's `RPCLedgerBackend` (`PrepareRange` over that one ledger, then `GetLedger`), the backend wallet-backend builds for `LedgerBackendType` `rpc` in `internal/ingest/ledger_backend.go`. `BufferSize` is 1, so it holds one ledger at a time, and the backend is closed after each entry. The XDR is decoded with the go-stellar-sdk version pinned by the commit.
2. Finds `source.txHash` with the SDK's ingest transaction reader.
3. Runs wallet-backend's indexer over it.
4. Pushes every stored address through the column type's `Value()` and `Scan()`.
5. Applies the entry's check (see README).

Run without `-rpc`, step 1 reads `ledger-close-meta.b64` instead. Both inputs give the same outcomes on every commit in `run.sh`.

## Writing an adapter for another indexer

For an indexer such as stellar-etl:

- [ ] Start `mockrpc` and point the indexer's own ledger source at its URL. Request one ledger per entry (`startLedger` = `source.ledger`): the mock's sequences are sparse, so a range that expects every ledger in between will not be satisfied.
- [ ] Use the indexer's real code path: its ledger reader, transaction reader and processors, not a copy.
- [ ] For each entry, locate `source.txHash` with `network.passphrase`.
- [ ] Take what the indexer would write, push it through its real storage encoding and read it back (or use its database).
- [ ] Compare with `expected.values` and `capture.json` and pick `pass`, `mishandled` or `dropped` as defined above. Record why in `stage`, `detail` and `evidence`.
- [ ] Turn indexer errors into `dropped`. Exit non-zero only for harness errors.
- [ ] Write one result file per indexer build with the fields above, and run the builds before and after each fix you want to measure.
