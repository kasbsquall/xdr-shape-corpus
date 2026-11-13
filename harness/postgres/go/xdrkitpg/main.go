// Command xdrkitpg is the database side of the xdr-kit Postgres harness for
// stellar/wallet-backend. It does not ingest anything itself: ingestion is done by
// wallet-backend's own `ingest` command, started by harness/postgres/run.sh. This program
// only prepares and reads the database, using wallet-backend's own data layer.
//
// It must live inside the wallet-backend module (internal packages), at
// internal/xdrkitpg/main.go, like the reference adapter. Modes:
//
//	seed      set the latest/oldest ingest cursors to (entry ledger - 1) with
//	          IngestStoreModel.Update inside db.RunInTransaction, then snapshot
//	snapshot  write row counts, cursors and the entry's SIGNER rows as JSON
//	report    take a final snapshot, read the signer back through
//	          StateChangeModel.BatchGetByAccountAddress (the API read path), parse the
//	          ingest logs and write the per-commit result
//
// The database URL is read from DATABASE_URL, the variable wallet-backend itself reads.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stellar/wallet-backend/internal/data"
	"github.com/stellar/wallet-backend/internal/db"
	"github.com/stellar/wallet-backend/internal/indexer/types"
	"github.com/stellar/wallet-backend/internal/metrics"
)

// oldestCursorName is the default of wallet-backend's --oldest-ledger-cursor-name (cmd/ingest.go).
const oldestCursorName = "oldest_ingest_ledger"

type entryInfo struct {
	ID       string
	Ledger   uint32
	Account  string // source account of the signer-setting operation
	Expected string // capture.json signedPayloadSigner
}

type signerRow struct {
	Reason      string `json:"reason"`
	Account     string `json:"account"`
	Signer      string `json:"signer"`
	StoredBytes int    `json:"storedBytes"`
	ScanError   string `json:"scanError,omitempty"`
}

type snapshot struct {
	TakenAt    string            `json:"takenAt"`
	Cursors    map[string]uint32 `json:"cursors"`
	TableRows  map[string]int64  `json:"tableRows"`
	LedgerRows map[string]int64  `json:"ledgerRows"` // rows with ledger_number = entry ledger
	SignerRows []signerRow       `json:"signerRows"` // SIGNER state changes in the entry ledger, raw SELECT
}

type persistAttempt struct {
	Ledger  uint32 `json:"ledger"`
	Attempt int    `json:"attempt"`
	Of      int    `json:"of"`
	Error   string `json:"error"`
}

type ingestRun struct {
	Run             int              `json:"run"`
	ExitCode        int              `json:"exitCode"`
	StoppedBy       string           `json:"stoppedBy"`
	StartedAtLedger []uint32         `json:"startedAtLedger"`
	IngestedLedgers []uint32         `json:"ingestedLedgers"`
	PersistAttempts []persistAttempt `json:"persistAttempts"`               // retry warnings logged before each backoff
	AttemptsMade    int              `json:"persistAttemptsMade,omitempty"` // from the final "failed after N attempts" error
	FinalError      string           `json:"finalError,omitempty"`
	ShutdownRequest bool             `json:"shutdownRequested"`
	LogFile         string           `json:"logFile"`
}

type readBack struct {
	Via         string   `json:"via"`
	Account     string   `json:"account"`
	Expected    string   `json:"expected"`
	Signers     []string `json:"signers"`
	ExactMatch  bool     `json:"exactMatch"`
	Error       string   `json:"error,omitempty"`
	StoredBytes []int    `json:"storedBytes"`
}

type result struct {
	Label            string      `json:"label"`
	Commit           string      `json:"commit"`
	Module           string      `json:"module"`
	GoStellarSDK     string      `json:"goStellarSdk"`
	PostgresImage    string      `json:"postgresImage"`
	Entry            string      `json:"entry"`
	Ledger           uint32      `json:"ledger"`
	Outcome          string      `json:"outcome"`   // RFP vocabulary: pass | mishandled | dropped
	Ingestion        string      `json:"ingestion"` // halted | advanced | advanced-without-row | unexpected
	Detail           string      `json:"detail"`
	Before           snapshot    `json:"before"`
	After            snapshot    `json:"after"`
	Runs             []ingestRun `json:"runs"`
	ReadBack         *readBack   `json:"readBack,omitempty"`
	RollbackObserved bool        `json:"rollbackObserved"`
}

func main() {
	mode := flag.String("mode", "", "seed | snapshot | report")
	corpus := flag.String("corpus", "", "path to xdr-kit/corpus")
	entryID := flag.String("entry", "cap40-signed-payload-signer-max-payload", "corpus entry id")
	out := flag.String("out", "", "output JSON file")
	before := flag.String("before", "", "report: snapshot JSON taken before ingestion")
	runs := flag.String("runs", "", "report: TSV file, one line per ingest run: run, exit code, stopped by, log file")
	label := flag.String("label", "", "report: commit label")
	commit := flag.String("commit", "", "report: full commit sha")
	image := flag.String("image", "", "report: Postgres image used")
	flag.Parse()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" || *corpus == "" || *mode == "" {
		fail(fmt.Errorf("DATABASE_URL, -corpus and -mode are required"))
	}
	e, err := loadEntry(*corpus, *entryID)
	must(err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := db.OpenDBConnectionPool(ctx, dbURL)
	must(err)
	defer pool.Close()
	models, err := data.NewModels(pool, metrics.NewMetrics(prometheus.NewRegistry()).DB)
	must(err)

	switch *mode {
	case "seed":
		// Emulates a deployment that has already ingested up to ledger-1, so live ingestion
		// resumes at the entry ledger instead of bootstrapping from a history-archive checkpoint.
		must(db.RunInTransaction(ctx, pool, func(tx pgx.Tx) error {
			if err := models.IngestStore.Update(ctx, tx, data.LatestLedgerCursorName, e.Ledger-1); err != nil {
				return err
			}
			return models.IngestStore.Update(ctx, tx, oldestCursorName, e.Ledger-1)
		}))
		writeJSON(*out, takeSnapshot(ctx, pool, models, e))
	case "snapshot":
		writeJSON(*out, takeSnapshot(ctx, pool, models, e))
	case "report":
		var b snapshot
		raw, err := os.ReadFile(*before)
		must(err)
		must(json.Unmarshal(raw, &b))
		r := result{Label: *label, Commit: *commit, PostgresImage: *image, Entry: e.ID, Ledger: e.Ledger, Before: b}
		if bi, ok := debug.ReadBuildInfo(); ok {
			r.Module = bi.Main.Path
			for _, d := range bi.Deps {
				if d.Path == "github.com/stellar/go-stellar-sdk" {
					r.GoStellarSDK = d.Version
				}
			}
		}
		r.After = takeSnapshot(ctx, pool, models, e)
		r.Runs = parseRuns(*runs)
		if r.After.Cursors[data.LatestLedgerCursorName] >= e.Ledger {
			r.ReadBack = readBackSigner(ctx, models, e, r.After.SignerRows)
		}
		classify(&r, e)
		writeJSON(*out, r)
		fmt.Printf("%s %s: outcome=%s ingestion=%s cursor %d -> %d\n  %s\n", r.Label, r.Commit, r.Outcome, r.Ingestion,
			r.Before.Cursors[data.LatestLedgerCursorName], r.After.Cursors[data.LatestLedgerCursorName], r.Detail)
	default:
		fail(fmt.Errorf("unknown -mode %q", *mode))
	}
}

func loadEntry(corpus, id string) (entryInfo, error) {
	var m struct {
		Entries []struct {
			ID     string `json:"id"`
			Source struct {
				Ledger uint32 `json:"ledger"`
			} `json:"source"`
		} `json:"entries"`
	}
	raw, err := os.ReadFile(filepath.Join(corpus, "manifest.json"))
	if err != nil {
		return entryInfo{}, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return entryInfo{}, err
	}
	e := entryInfo{ID: id}
	for _, me := range m.Entries {
		if me.ID == id {
			e.Ledger = me.Source.Ledger
		}
	}
	if e.Ledger == 0 {
		return e, fmt.Errorf("entry %s not in manifest", id)
	}
	dir := filepath.Join(corpus, "entries", id)
	var capture struct {
		SignedPayloadSigner string `json:"signedPayloadSigner"`
	}
	raw, err = os.ReadFile(filepath.Join(dir, "capture.json"))
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		return e, err
	}
	e.Expected = capture.SignedPayloadSigner
	env, err := os.ReadFile(filepath.Join(dir, "envelope.b64"))
	if err != nil {
		return e, err
	}
	var te xdr.TransactionEnvelope
	if err := xdr.SafeUnmarshalBase64(strings.TrimSpace(string(env)), &te); err != nil {
		return e, err
	}
	src := te.SourceAccount()
	if ops := te.Operations(); len(ops) > 0 && ops[0].SourceAccount != nil {
		src = *ops[0].SourceAccount
	}
	e.Account = src.ToAccountId().Address()
	return e, nil
}

func takeSnapshot(ctx context.Context, pool *pgxpool.Pool, models *data.Models, e entryInfo) snapshot {
	s := snapshot{TakenAt: time.Now().UTC().Format(time.RFC3339), Cursors: map[string]uint32{}, TableRows: map[string]int64{}, LedgerRows: map[string]int64{}}
	for _, k := range []string{data.LatestLedgerCursorName, oldestCursorName} {
		v, err := models.IngestStore.Get(ctx, k)
		must(err)
		s.Cursors[k] = v
	}
	rows, err := pool.Query(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' ORDER BY table_name`)
	must(err)
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	must(err)
	for _, t := range tables {
		var n int64
		must(pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{t}.Sanitize()).Scan(&n))
		s.TableRows[t] = n
	}
	for _, t := range []string{"transactions", "operations", "state_changes"} {
		var n int64
		must(pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{t}.Sanitize()+" WHERE ledger_number = $1", int32(e.Ledger)).Scan(&n))
		s.LedgerRows[t] = n
	}
	// Raw read of the stored bytes, scanned with the column's own Go type.
	signerType := reflect.TypeOf(types.StateChange{}).Field(fieldIndex("SignerAccountID")).Type
	q := `SELECT state_change_reason, account_id, signer_account_id, coalesce(octet_length(signer_account_id), 0)
		FROM state_changes WHERE ledger_number = $1 AND state_change_category = $2 ORDER BY state_change_id`
	rows, err = pool.Query(ctx, q, int32(e.Ledger), string(types.StateChangeCategorySigner))
	must(err)
	defer rows.Close()
	s.SignerRows = []signerRow{}
	for rows.Next() {
		var reason sql.NullString
		var account types.AddressBytea
		var rawSigner []byte
		var n int
		must(rows.Scan(&reason, &account, &rawSigner, &n))
		r := signerRow{Reason: reason.String, Account: string(account), StoredBytes: n}
		sv := reflect.New(signerType)
		if err := sv.Interface().(sql.Scanner).Scan(rawSigner); err != nil {
			r.ScanError = err.Error()
		} else {
			r.Signer = stringOf(sv.Elem())
		}
		s.SignerRows = append(s.SignerRows, r)
	}
	must(rows.Err())
	return s
}

// readBackSigner reads the signer back through the same model call the GraphQL account
// state-changes resolver uses.
func readBackSigner(ctx context.Context, models *data.Models, e entryInfo, raw []signerRow) *readBack {
	rb := &readBack{Via: "StateChangeModel.BatchGetByAccountAddress (category SIGNER)", Account: e.Account, Expected: e.Expected, Signers: []string{}}
	for _, r := range raw {
		rb.StoredBytes = append(rb.StoredBytes, r.StoredBytes)
	}
	cat := string(types.StateChangeCategorySigner)
	limit := int32(100)
	got, err := models.StateChanges.BatchGetByAccountAddress(ctx, e.Account, nil, nil, &cat, nil, "", &limit, nil, data.ASC, nil)
	if err != nil {
		rb.Error = err.Error()
		return rb
	}
	for _, sc := range got {
		if sc.LedgerNumber != e.Ledger {
			continue
		}
		s := stringOf(reflect.ValueOf(sc.StateChange).FieldByName("SignerAccountID"))
		rb.Signers = append(rb.Signers, s)
		if s == e.Expected {
			rb.ExactMatch = true
		}
	}
	return rb
}

func classify(r *result, e entryInfo) {
	latestBefore := r.Before.Cursors[data.LatestLedgerCursorName]
	latestAfter := r.After.Cursors[data.LatestLedgerCursorName]
	var ledgerRows int64
	for _, n := range r.After.LedgerRows {
		ledgerRows += n
	}
	sameTables := reflect.DeepEqual(r.Before.TableRows, r.After.TableRows)
	r.RollbackObserved = latestAfter == latestBefore && ledgerRows == 0 && sameTables && len(r.After.SignerRows) == 0

	ingested := false
	for _, run := range r.Runs {
		for _, l := range run.IngestedLedgers {
			if l == e.Ledger {
				ingested = true
			}
		}
	}

	switch {
	case latestAfter < e.Ledger && r.RollbackObserved && !ingested:
		r.Outcome, r.Ingestion = "dropped", "halted"
		var attempts []string
		for _, run := range r.Runs {
			attempts = append(attempts, strconv.Itoa(run.AttemptsMade))
		}
		last := ""
		if n := len(r.Runs); n > 0 {
			last = r.Runs[n-1].FinalError
		}
		r.Detail = fmt.Sprintf("ledger %d never commits: %d run(s) of `wallet-backend ingest` each started at %d and failed to persist it (persist attempts per run: %s); every table has the same row count as before, no row has ledger_number %d, and latest_ingest_ledger stays at %d. Last error: %s",
			e.Ledger, len(r.Runs), e.Ledger, strings.Join(attempts, ", "), e.Ledger, latestAfter, last)
	case latestAfter >= e.Ledger && r.ReadBack != nil && r.ReadBack.ExactMatch:
		r.Outcome, r.Ingestion = "pass", "advanced"
		r.Detail = fmt.Sprintf("ledger %d commits (%d transactions, %d operations, %d state_changes rows), latest_ingest_ledger %d -> %d, and the SIGNER row reads back through the API query as the full P key (%v stored bytes)",
			e.Ledger, r.After.LedgerRows["transactions"], r.After.LedgerRows["operations"], r.After.LedgerRows["state_changes"], latestBefore, latestAfter, r.ReadBack.StoredBytes)
	case latestAfter >= e.Ledger && (r.ReadBack == nil || len(r.ReadBack.Signers) == 0):
		r.Outcome, r.Ingestion = "dropped", "advanced-without-row"
		r.Detail = fmt.Sprintf("latest_ingest_ledger advanced to %d but no SIGNER row for %s is readable for ledger %d: the ledger was skipped without its signer (silent loss)", latestAfter, e.Account, e.Ledger)
	case latestAfter >= e.Ledger:
		r.Outcome, r.Ingestion = "mishandled", "advanced"
		r.Detail = fmt.Sprintf("ledger %d commits but the SIGNER row reads back as %v, not %s", e.Ledger, r.ReadBack.Signers, e.Expected)
	default:
		r.Outcome, r.Ingestion = "unexpected", "unexpected"
		r.Detail = fmt.Sprintf("cursor %d -> %d, %d rows for ledger %d, tables unchanged: %v", latestBefore, latestAfter, ledgerRows, e.Ledger, sameTables)
	}
}

var (
	reStart   = regexp.MustCompile(`Starting ingestion from ledger: (\d+)`)
	reRetry   = regexp.MustCompile(`Error ingesting data for ledger (\d+) \(attempt (\d+)/(\d+)\): (.*), retrying in `)
	reDone    = regexp.MustCompile(`Ingested ledger (\d+) in `)
	reRunErr  = regexp.MustCompile(`running 'ingest' from .*`)
	reAfter   = regexp.MustCompile(`failed after (\d+) attempts`)
	reMsg     = regexp.MustCompile(`msg="((?:[^"\\]|\\.)*)"`)
	reTimeHdr = regexp.MustCompile(`^time="[^"]*" `)
)

func parseRuns(path string) []ingestRun {
	f, err := os.Open(path)
	must(err)
	defer f.Close()
	var out []ingestRun
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) != 4 {
			continue
		}
		run := ingestRun{StoppedBy: parts[2], LogFile: filepath.Base(parts[3]), StartedAtLedger: []uint32{}, IngestedLedgers: []uint32{}, PersistAttempts: []persistAttempt{}}
		run.Run, _ = strconv.Atoi(parts[0])
		run.ExitCode, _ = strconv.Atoi(parts[1])
		parseLog(parts[3], &run)
		out = append(out, run)
	}
	must(sc.Err())
	return out
}

func parseLog(path string, run *ingestRun) {
	f, err := os.Open(path)
	must(err)
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		msg := line
		if m := reMsg.FindStringSubmatch(line); m != nil {
			msg = strings.ReplaceAll(m[1], `\"`, `"`)
		}
		if m := reStart.FindStringSubmatch(msg); m != nil {
			run.StartedAtLedger = append(run.StartedAtLedger, atou(m[1]))
		}
		if m := reRetry.FindStringSubmatch(msg); m != nil {
			a, _ := strconv.Atoi(m[2])
			of, _ := strconv.Atoi(m[3])
			run.PersistAttempts = append(run.PersistAttempts, persistAttempt{Ledger: atou(m[1]), Attempt: a, Of: of, Error: m[4]})
		}
		if m := reDone.FindStringSubmatch(msg); m != nil {
			run.IngestedLedgers = append(run.IngestedLedgers, atou(m[1]))
		}
		if strings.Contains(msg, "shutdown requested; exiting cleanly") {
			run.ShutdownRequest = true
		}
		if m := reRunErr.FindString(reTimeHdr.ReplaceAllString(msg, "")); m != "" {
			run.FinalError = m
			if a := reAfter.FindStringSubmatch(m); a != nil {
				run.AttemptsMade, _ = strconv.Atoi(a[1])
			}
		}
	}
	must(sc.Err())
	sort.Slice(run.IngestedLedgers, func(i, j int) bool { return run.IngestedLedgers[i] < run.IngestedLedgers[j] })
}

func fieldIndex(name string) int {
	f, ok := reflect.TypeOf(types.StateChange{}).FieldByName(name)
	if !ok || len(f.Index) != 1 {
		fail(fmt.Errorf("types.StateChange has no direct field %s", name))
	}
	return f.Index[0]
}

func stringOf(v reflect.Value) string {
	if !v.IsValid() {
		return ""
	}
	if s, ok := v.Interface().(fmt.Stringer); ok {
		return s.String()
	}
	if v.Kind() == reflect.String {
		return v.String()
	}
	return ""
}

func atou(s string) uint32 {
	v, _ := strconv.ParseUint(s, 10, 32)
	return uint32(v)
}

func writeJSON(path string, v any) {
	enc, err := json.MarshalIndent(v, "", "  ")
	must(err)
	if path == "" {
		fmt.Println(string(enc))
		return
	}
	must(os.WriteFile(path, append(enc, '\n'), 0o644))
}

func must(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "xdrkitpg:", err)
	os.Exit(1)
}
