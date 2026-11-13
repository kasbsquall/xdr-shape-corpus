# wallet-backend #729 against a real Postgres

The [reference adapter](../README.md) shows that at `2a0a84ea` (parent of the #729 merge) the `Value()` that `StateChangeModel.BatchCopy` hands to pgx fails for the `signer_account_id` of entry `cap40-signed-payload-signer-max-payload` (a signer with a 64-byte signed payload). What happens next (the COPY fails, the persist transaction rolls back, ingestion stops at that ledger) was taken from the description of PR #729.

This harness observes it. It runs wallet-backend's own `migrate up` and `ingest` commands, unchanged, against TimescaleDB, at `2a0a84ea` and at the #729 merge `2c873353`, and records the database and the ingest logs before and after.

## Result

| Commit | Role | Outcome | What was observed |
| --- | --- | --- | --- |
| `2a0a84ea` | pre-fix | **dropped, ingestion halted** | Two starts of `wallet-backend ingest`. Each starts at ledger 5699703, makes 5 persist attempts (4 retry warnings, backoff 1, 2, 4, 8 s) and exits with code 2. Every attempt fails with the error below. `latest_ingest_ledger` stays at 5699702, every table has the same row count as before the runs, and no row has `ledger_number` 5699703. |
| `2c873353` | fixed | **pass** | One start. Ledger 5699703 commits on the first attempt: 15 `transactions`, 15 `operations`, 38 `state_changes`, 38 `transactions_accounts`, 37 `operations_accounts`, 16 `native_balances`, 3 `trustline_assets`, 4 `trustline_balances` rows. `latest_ingest_ledger` goes 5699702 -> 5699703. The `SIGNER/ADD` row for `GCV2KRRA...GU2PT` stores 101 bytes and reads back through `StateChangeModel.BatchGetByAccountAddress` as `PCV2KRRA...FYTC2`, equal to `signedPayloadSigner` in `capture.json`. |

Error at `2a0a84ea`, identical on every attempt of both runs:

```
persisting ledger data for ledger 5699703: running atomic function in RunInTransaction: inserting processed data
into db for ledger 5699703: batch inserting state changes: pgx CopyFrom state_changes: ERROR: COPY from stdin
failed: converting signer_account_id: converting address to bytes: stellar address PCV2KRRA...FYTC2 has a 100-byte
key payload; the account_id column stores only 32-byte account/contract keys (SQLSTATE 57014)
```

So the halt that the reference adapter infers is what happens: the ledger is not skipped, nothing from it is committed, the cursor does not advance, the process exits after its retries, and a restarted process resumes at the same ledger and fails the same way. Three details the adapter could not show:

- pgx reports the client-side encoding error as a server error with SQLSTATE 57014 (`query_canceled`, from the COPY being aborted). `isPermanentPersistError` at `2a0a84ea` treats only classes 22, 23 and 42 as permanent, so the deterministic failure goes through the whole retry ladder (5 attempts, about 15 s of backoff) before the process exits. #729 adds `ErrRowEncoding` and marks it permanent.
- The process does not retry forever: after the fifth attempt `cmd/root.go` panics and the process exits with code 2. Recovery depends on whatever restarts it, and each restart fails at the same ledger.
- The rest of the ledger is held back with it: the fixed commit stores 15 transactions for ledger 5699703, the pre-fix commit none, and the loop never moves to the next ledger (`currentLedger++` runs only after a successful persist, `internal/services/ingest_live.go:489`).

Both commits pin go-stellar-sdk v0.7.3, which cannot decode protocol 30 ledger headers (see the [harness README](../README.md#prior-finding-the-pinned-xdr-cannot-read-protocol-30-ledgers)). As in the bumped-SDK mode of the existing harness, `run.sh` runs `go mod edit -require=github.com/stellar/go-stellar-sdk@v0.8.0` in each checkout ([diff](../sdk-bump-v0.8.0.go.mod.diff)) and changes nothing else in wallet-backend. The observation is for both commits with that dependency raised.

Machine-readable result: [`results.json`](results.json). Per commit, with snapshots before and after, parsed log lines per run and read-back: [`out/`](out/).

## Method

Option (a) of the plan was feasible, so it is what runs: wallet-backend's real ingestion, configured for RPC, not a call into its persist function.

1. Start TimescaleDB `timescale/timescaledb:2.28.2-pg17` pinned by digest, the image and tag wallet-backend's `docker-compose.yaml` and `.github/workflows/go.yaml` use at both commits.
2. For each commit: build the wallet-backend binary and the `xdrkitpg` observer as static linux binaries, create a fresh database with the `timescaledb` extension and the two settings `docker-compose.yaml` starts Postgres with (`timescaledb.enable_chunk_skipping`, `timescaledb.enable_sparse_index_bloom`, set per database here), then run `wallet-backend migrate up` (all schema migrations plus protocol registration).
3. `xdrkitpg -mode seed` sets `latest_ingest_ledger` and `oldest_ingest_ledger` to 5699702 with `IngestStoreModel.Update` inside `db.RunInTransaction`, and snapshots every table's row count. This stands for a deployment that has already ingested up to the previous ledger. Without it, live ingestion starts by populating state from a history-archive checkpoint (`startLiveIngestion`, ingest_live.go:302-314), which needs the full testnet archive.
4. Run `wallet-backend ingest` with `LEDGER_BACKEND_TYPE=rpc`, `RPC_URL` pointing at `harness/mockrpc` (serving the corpus over `getLedgers`), `NETWORK=testnet`, `LOG_LEVEL=INFO`, and `ARCHIVE_URL=http://127.0.0.1:9/`, a closed port: the archive is never contacted when the cursor is set, and if it were, the run would fail instead of reaching the network. Everything else is the command's default (live mode, `GET_LEDGERS_LIMIT` 10).
5. Watch `latest_ingest_ledger`. If it reaches 5699703, stop the process with SIGTERM (wallet-backend logs `shutdown requested; exiting cleanly`). The mock serves only corpus ledgers, so the next one, 5699704, does not exist; without the stop, ingest would keep retrying that fetch. If the process exits by itself, start it once more to see where it resumes. Each run has a 240 s limit.
6. `xdrkitpg -mode report` snapshots again, reads the signer back through `StateChangeModel.BatchGetByAccountAddress` (category `SIGNER`; the GraphQL account resolver calls it at `internal/serve/graphql/resolvers/account.resolvers.go:155` at `2c873353`) and through a raw `SELECT` scanned with the column's Go type, parses the ingest logs and classifies:
   - `halted`: cursor unchanged, every row count unchanged, no row for the ledger, and no `Ingested ledger 5699703` log line;
   - `advanced`: cursor at the ledger and the signer read back;
   - `advanced-without-row`: cursor at the ledger but no signer row (silent loss; not observed).

## wallet-backend source, cited

Same line numbers at `2a0a84ea` and `2c873353` unless both are given. `internal/ingest/ingest.go`, `internal/services/ingest.go`, `internal/db/db.go`, `internal/data/ingest_store.go` and `internal/db/migrations/` are identical between the two commits.

| What | Where |
| --- | --- |
| `ingest` command calls `ingest.Ingest` | `cmd/ingest.go:200` |
| A command error panics (exit code 2) | `cmd/root.go:35` |
| `migrate up`: schema migrations, then protocol registration | `cmd/migrate.go:59-76`; `internal/db/migrate.go` (`Migrate`, sql-migrate over `internal/db/migrations/*.sql`) |
| `ingest_store` table (the cursors) | `internal/db/migrations/2024-05-22.0-ingest.sql` |
| `state_changes.signer_account_id BYTEA` | `internal/db/migrations/2025-06-10.4-statechanges.sql:27` |
| `Ingest`, `setupDeps`, hypertable settings in live mode, ingest service construction | `internal/ingest/ingest.go:117-143`, `161`, `172-176`, `276-300` |
| RPC ledger backend (`LedgerBackendType` `rpc`) | `internal/ingest/ledger_backend.go:89-96` |
| `Run` dispatches live mode to `startLiveIngestion` | `internal/services/ingest.go:222-231` |
| Live start: read `latest_ingest_ledger`, start at cursor + 1, checkpoint bootstrap only when it is 0 | `internal/services/ingest_live.go:296`, `301`, `302-314` |
| Live loop: fetch, process, persist with retry, advance | `internal/services/ingest_live.go:376-491` (persist call 461-465, `currentLedger++` 489) |
| Persist transaction: everything for a ledger, then the cursor, in one `db.RunInTransaction` | `internal/services/ingest_live.go:42-247` (transaction 52, `insertIntoDB` 211, `UpdateGuarded` on the cursor 232-238) |
| `db.RunInTransaction`: rollback on any error, commit otherwise | `internal/db/db.go:101-122` |
| Retry ladder: 5 attempts, exponential backoff | `internal/services/ingest_live.go:26`, `659-704` |
| Permanent errors: SQLSTATE classes 22, 23, 42 (pre-fix) | `internal/services/ingest_live.go:719-733` (`2a0a84ea`) |
| ... plus `ErrRowEncoding` (fix) | `internal/services/ingest_live.go:721-724` (`2c873353`) |
| `insertIntoDB` -> `insertStateChanges` -> `StateChangeModel.BatchCopy` | `internal/services/ingest.go:259-277`, `304-314` |
| `BatchCopy` COPY of `state_changes`, `signer_account_id` converted in the row callback | `internal/data/statechanges.go:146-240` (CopyFrom 158, signer 183-186) at `2a0a84ea` |
| ... converted before COPY and wrapped in `ErrRowEncoding` | `internal/data/statechanges.go:152-205`, `236` at `2c873353` |
| Signer conversion via `NullAddressBytea.Value()` | `internal/data/query_utils.go:78-90` (`2a0a84ea`) |
| `AddressBytea.Value()` rejects payloads other than 32 bytes (the error text above) | `internal/indexer/types/types.go:84-102` (`2a0a84ea`) |
| `SignerKeyBytea` stores the full key (#729) | `internal/indexer/types/types.go:157-246` (`2c873353`) |
| Cursor read/write | `internal/data/ingest_store.go:19` (`latest_ingest_ledger`), `45` (`Get`), `106` (`Update`), `140` (`UpdateGuarded`) |

## Files

| File | Content |
| --- | --- |
| `run.sh` | Starts Postgres (or uses `PG_CONTAINER`), builds, migrates, seeds, runs `ingest`, reports, cleans up. |
| `go/xdrkitpg/main.go` | Observer. `run.sh` copies it to `internal/xdrkitpg/main.go` in each checkout. It never ingests; it seeds the cursors, takes snapshots and reads back with wallet-backend's data layer. |
| `aggregate.py` | Merges `out/*.json` into `results.json`. |
| `compare_results.py` | Fails if a fresh `results.json` differs from the committed one (everything observed except free text and timestamps). |
| `out/<label>-<sha>-sdk-v0.8.0.json` | Per commit: snapshots before/after (cursors, row count of every table, rows of the ledger, SIGNER rows), each ingest run (exit code, how it stopped, start ledger, persist attempts with their error, final error), read-back, outcome. The raw ingest logs are written next to them as `*.log`, which `.gitignore` excludes. |

## Run

```bash
bash harness/postgres/run.sh
```

Needs Docker, git, bash, Python 3 and the Go toolchain `run.sh` finds like `../run.sh` does (portable one in `tools-local`, else `PATH`). The first run pulls the image and the Go modules. A run took about 2 min 20 s here; about 36 s of it are the two pre-fix starts, mostly retry backoff.

`PG_CONTAINER=<id> PG_PASSWORD=<pw> bash harness/postgres/run.sh` uses an existing TimescaleDB container instead of starting one.

## Isolation and secrets

- Postgres publishes no port. mockrpc, `migrate`, `ingest` and the observer run as static binaries in containers that join the Postgres container's network namespace (`--network container:<pg>`), so all of them, including wallet-backend's ingest health server on `:8002`, are reachable only inside that namespace.
- The Postgres password is 32 random bytes from `/dev/urandom`, generated per run, written with mode 0600 to env files in a 0700 temp dir that is deleted on exit, and passed to containers with `--env-file`. It is never printed. Each ingest log is checked for it before being copied out.
- The image's `pg_hba.conf` trusts loopback (`127.0.0.1/32`, `::1/128`) and requires `scram-sha-256` for every other address. The harness connects over loopback inside the namespace, so the password guards the container's bridge interface, not those connections.
- Every container has `--memory 1g --memory-swap 1g --cpus 1 --pids-limit 512` and a name starting with `xdrkit-pg-<random>`. A trap removes them on exit, including on failure. TimescaleDB telemetry is off.

## CI

[`.github/workflows/harness-postgres.yml`](../../.github/workflows/harness-postgres.yml) runs the same script on `ubuntu-24.04`, which starts its own container exactly as it does locally (same image pinned by digest, password generated for the run, resource limits, no ports). The job fails if `compare_results.py` finds any difference from the committed `results.json`, and a last step fails if an `xdrkit-pg-` container is left.

## Limits

- One entry and two commits. The other #729-era commits and entries are covered without a database by the reference adapter.
- The cursor is seeded instead of bootstrapped from a checkpoint, so balance tables start empty. That changes what `native_balances` and the trustline tables hold after the fixed run, not the state-change path #729 is about.
- The mock serves only corpus ledgers. What the ingest loop does after 5699703 at the fixed commit (it starts fetching 5699704) is not part of the result.
- go-stellar-sdk is raised to v0.8.0 on both commits, as in the bumped-SDK mode of the reference adapter.
