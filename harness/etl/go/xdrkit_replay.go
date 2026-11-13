// xdrkit_replay is the xdr-kit replay adapter for stellar/stellar-etl.
//
// It must be compiled inside stellar-etl's own `cmd` package, because it calls the
// unexported per-ledger export functions (processTransactions, processOperations,
// ...) and the JSON writer (ExportEntry) that every export command uses. run.sh
// copies this file to <stellar-etl>/cmd/xdrkit_replay.go and runs:
//
//	go run . xdrkit_replay --corpus <xdr-kit/corpus> --rpc <mock RPC URL> --out <dir> --commit <sha>
//
// Per corpus entry:
//
//  1. Gets the entry's LedgerCloseMeta from the harness mock RPC (getLedgers) through
//     go-stellar-sdk's RPCLedgerBackend, or from ledger-close-meta.b64 without --rpc.
//     stellar-etl itself reads ledgers from a GCS datastore or captive core
//     (internal/utils/main.go CreateLedgerBackend); both hand the export commands
//     the same xdr.LedgerCloseMeta value, which is the only input used from here on.
//  2. Runs that ledger through stellar-etl's real export path:
//     - processTransactions, processLedgerTransaction, processOperations,
//     processEffects, processContractEvents, processTokenTransfers, processTrades
//     (cmd/export_*.go), each writing JSON lines with ExportEntry to a temp file
//     exactly as the export commands do; the lines are read back with UseNumber.
//     - ledger entry changes: input.ExtractBatch (the function StreamChanges calls)
//     over a one-ledger backend, then the same Transform* dispatch that
//     cmd/export_ledger_entry_changes.go runs, written with ExportEntry.
//     - for rows the export would also write to Parquet, the ToParquet() value.
//  3. Keeps the rows that belong to the entry's transaction and applies the
//     shape-specific checks against manifest.json and capture.json.
//
// Outcomes use the harness vocabulary: pass, mishandled, dropped.
package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
	"github.com/stellar/go-stellar-sdk/toid"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stellar/stellar-etl/v2/internal/input"
	"github.com/stellar/stellar-etl/v2/internal/transform"
	"github.com/stellar/stellar-etl/v2/internal/utils"
)

var xkCmd = &cobra.Command{
	Use:    "xdrkit_replay",
	Short:  "xdr-kit harness adapter: replays the corpus through stellar-etl's export path",
	Hidden: true,
	Run:    xkRun,
}

func init() {
	rootCmd.AddCommand(xkCmd)
	xkCmd.Flags().String("corpus", "", "path to xdr-kit/corpus")
	xkCmd.Flags().String("rpc", "", "mock RPC URL serving the corpus over getLedgers (default: read the .b64 files)")
	xkCmd.Flags().String("out", "", "output directory (per-entry JSON files and results.json)")
	xkCmd.Flags().String("commit", "", "stellar-etl commit under test")
	xkCmd.Flags().String("entry", "", "run only this entry id")
}

// ---------------------------------------------------------------- types

type xkManifest struct {
	Entries []struct {
		ID      string   `json:"id"`
		Shapes  []string `json:"shapes"`
		Network struct {
			Passphrase string `json:"passphrase"`
		} `json:"network"`
		Source struct {
			Ledger uint32 `json:"ledger"`
			TxHash string `json:"txHash"`
		} `json:"source"`
		Expected struct {
			Values []struct {
				Pointer string          `json:"pointer"`
				Value   json.RawMessage `json:"value"`
			} `json:"values"`
		} `json:"expected"`
	} `json:"entries"`
}

type xkFact struct {
	Export   string `json:"export"`
	Check    string `json:"check"`
	Want     string `json:"want"`
	Got      string `json:"got"`
	Status   string `json:"status"` // exact | altered | missing
	Required bool   `json:"required"`
	Ref      string `json:"knownIssue,omitempty"`
}

type xkExport struct {
	Attempts        int               `json:"attempts"`
	Failures        int               `json:"failures"`
	Errors          []string          `json:"errors,omitempty"`
	Fatal           string            `json:"fatal,omitempty"`
	RowsInLedger    int               `json:"rowsInLedger"`
	Rows            []json.RawMessage `json:"rowsForEntry,omitempty"`
	ParquetRows     []json.RawMessage `json:"parquetRowsForEntry,omitempty"`
	rows            []map[string]any
	parquet         []map[string]any
	parquetPossible bool
}

type xkEntryResult struct {
	ID           string               `json:"id"`
	Outcome      string               `json:"outcome"`
	Stage        string               `json:"stage"`
	Detail       string               `json:"detail"`
	Shapes       []string             `json:"shapes"`
	Ledger       uint32               `json:"ledger"`
	TxHash       string               `json:"txHash"`
	TxID         int64                `json:"txId"`
	LedgerSource string               `json:"ledgerSource"`
	PerExport    map[string]string    `json:"perExport"`
	Facts        []xkFact             `json:"facts"`
	Context      map[string]any       `json:"context,omitempty"`
	Exports      map[string]*xkExport `json:"exports,omitempty"`
}

type xkResults struct {
	SchemaVersion      string            `json:"schemaVersion"`
	Adapter            string            `json:"adapter"`
	OutcomeVocabulary  []string          `json:"outcomeVocabulary"`
	Indexer            string            `json:"indexer"`
	Commit             string            `json:"commit"`
	GoVersion          string            `json:"goVersion"`
	GoStellarSDK       string            `json:"goStellarSdk"`
	GoStellarXDRJSON   string            `json:"goStellarXdrJson"`
	LedgerSource       string            `json:"ledgerSource"`
	GeneratedAt        string            `json:"generatedAt"`
	Entries            []xkResultSummary `json:"entries"`
	Outcomes           map[string]string `json:"outcomes"`
	StrictExportNote   string            `json:"strictExportNote"`
	AmountPrecision    map[string]any    `json:"amountPrecision"`
	EntryFilesLocation string            `json:"entryFiles"`
}

type xkResultSummary struct {
	ID        string            `json:"id"`
	Outcome   string            `json:"outcome"`
	Stage     string            `json:"stage"`
	Detail    string            `json:"detail"`
	PerExport map[string]string `json:"perExport"`
}

// xkFatal is raised (as a panic) when stellar-etl's logger calls Fatal, so one
// entry's fatal error becomes that entry's result instead of ending the run.
type xkFatal struct{ msg string }

// ---------------------------------------------------------------- driver

func xkRun(cmd *cobra.Command, _ []string) {
	corpus, _ := cmd.Flags().GetString("corpus")
	rpcURL, _ := cmd.Flags().GetString("rpc")
	outDir, _ := cmd.Flags().GetString("out")
	commit, _ := cmd.Flags().GetString("commit")
	only, _ := cmd.Flags().GetString("entry")
	if corpus == "" || outDir == "" {
		xkDie(errors.New("--corpus and --out are required"))
	}
	raw, err := os.ReadFile(filepath.Join(corpus, "manifest.json"))
	xkDie(err)
	var m xkManifest
	xkDie(json.Unmarshal(raw, &m))
	xkDie(os.MkdirAll(filepath.Join(outDir, "out"), 0o755))

	// Capture every error stellar-etl logs, and turn Fatal into a recoverable panic.
	hook := &xkHook{}
	cmdLogger.AddHook(hook)
	cmdLogger.SetOutput(io.Discard)
	cmdLogger.SetExitFunc(func(int) { panic(xkFatal{msg: hook.last()}) })

	res := xkResults{
		SchemaVersion:      "0.1",
		Adapter:            "xdr-kit/harness/etl/go/xdrkit_replay.go (stellar-etl cmd process* functions + ExportEntry, ledger entry changes via input.ExtractBatch + Transform*, no GCS or BigQuery)",
		OutcomeVocabulary:  []string{"pass", "mishandled", "dropped"},
		Indexer:            "stellar/stellar-etl",
		Commit:             commit,
		LedgerSource:       "file",
		GeneratedAt:        time.Now().UTC().Format(time.RFC3339),
		Outcomes:           map[string]string{},
		StrictExportNote:   "Runs with StrictExport=false so every export reports attempts/failures; with --strict-export any logged transform error would be fatal for the whole export.",
		EntryFilesLocation: "out/<entry-id>.json",
	}
	if rpcURL != "" {
		res.LedgerSource = "getLedgers"
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		res.GoVersion = bi.GoVersion
		for _, d := range bi.Deps {
			switch d.Path {
			case "github.com/stellar/go-stellar-sdk":
				res.GoStellarSDK = d.Version
			case "github.com/stellar/go-stellar-xdr-json":
				res.GoStellarXDRJSON = d.Version
			}
		}
	}

	precRows := 0
	var precBad []map[string]any
	for _, e := range m.Entries {
		if only != "" && e.ID != only {
			continue
		}
		dir := filepath.Join(corpus, "entries", e.ID)
		expected := map[string]json.RawMessage{}
		for _, v := range e.Expected.Values {
			expected[v.Pointer] = v.Value
		}
		r := xkEntry(rpcURL, dir, e.ID, e.Shapes, e.Source.Ledger, e.Network.Passphrase, e.Source.TxHash, expected, hook)
		r.LedgerSource = res.LedgerSource
		enc, err := json.MarshalIndent(r, "", "  ")
		xkDie(err)
		xkDie(os.WriteFile(filepath.Join(outDir, "out", e.ID+".json"), append(enc, '\n'), 0o644))
		res.Entries = append(res.Entries, xkResultSummary{ID: r.ID, Outcome: r.Outcome, Stage: r.Stage, Detail: r.Detail, PerExport: r.PerExport})
		if rows, ok := r.Context["amountPrecision"].([]map[string]any); ok {
			for _, row := range rows {
				precRows++
				if row["amountEqualsRawOver1e7"] != true {
					row["entry"] = r.ID
					precBad = append(precBad, row)
				}
			}
		}
		res.Outcomes[r.ID] = r.Outcome
		fmt.Printf("%-48s %-10s %s\n", r.ID, r.Outcome, r.Detail)
	}
	res.AmountPrecision = map[string]any{
		"check":         "token_transfers.amount (float64) read back from the JSON line equals amount_raw / 10^7 exactly",
		"rows":          precRows,
		"notExact":      len(precBad),
		"notExactRows":  precBad,
		"knownIssue":    "stellar/stellar-etl#454 (open)",
		"integerFields": "amount_raw, to_muxed_id, muxed ids in operation details, trust_line_limit and contract-event u64/i128 values are exact in every entry (see facts)",
	}
	enc, err := json.MarshalIndent(res, "", "  ")
	xkDie(err)
	xkDie(os.WriteFile(filepath.Join(outDir, "results.json"), append(enc, '\n'), 0o644))
}

type xkHook struct{ msgs []string }

func (h *xkHook) Levels() []logrus.Level {
	return []logrus.Level{logrus.ErrorLevel, logrus.FatalLevel, logrus.PanicLevel}
}
func (h *xkHook) Fire(e *logrus.Entry) error { h.msgs = append(h.msgs, e.Message); return nil }
func (h *xkHook) take() []string             { m := h.msgs; h.msgs = nil; return m }
func (h *xkHook) last() string {
	if len(h.msgs) == 0 {
		return "fatal"
	}
	return h.msgs[len(h.msgs)-1]
}

// ---------------------------------------------------------------- per entry

func xkEntry(rpcURL, dir, id string, shapes []string, seq uint32, passphrase, txHash string, expected map[string]json.RawMessage, hook *xkHook) xkEntryResult {
	r := xkEntryResult{ID: id, Shapes: shapes, Ledger: seq, TxHash: txHash, Context: map[string]any{}, Exports: map[string]*xkExport{}}

	lcm, err := xkGetLedger(rpcURL, dir, seq)
	if err != nil {
		r.Outcome, r.Stage, r.Detail = "dropped", "decode-ledger-close-meta", "the LedgerCloseMeta does not decode: "+err.Error()
		return r
	}

	// Locate the transaction the way input.TransactionsFromLedger does.
	reader, err := ingest.NewLedgerTransactionReaderFromLedgerCloseMeta(passphrase, lcm)
	if err != nil {
		r.Outcome, r.Stage, r.Detail = "dropped", "ingest-reader", "stellar-etl's transaction reader rejects the ledger: "+err.Error()
		return r
	}
	var tx *ingest.LedgerTransaction
	for {
		t, rerr := reader.Read()
		if rerr != nil {
			break
		}
		if t.Hash.HexString() == txHash {
			tt := t
			tx = &tt
			break
		}
	}
	reader.Close()
	if tx == nil {
		r.Outcome, r.Stage, r.Detail = "dropped", "ingest-reader", "transaction hash not found in the ledger"
		return r
	}
	r.TxID = toid.New(int32(seq), int32(tx.Index), 0).ToInt64()
	env := utils.EnvironmentDetails{NetworkPassphrase: passphrase, Network: "testnet"}

	envelopeB64 := xkReadTrim(filepath.Join(dir, "envelope.b64"))
	metaB64 := xkReadTrim(filepath.Join(dir, "meta.b64"))
	sameTx := func(op int64) bool { return op>>12 == r.TxID>>12 }

	type proc struct {
		name   string
		fn     processLedgerFunc
		belong func(map[string]any) bool
	}
	procs := []proc{
		{"transactions", processTransactions, func(row map[string]any) bool { return xkStr(row["transaction_hash"]) == txHash }},
		{"ledger_transaction", processLedgerTransaction, func(row map[string]any) bool { return xkStr(row["tx_envelope"]) == envelopeB64 }},
		{"operations", processOperations, func(row map[string]any) bool { return xkInt(row["transaction_id"]) == r.TxID }},
		{"effects", processEffects, func(row map[string]any) bool { return sameTx(xkInt(row["operation_id"])) }},
		{"contract_events", processContractEvents, func(row map[string]any) bool { return xkStr(row["transaction_hash"]) == txHash }},
		{"token_transfers", processTokenTransfers, func(row map[string]any) bool { return xkStr(row["transaction_hash"]) == txHash }},
		{"trades", processTrades, func(row map[string]any) bool { return sameTx(xkInt(row["history_operation_id"])) }},
	}
	for _, p := range procs {
		r.Exports[p.name] = xkRunProcess(p.name, p.fn, lcm, env, p.belong, hook)
	}

	// Ledger-entry rows have no transaction column: keep those that mention an
	// identifier from capture.json or a source account of this transaction.
	interest := xkInterest(dir)
	interest = append(interest, tx.Envelope.SourceAccount().ToAccountId().Address())
	for _, op := range tx.Envelope.Operations() {
		if op.SourceAccount != nil {
			interest = append(interest, op.SourceAccount.ToAccountId().Address())
		}
	}
	r.Context["ledgerEntryRowFilter"] = interest
	for name, ex := range xkRunChanges(lcm, env, interest, hook) {
		r.Exports[name] = ex
	}

	// Lossless copies of the transaction, reported for every entry.
	if lt := xkFirst(r.Exports["ledger_transaction"]); lt != nil {
		r.Context["ledger_transaction.tx_envelope == envelope.b64"] = xkStr(lt["tx_envelope"]) == envelopeB64
		r.Context["ledger_transaction.tx_meta == meta.b64"] = xkStr(lt["tx_meta"]) == metaB64
	}
	if t := xkFirst(r.Exports["transactions"]); t != nil {
		r.Context["transactions.tx_envelope == envelope.b64"] = xkStr(t["tx_envelope"]) == envelopeB64
	}
	r.Context["amountPrecision"] = xkAmountPrecision(r.Exports["token_transfers"])

	c := &xkChecker{r: &r, dir: dir, expected: expected}
	switch id {
	case "cap40-signed-payload-signer-max-payload":
		c.signedPayloadSigner()
	case "cap27-muxed-source-and-destination-id-extremes":
		c.muxedPayment()
	case "cap73-sac-created-trustline":
		c.sacTrustline()
	case "cap67-sac-transfer-to-muxed-address-max-id":
		c.sacTransferToMuxed()
	case "cap38-pool-share-issuers-ordered-by-key-bytes":
		c.poolShare()
	case "cap46-scmap-keys-collide-after-native-conversion":
		c.scMapCollision()
	case "cap46-nested-authorization-contract-calls-sac":
		c.nestedAuth()
	case "cap21-vectors-at-max-length":
		c.vectorsAtMax()
	case "cap06-manage-buy-offer-update-by-id":
		c.offerByID()
	}
	c.decide()
	return r
}

func xkGetLedger(rpcURL, dir string, seq uint32) (xdr.LedgerCloseMeta, error) {
	var lcm xdr.LedgerCloseMeta
	if rpcURL == "" {
		return lcm, xdr.SafeUnmarshalBase64(xkReadTrim(filepath.Join(dir, "ledger-close-meta.b64")), &lcm)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	backend := ledgerbackend.NewRPCLedgerBackend(ledgerbackend.RPCLedgerBackendOptions{RPCServerURL: rpcURL, BufferSize: 1})
	defer backend.Close()
	xkDie(backend.PrepareRange(ctx, ledgerbackend.BoundedRange(seq, seq))) // getHealth + getLedgers
	lcm, err := backend.GetLedger(ctx, seq)
	const decodeFailed = "failed to unmarshal cached ledger: "
	if err != nil && !strings.HasPrefix(err.Error(), decodeFailed) {
		xkDie(err) // harness error: the mock is unreachable or the ledger is missing
	}
	if inner := errors.Unwrap(err); inner != nil {
		err = inner
	}
	return lcm, err
}

// xkRunProcess calls one of stellar-etl's per-ledger export functions with a real
// output file, reads the JSON lines back and keeps the rows of this transaction.
func xkRunProcess(name string, fn processLedgerFunc, lcm xdr.LedgerCloseMeta, env utils.EnvironmentDetails, belong func(map[string]any) bool, hook *xkHook) (ex *xkExport) {
	ex = &xkExport{parquetPossible: true}
	f, err := os.CreateTemp("", "xdrkit-etl-"+name+"-*.txt")
	xkDie(err)
	defer os.Remove(f.Name())
	defer f.Close()
	hook.take()

	var rows []transform.SchemaParquet
	func() {
		defer func() {
			if p := recover(); p != nil {
				ex.Fatal = xkPanicText(p)
			}
		}()
		rows, ex.Attempts, ex.Failures = fn(lcm, env, f, true, nil)
	}()
	ex.Errors = hook.take()
	lines := xkReadLines(f.Name())
	ex.RowsInLedger = len(lines)
	// The process functions append a Parquet row for every line they write, in order.
	aligned := len(rows) == len(lines)
	for i, line := range lines {
		row := xkDecode(line)
		if !belong(row) {
			continue
		}
		ex.Rows = append(ex.Rows, line)
		ex.rows = append(ex.rows, row)
		if aligned {
			pj, perr := json.Marshal(rows[i].ToParquet())
			xkDie(perr)
			ex.ParquetRows = append(ex.ParquetRows, pj)
			ex.parquet = append(ex.parquet, xkDecode(pj))
		}
	}
	ex.parquetPossible = aligned && len(rows) > 0
	return ex
}

// staticBackend serves one already-fetched ledger to input.ExtractBatch.
type xkStaticBackend struct{ lcm xdr.LedgerCloseMeta }

func (b xkStaticBackend) GetLatestLedgerSequence(context.Context) (uint32, error) {
	return b.lcm.LedgerSequence(), nil
}
func (b xkStaticBackend) GetLedger(_ context.Context, seq uint32) (xdr.LedgerCloseMeta, error) {
	if seq != b.lcm.LedgerSequence() {
		return xdr.LedgerCloseMeta{}, fmt.Errorf("ledger %d not held", seq)
	}
	return b.lcm, nil
}
func (b xkStaticBackend) PrepareRange(context.Context, ledgerbackend.Range) error { return nil }
func (b xkStaticBackend) IsPrepared(context.Context, ledgerbackend.Range) (bool, error) {
	return true, nil
}
func (b xkStaticBackend) Close() error { return nil }

// xkRunChanges runs export_ledger_entry_changes' path for one ledger: input.ExtractBatch
// (what input.StreamChanges calls per batch) and then the per-type Transform* dispatch
// of cmd/export_ledger_entry_changes.go (the loop over batch.Changes), every output
// type enabled, each row written with ExportEntry as exportTransformedData does.
func xkRunChanges(lcm xdr.LedgerCloseMeta, env utils.EnvironmentDetails, interest []string, hook *xkHook) map[string]*xkExport {
	out := map[string]*xkExport{}
	seq := lcm.LedgerSequence()
	var backend ledgerbackend.LedgerBackend = xkStaticBackend{lcm: lcm}
	hook.take()
	var batch input.ChangeBatch
	var fatal string
	func() {
		defer func() {
			if p := recover(); p != nil {
				fatal = xkPanicText(p)
			}
		}()
		batch = input.ExtractBatch(seq, seq, &backend, env, cmdLogger)
	}()
	if fatal != "" {
		out["ledger_entry_changes"] = &xkExport{Fatal: fatal, Errors: hook.take()}
		return out
	}

	outputs := map[string][]interface{}{}
	attempts := map[string]int{}
	failures := map[string]int{}
	errs := map[string][]string{}
	add := func(key string, v interface{}, err error, where string) {
		attempts[key]++
		if err != nil {
			failures[key]++
			errs[key] = append(errs[key], where+": "+err.Error())
			return
		}
		outputs[key] = append(outputs[key], v)
	}
	for entryType, changes := range batch.Changes {
		for i, change := range changes.Changes {
			_, changeType, _, _ := utils.ExtractEntryFromChange(change)
			if changeType == xdr.LedgerEntryChangeTypeLedgerEntryRestored {
				key, err := transform.TransformRestoredKey(change, changes.LedgerHeaders[i])
				add("restored_key", key, err, "TransformRestoredKey")
			}
		}
		switch entryType {
		case xdr.LedgerEntryTypeAccount:
			for i, change := range changes.Changes {
				if changed, err := change.AccountChangedExceptSigners(); err != nil {
					add("accounts", nil, err, "AccountChangedExceptSigners")
				} else if changed {
					acc, err := transform.TransformAccount(change, changes.LedgerHeaders[i])
					add("accounts", acc, err, "TransformAccount")
				}
				if utils.AccountSignersChanged(change) {
					signers, err := transform.TransformSigners(change, changes.LedgerHeaders[i])
					if err != nil {
						add("signers", nil, err, "TransformSigners")
						continue
					}
					for _, s := range signers {
						add("signers", s, nil, "")
					}
				}
			}
		case xdr.LedgerEntryTypeClaimableBalance:
			for i, change := range changes.Changes {
				v, err := transform.TransformClaimableBalance(change, changes.LedgerHeaders[i])
				add("claimable_balances", v, err, "TransformClaimableBalance")
			}
		case xdr.LedgerEntryTypeOffer:
			for i, change := range changes.Changes {
				v, err := transform.TransformOffer(change, changes.LedgerHeaders[i])
				add("offers", v, err, "TransformOffer")
			}
		case xdr.LedgerEntryTypeTrustline:
			for i, change := range changes.Changes {
				v, err := transform.TransformTrustline(change, changes.LedgerHeaders[i])
				add("trustlines", v, err, "TransformTrustline")
			}
		case xdr.LedgerEntryTypeLiquidityPool:
			for i, change := range changes.Changes {
				v, err := transform.TransformPool(change, changes.LedgerHeaders[i])
				add("liquidity_pools", v, err, "TransformPool")
			}
		case xdr.LedgerEntryTypeContractData:
			for i, change := range changes.Changes {
				t := transform.NewTransformContractDataStruct(transform.AssetFromContractData, transform.ContractBalanceFromContractData)
				v, err, _ := t.TransformContractData(change, env.NetworkPassphrase, changes.LedgerHeaders[i])
				if err == nil && v.ContractId == "" {
					continue
				}
				add("contract_data", v, err, "TransformContractData")
			}
		case xdr.LedgerEntryTypeContractCode:
			for i, change := range changes.Changes {
				v, err := transform.TransformContractCode(change, changes.LedgerHeaders[i])
				add("contract_code", v, err, "TransformContractCode")
			}
		case xdr.LedgerEntryTypeConfigSetting:
			for i, change := range changes.Changes {
				v, err := transform.TransformConfigSetting(change, changes.LedgerHeaders[i])
				add("config_settings", v, err, "TransformConfigSetting")
			}
		case xdr.LedgerEntryTypeTtl:
			for i, change := range changes.Changes {
				v, err := transform.TransformTtl(change, changes.LedgerHeaders[i])
				add("ttl", v, err, "TransformTtl")
			}
		}
	}

	keys := []string{"accounts", "signers", "claimable_balances", "offers", "trustlines", "liquidity_pools", "contract_data", "contract_code", "config_settings", "ttl", "restored_key"}
	for _, key := range keys {
		ex := &xkExport{Attempts: attempts[key], Failures: failures[key], Errors: errs[key]}
		f, err := os.CreateTemp("", "xdrkit-etl-"+key+"-*.txt")
		xkDie(err)
		for _, o := range outputs[key] {
			_, err := ExportEntry(o, f, nil)
			xkDie(err)
		}
		f.Close()
		lines := xkReadLines(f.Name())
		os.Remove(f.Name())
		ex.RowsInLedger = len(lines)
		for i, line := range lines {
			if !xkMentions(line, interest) {
				continue
			}
			ex.Rows = append(ex.Rows, line)
			row := xkDecode(line)
			ex.rows = append(ex.rows, row)
			if sp, ok := outputs[key][i].(transform.SchemaParquet); ok && key != "claimable_balances" && key != "restored_key" {
				pj, perr := json.Marshal(sp.ToParquet())
				xkDie(perr)
				ex.ParquetRows = append(ex.ParquetRows, pj)
				ex.parquet = append(ex.parquet, xkDecode(pj))
			}
		}
		out[key] = ex
	}
	if errsLogged := hook.take(); len(errsLogged) > 0 {
		out["accounts"].Errors = append(out["accounts"].Errors, errsLogged...)
	}
	return out
}

// xkInterest lists the account, contract and pool identifiers from capture.json;
// ledger-entry rows are kept when they mention one of them.
func xkInterest(dir string) []string {
	raw, err := os.ReadFile(filepath.Join(dir, "capture.json"))
	xkDie(err)
	var cap map[string]any
	xkDie(json.Unmarshal(raw, &cap))
	var out []string
	for k, v := range cap {
		s, ok := v.(string)
		if !ok || k == "txHash" || k == "createdInTx" {
			continue
		}
		if (len(s) >= 56 && strings.ContainsRune("GCLMP", rune(s[0])) && strings.ToUpper(s) == s) || (len(s) == 64 && xkIsHex(s)) {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- checks

type xkChecker struct {
	r        *xkEntryResult
	dir      string
	expected map[string]json.RawMessage
}

func (c *xkChecker) capture(key string) string {
	raw, err := os.ReadFile(filepath.Join(c.dir, "capture.json"))
	xkDie(err)
	var m map[string]any
	xkDie(json.Unmarshal(raw, &m))
	return xkStr(m[key])
}

func (c *xkChecker) exp(pointerSuffix string) string {
	for p, v := range c.expected {
		if strings.HasSuffix(p, pointerSuffix) {
			var s string
			if json.Unmarshal(v, &s) == nil {
				return s
			}
			return string(v)
		}
	}
	xkDie(fmt.Errorf("%s: no expected value ending in %s", c.r.ID, pointerSuffix))
	return ""
}

func (c *xkChecker) rows(export string) []map[string]any {
	if ex := c.r.Exports[export]; ex != nil {
		return ex.rows
	}
	return nil
}

func (c *xkChecker) find(export string, pred func(map[string]any) bool) map[string]any {
	for _, row := range c.rows(export) {
		if pred(row) {
			return row
		}
	}
	return nil
}

// fact records want vs got. got == nil means the row or field is missing.
func (c *xkChecker) fact(export, check, want string, got *string, required bool) {
	f := xkFact{Export: export, Check: check, Want: want, Required: required}
	switch {
	case got == nil:
		f.Status, f.Got = "missing", "(no row or field)"
	case *got == want:
		f.Status, f.Got = "exact", *got
	default:
		f.Status, f.Got = "altered", *got
	}
	c.r.Facts = append(c.r.Facts, f)
}

// ref attaches a known upstream issue to the last fact.
func (c *xkChecker) ref(issue string) { c.r.Facts[len(c.r.Facts)-1].Ref = issue }

// xkDistinctRenderedKeys counts distinct keys in a Go ScMap rendering like "[{k v} {k v}]".
func xkDistinctRenderedKeys(s string) int {
	set := map[string]bool{}
	for _, part := range strings.Split(strings.Trim(s, "[]"), "} {") {
		part = strings.Trim(part, "{}")
		if k, _, ok := strings.Cut(part, " "); ok {
			set[k] = true
		}
	}
	return len(set)
}

// field returns the value at path inside row (nil row or absent field: nil).
func field(row map[string]any, path ...string) *string {
	if row == nil {
		return nil
	}
	var cur any = row
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		v, ok := m[p]
		if !ok {
			return nil
		}
		cur = v
	}
	s := xkStr(cur)
	return &s
}

func ptr(s string) *string { return &s }

func (c *xkChecker) signedPayloadSigner() {
	key := c.exp("/signers/0/key")
	account := ""
	if row := c.find("signers", func(r map[string]any) bool { return xkStr(r["signer"]) == key }); row != nil {
		account = xkStr(row["account_id"])
	}
	row := c.find("signers", func(r map[string]any) bool { return xkStr(r["signer"]) == key })
	c.fact("signers", "signer row for the signed-payload key (all 64 payload bytes)", key, field(row, "signer"), true)
	c.fact("signers", "signer weight", c.exp("/signers/0/weight"), field(row, "weight"), true)
	if pq := c.parquetFor("signers", func(r map[string]any) bool { return xkStr(r["Signer"]) == key }); pq != nil || c.r.Exports["signers"].parquetPossible {
		c.fact("signers", "Parquet row Signer", key, field(pq, "Signer"), true)
	}
	op := c.find("operations", func(r map[string]any) bool { return xkStr(r["type_string"]) == "set_options" })
	c.fact("operations", "set_options details.signer_key", key, field(op, "details", "signer_key"), true)
	eff := c.find("effects", func(r map[string]any) bool { return xkStr(r["type_string"]) == "signer_created" })
	c.fact("effects", "signer_created details.public_key", key, field(eff, "details", "public_key"), true)
	c.r.Context["signerAccount"] = account
}

func (c *xkChecker) muxedPayment() {
	src := c.capture("muxedSource")
	srcID := c.capture("muxedSourceId")
	dst := c.capture("muxedDestination")
	dstID := c.capture("muxedDestinationId")
	tx := xkFirst(c.r.Exports["transactions"])
	c.fact("transactions", "account_muxed (u64 max id)", src, field(tx, "account_muxed"), true)
	op := c.find("operations", func(r map[string]any) bool { return xkStr(r["type_string"]) == "payment" })
	c.fact("operations", "source_account_muxed", src, field(op, "source_account_muxed"), true)
	c.fact("operations", "details.from_muxed", src, field(op, "details", "from_muxed"), true)
	c.fact("operations", "details.from_muxed_id (u64 max)", srcID, field(op, "details", "from_muxed_id"), true)
	c.fact("operations", "details.to_muxed", dst, field(op, "details", "to_muxed"), true)
	c.fact("operations", "details.to_muxed_id (0)", dstID, field(op, "details", "to_muxed_id"), true)
	c.fact("operations", "details.amount (1 XLM = 10000000 stroops)", "1", field(op, "details", "amount"), true)
	if pq := c.parquetFor("operations", func(r map[string]any) bool { return xkStr(r["TypeString"]) == "payment" }); pq != nil {
		d := xkDecode([]byte(xkStr(pq["OperationDetails"])))
		c.fact("operations", "Parquet OperationDetails from_muxed_id", srcID, field(d, "from_muxed_id"), true)
		c.fact("operations", "Parquet OperationDetails to_muxed_id", dstID, field(d, "to_muxed_id"), true)
	}
	cr := c.find("effects", func(r map[string]any) bool { return xkStr(r["type_string"]) == "account_credited" })
	c.fact("effects", "account_credited address_muxed", dst, field(cr, "address_muxed"), true)
	db := c.find("effects", func(r map[string]any) bool { return xkStr(r["type_string"]) == "account_debited" })
	c.fact("effects", "account_debited address_muxed", src, field(db, "address_muxed"), true)
	tt := c.find("token_transfers", func(r map[string]any) bool { return xkStr(r["event_topic"]) == "transfer" })
	c.fact("token_transfers", "transfer to_muxed", dst, field(tt, "to_muxed"), true)
	c.fact("token_transfers", "transfer to_muxed_id (0)", dstID, field(tt, "to_muxed_id"), true)
}

func (c *xkChecker) sacTrustline() {
	trustor := c.exp("/trustline/account_id")
	code := c.exp("/trustline/asset/credit_alphanum4/asset_code")
	limit := c.exp("/trustline/limit")
	issuer := strings.SplitN(c.capture("asset"), ":", 2)[1]
	row := c.find("trustlines", func(r map[string]any) bool {
		return xkStr(r["account_id"]) == trustor && xkStr(r["asset_code"]) == code
	})
	c.fact("trustlines", "trustline row for the trustor (created by SAC trust())", trustor, field(row, "account_id"), true)
	c.fact("trustlines", "asset_issuer", issuer, field(row, "asset_issuer"), true)
	c.fact("trustlines", "trust_line_limit (i64::MAX)", limit, field(row, "trust_line_limit"), true)
	c.fact("trustlines", "ledger_entry_change (0 = created)", "0", field(row, "ledger_entry_change"), true)
	if c.r.Exports["trustlines"].parquetPossible || len(c.r.Exports["trustlines"].parquet) > 0 {
		pq := c.parquetFor("trustlines", func(r map[string]any) bool { return xkStr(r["AccountID"]) == trustor })
		c.fact("trustlines", "Parquet TrustlineLimit", limit, field(pq, "TrustlineLimit"), true)
	}
	var effects []string
	for _, e := range c.rows("effects") {
		effects = append(effects, xkStr(e["type_string"]))
	}
	c.r.Context["effectsForTransaction"] = effects
	eff := c.find("effects", func(r map[string]any) bool {
		return xkStr(r["type_string"]) == "trustline_created" && xkStr(r["address"]) == trustor
	})
	c.fact("effects", "trustline_created effect for the trustor", trustor, field(eff, "address"), true)
	c.ref("stellar/stellar-etl#460 (open)")
}

func (c *xkChecker) sacTransferToMuxed() {
	from := c.capture("from")
	dest := c.capture("destinationAccount")
	muxed := c.capture("muxedDestination")
	id := c.capture("muxedDestinationId")
	amount := c.capture("amountStroops")
	tt := c.find("token_transfers", func(r map[string]any) bool {
		return xkStr(r["event_topic"]) == "transfer" && xkStr(r["from"]) == from
	})
	c.fact("token_transfers", "transfer from", from, field(tt, "from"), true)
	c.fact("token_transfers", "transfer to (base G account)", dest, field(tt, "to"), true)
	c.fact("token_transfers", "to_muxed", muxed, field(tt, "to_muxed"), true)
	c.fact("token_transfers", "to_muxed_id (u64 max)", id, field(tt, "to_muxed_id"), true)
	c.fact("token_transfers", "amount_raw", amount, field(tt, "amount_raw"), true)
	ev := c.find("contract_events", func(r map[string]any) bool {
		return xkTopicSymbol(r, 0) == "transfer" && xkStr(r["type_string"]) == "ContractEventTypeContract"
	})
	if ev == nil {
		ev = c.find("contract_events", func(r map[string]any) bool { return xkTopicSymbol(r, 0) == "transfer" })
	}
	c.fact("contract_events", "data_decoded map to_muxed_id (u64 max)", id, xkMapVal(ev, "data_decoded", "to_muxed_id", "u64"), true)
	c.fact("contract_events", "topics_decoded[2] (base G account)", dest, xkTopicAddress(ev, 2), true)
	op := c.find("operations", func(r map[string]any) bool { return xkStr(r["type_string"]) == "invoke_host_function" })
	c.fact("operations", "parameters_json_decoded `to` argument (M address)", muxed, xkParamAddress(op, 3), true)
	c.r.Context["operations.parameters_decoded[3]"] = xkDecodedParam(op, 3)
	if abc := field(op, "details", "asset_balance_changes"); abc != nil {
		c.r.Context["operations.asset_balance_changes"] = *abc
	}
	cr := c.find("effects", func(r map[string]any) bool { return xkStr(r["type_string"]) == "account_credited" })
	c.fact("effects", "account_credited address (base G)", dest, field(cr, "address"), true)
	c.fact("effects", "account_credited address_muxed (M address of the CAP-67 destination)", muxed, field(cr, "address_muxed"), true)
	c.ref("stellar/stellar-etl#460 (open)")
	c.r.Context["effects.account_credited"] = cr
}

func (c *xkChecker) poolShare() {
	first := c.capture("issuerFirstByBytes")
	second := c.capture("issuerSecondByBytes")
	poolHex := c.capture("liquidityPoolId")
	poolStrkey := c.exp("/liquidity_pool_deposit/liquidity_pool_id")
	depositor := c.capture("depositor")
	lp := c.find("liquidity_pools", func(r map[string]any) bool { return xkStr(r["liquidity_pool_id"]) == poolHex })
	c.fact("liquidity_pools", "pool row by liquidity_pool_id (hex)", poolHex, field(lp, "liquidity_pool_id"), true)
	c.fact("liquidity_pools", "liquidity_pool_id_strkey", poolStrkey, field(lp, "liquidity_pool_id_strkey"), true)
	c.fact("liquidity_pools", "asset_a_issuer (first by key bytes)", first, field(lp, "asset_a_issuer"), true)
	c.fact("liquidity_pools", "asset_b_issuer (second by key bytes)", second, field(lp, "asset_b_issuer"), true)
	tl := c.find("trustlines", func(r map[string]any) bool {
		return xkStr(r["account_id"]) == depositor && xkStr(r["liquidity_pool_id"]) == poolHex
	})
	c.fact("trustlines", "pool-share trustline liquidity_pool_id", poolHex, field(tl, "liquidity_pool_id"), true)
	c.fact("trustlines", "pool-share trustline liquidity_pool_id_strkey", poolStrkey, field(tl, "liquidity_pool_id_strkey"), true)
	ct := c.find("operations", func(r map[string]any) bool { return xkStr(r["type_string"]) == "change_trust" })
	c.fact("operations", "change_trust details.liquidity_pool_id", poolHex, field(ct, "details", "liquidity_pool_id"), true)
	dep := c.find("operations", func(r map[string]any) bool { return xkStr(r["type_string"]) == "liquidity_pool_deposit" })
	c.fact("operations", "liquidity_pool_deposit details.liquidity_pool_id", poolHex, field(dep, "details", "liquidity_pool_id"), true)
	c.fact("operations", "liquidity_pool_deposit details.reserve_a_asset_issuer", first, field(dep, "details", "reserve_a_asset_issuer"), true)
	c.fact("operations", "liquidity_pool_deposit details.reserve_b_asset_issuer", second, field(dep, "details", "reserve_b_asset_issuer"), true)
	ef := c.find("effects", func(r map[string]any) bool { return xkStr(r["type_string"]) == "liquidity_pool_deposited" })
	c.fact("effects", "liquidity_pool_deposited details.liquidity_pool.id", poolHex, field(ef, "details", "liquidity_pool", "id"), true)
	if ef != nil {
		c.r.Context["effects.liquidity_pool_deposited.details"] = ef["details"]
	}
}

func (c *xkChecker) scMapCollision() {
	want := xkCanon(c.expected["/v4/operations/0/events/0/body/v0/data/map"])
	ev := c.find("contract_events", func(r map[string]any) bool {
		d, ok := r["data_decoded"].(map[string]any)
		return ok && d["map"] != nil && xkStr(r["type_string"]) == "ContractEventTypeContract"
	})
	var got *string
	if ev != nil {
		if d, ok := ev["data_decoded"].(map[string]any); ok {
			got = ptr(xkCanonAny(d["map"]))
		}
	}
	c.fact("contract_events", "data_decoded map keeps u32 1, string \"1\" and symbol \"1\" as three entries", want, got, true)
	op := c.find("operations", func(r map[string]any) bool { return xkStr(r["type_string"]) == "invoke_host_function" })
	var argMap *string
	var argIdx = -1
	if op != nil {
		if d, ok := op["details"].(map[string]any); ok {
			if params, ok := d["parameters_json_decoded"].([]any); ok {
				for i, p := range params {
					if pm, ok := p.(map[string]any); ok && pm["map"] != nil {
						argMap, argIdx = ptr(xkCanonAny(pm["map"])), i
						break
					}
				}
			}
		}
	}
	c.fact("operations", "parameters_json_decoded map argument keeps the three keys", xkMapKeysCanon(want), xkKeysOf(argMap), true)
	var legacy *string
	if argIdx >= 0 {
		if p, ok := xkDecodedParam(op, argIdx).(map[string]any); ok {
			legacy = ptr(xkStr(p["value"]))
		}
	}
	f := xkFact{Export: "operations", Check: "parameters_decoded (ScVal.String() rendering) keeps u32 1, string \"1\" and symbol \"1\" distinguishable", Want: "three distinguishable keys", Required: true}
	switch {
	case legacy == nil:
		f.Status, f.Got = "missing", "(no row or field)"
	case xkDistinctRenderedKeys(*legacy) < 3:
		f.Status, f.Got = "altered", *legacy
	default:
		f.Status, f.Got = "exact", *legacy
	}
	c.r.Facts = append(c.r.Facts, f)
	c.ref("stellar/stellar-etl#330 (closed: added parameters_json_decoded; parameters_decoded is still written)")
	if argIdx >= 0 {
		c.r.Context["operations.parameters_decoded[map argument]"] = xkDecodedParam(op, argIdx)
		c.r.Context["operations.parameters_json_decoded[map argument]"] = *argMap
	}
	c.r.Context["returnValueExported"] = "no transform output carries the Soroban return value (no reference to ReturnValue in internal/transform)"
}

func (c *xkChecker) nestedAuth() {
	envB64 := xkReadTrim(filepath.Join(c.dir, "envelope.b64"))
	lt := xkFirst(c.r.Exports["ledger_transaction"])
	c.fact("ledger_transaction", "tx_envelope keeps the authorization tree byte for byte", envB64, field(lt, "tx_envelope"), true)
	tx := xkFirst(c.r.Exports["transactions"])
	c.fact("transactions", "tx_envelope keeps the authorization tree byte for byte", envB64, field(tx, "tx_envelope"), true)
	op := c.find("operations", func(r map[string]any) bool { return xkStr(r["type_string"]) == "invoke_host_function" })
	c.fact("operations", "details.contract_id (probe contract)", c.capture("probeContractId"), field(op, "details", "contract_id"), true)
	c.fact("operations", "parameters_json_decoded[1] function name", "pay", xkParamSymbol(op, 1), true)
	authorizer := c.capture("authorizer")
	recipient := c.capture("recipient")
	tt := c.find("token_transfers", func(r map[string]any) bool {
		return xkStr(r["event_topic"]) == "transfer" && xkStr(r["from"]) == authorizer
	})
	c.fact("token_transfers", "sub-invocation transfer from the authorizer", authorizer, field(tt, "from"), true)
	c.fact("token_transfers", "sub-invocation transfer to the recipient", recipient, field(tt, "to"), true)
	c.fact("token_transfers", "sub-invocation transfer amount_raw", c.capture("amountStroops"), field(tt, "amount_raw"), true)
	if op != nil {
		if d, ok := op["details"].(map[string]any); ok {
			var keys []string
			for k := range d {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			c.r.Context["operations.details keys"] = keys
		}
	}
	c.r.Context["authorizationColumns"] = "no transform output decodes SorobanAuthorizationEntry (no reference to auth entries in internal/transform); the tree is kept only inside tx_envelope"
}

func (c *xkChecker) vectorsAtMax() {
	tx := xkFirst(c.r.Exports["transactions"])
	c.fact("transactions", "operation_count", c.capture("operationCount"), field(tx, "operation_count"), true)
	c.fact("transactions", "memo (28-byte text)", c.exp("/tx/tx/memo/text"), field(tx, "memo"), true)
	signers := c.exp("/cond/v2/extra_signers/0") + "," + c.exp("/cond/v2/extra_signers/1")
	var gotSigners *string
	if tx != nil {
		if arr, ok := tx["extra_signers"].([]any); ok {
			var s []string
			for _, a := range arr {
				s = append(s, xkStr(a))
			}
			gotSigners = ptr(strings.Join(s, ","))
		}
	}
	c.fact("transactions", "extra_signers (two P keys, 64-byte payloads)", signers, gotSigners, true)
	if pq := xkFirstParquet(c.r.Exports["transactions"]); pq != nil {
		var s []string
		if arr, ok := pq["ExtraSigners"].([]any); ok {
			for _, a := range arr {
				s = append(s, xkStr(a))
			}
		}
		c.fact("transactions", "Parquet ExtraSigners", signers, ptr(strings.Join(s, ",")), true)
		c.fact("transactions", "Parquet Memo", c.exp("/tx/tx/memo/text"), field(pq, "Memo"), true)
	}
	c.fact("operations", "operation rows for the transaction", "100", ptr(fmt.Sprint(len(c.rows("operations")))), true)
	last := c.find("operations", func(r map[string]any) bool { return xkInt(r["id"]) == c.r.TxID+100 })
	c.fact("operations", "operation 100 details.name (64 characters)", c.exp("/operations/99/body/manage_data/data_name"), field(last, "details", "name"), true)
	valueHex := c.exp("/operations/99/body/manage_data/data_value")
	var gotHex *string
	if v := field(last, "details", "value"); v != nil {
		if b, err := base64.StdEncoding.DecodeString(*v); err == nil {
			gotHex = ptr(hex.EncodeToString(b))
		} else {
			gotHex = ptr("undecodable base64: " + *v)
		}
	}
	c.fact("operations", "operation 100 details.value (64 bytes, base64)", valueHex, gotHex, true)
	n := 0
	for _, e := range c.rows("effects") {
		if xkStr(e["type_string"]) == "data_created" {
			n++
		}
	}
	c.fact("effects", "data_created effects", "100", ptr(fmt.Sprint(n)), true)
	var tts []map[string]any
	for _, t := range c.rows("token_transfers") {
		tts = append(tts, map[string]any{"event_topic": t["event_topic"], "from": t["from"], "to": t["to"], "to_muxed": t["to_muxed"], "to_muxed_id": t["to_muxed_id"]})
	}
	c.r.Context["token_transfers"] = tts
}

func (c *xkChecker) offerByID() {
	id := c.exp("/manage_buy_offer/offer_id")
	seller := c.capture("seller")
	op := c.find("operations", func(r map[string]any) bool { return xkStr(r["type_string"]) == "manage_buy_offer" })
	c.fact("operations", "manage_buy_offer details.offer_id", id, field(op, "details", "offer_id"), true)
	of := c.find("offers", func(r map[string]any) bool { return xkStr(r["offer_id"]) == id })
	c.fact("offers", "offer row by offer_id", id, field(of, "offer_id"), true)
	c.fact("offers", "offer seller_id", seller, field(of, "seller_id"), true)
	c.fact("offers", "ledger_entry_change (1 = updated)", "1", field(of, "ledger_entry_change"), true)
	c.r.Context["trades"] = len(c.rows("trades"))
}

func (c *xkChecker) parquetFor(export string, pred func(map[string]any) bool) map[string]any {
	ex := c.r.Exports[export]
	if ex == nil {
		return nil
	}
	for _, p := range ex.parquet {
		if pred(p) {
			return p
		}
	}
	return nil
}

// decide turns the facts into one outcome: a missing required fact means the
// shape never reached the output (dropped); an altered one means it did but changed.
func (c *xkChecker) decide() {
	r := c.r
	var missing, altered *xkFact
	for i := range r.Facts {
		f := &r.Facts[i]
		if !f.Required {
			continue
		}
		if f.Status == "missing" && missing == nil {
			missing = f
		}
		if f.Status == "altered" && altered == nil {
			altered = f
		}
	}
	explain := func(f *xkFact) string {
		ex := r.Exports[f.Export]
		s := ""
		if ex != nil {
			if ex.Fatal != "" {
				s = "; export fatal: " + ex.Fatal
			} else if ex.Failures > 0 && len(ex.Errors) > 0 {
				s = fmt.Sprintf("; %d transform failure(s), first: %s", ex.Failures, ex.Errors[0])
			}
		}
		return s
	}
	// Per-export verdict, so a table that keeps the shape is not hidden by one that does not.
	r.PerExport = map[string]string{}
	exact := 0
	for _, f := range r.Facts {
		v := map[string]string{"exact": "pass", "altered": "mishandled", "missing": "dropped"}[f.Status]
		if f.Status == "exact" {
			exact++
		}
		if prev, ok := r.PerExport[f.Export]; !ok || xkRank(v) > xkRank(prev) {
			r.PerExport[f.Export] = v
		}
	}
	known := func(f *xkFact) string {
		if f.Ref == "" {
			return ""
		}
		return " [known: " + f.Ref + "]"
	}
	var passing []string
	for ex, v := range r.PerExport {
		if v == "pass" {
			passing = append(passing, ex)
		}
	}
	sort.Strings(passing)
	also := ""
	if len(passing) > 0 {
		also = "; exact in " + strings.Join(passing, ", ")
	}
	switch {
	case missing != nil:
		r.Outcome, r.Stage = "dropped", missing.Export
		r.Detail = fmt.Sprintf("%s: %s not in the export output%s%s%s", missing.Export, missing.Check, explain(missing), known(missing), also)
	case altered != nil:
		got := altered.Got
		if got == "" {
			got = "null"
		}
		r.Outcome, r.Stage = "mishandled", altered.Export
		r.Detail = fmt.Sprintf("%s: %s is %q, expected %q%s%s%s", altered.Export, altered.Check, got, altered.Want, explain(altered), known(altered), also)
	default:
		r.Outcome, r.Stage = "pass", "export-json"
		r.Detail = fmt.Sprintf("all %d checks exact across %d exports", exact, len(r.PerExport))
	}
}

func xkRank(v string) int { return map[string]int{"pass": 0, "mishandled": 1, "dropped": 2}[v] }

// ---------------------------------------------------------------- precision (#454)

// xkAmountPrecision compares token_transfers.amount (float64) with amount_raw (exact
// integer string) for this transaction's rows.
func xkAmountPrecision(ex *xkExport) []map[string]any {
	var out []map[string]any
	if ex == nil {
		return out
	}
	for _, row := range ex.rows {
		raw := xkStr(row["amount_raw"])
		amt := xkStr(row["amount"])
		exactRaw, ok1 := new(big.Rat).SetString(raw)
		gotAmt, ok2 := new(big.Rat).SetString(amt)
		entry := map[string]any{"event_topic": row["event_topic"], "amount_raw": raw, "amount": amt}
		if ok1 && ok2 {
			want := new(big.Rat).Quo(exactRaw, big.NewRat(10_000_000, 1))
			entry["amountEqualsRawOver1e7"] = want.Cmp(gotAmt) == 0
		}
		out = append(out, entry)
	}
	return out
}

// ---------------------------------------------------------------- helpers

func xkTopicSymbol(r map[string]any, i int) string {
	topics, ok := r["topics_decoded"].([]any)
	if !ok || len(topics) <= i {
		return ""
	}
	m, _ := topics[i].(map[string]any)
	return xkStr(m["symbol"])
}

func xkTopicAddress(r map[string]any, i int) *string {
	if r == nil {
		return nil
	}
	topics, ok := r["topics_decoded"].([]any)
	if !ok || len(topics) <= i {
		return nil
	}
	m, _ := topics[i].(map[string]any)
	if v, ok := m["address"]; ok {
		return ptr(xkStr(v))
	}
	return nil
}

// xkMapVal finds key (a symbol) in an XDR-JSON ScMap at r[col] and returns val[arm].
func xkMapVal(r map[string]any, col, key, arm string) *string {
	if r == nil {
		return nil
	}
	d, ok := r[col].(map[string]any)
	if !ok {
		return nil
	}
	entries, _ := d["map"].([]any)
	for _, e := range entries {
		em, _ := e.(map[string]any)
		k, _ := em["key"].(map[string]any)
		if xkStr(k["symbol"]) == key {
			v, _ := em["val"].(map[string]any)
			if x, ok := v[arm]; ok {
				return ptr(xkStr(x))
			}
			return ptr(xkCanonAny(v))
		}
	}
	return nil
}

func xkParam(op map[string]any, i int, col string) any {
	if op == nil {
		return nil
	}
	d, _ := op["details"].(map[string]any)
	params, _ := d[col].([]any)
	if len(params) <= i {
		return nil
	}
	return params[i]
}

func xkParamAddress(op map[string]any, i int) *string {
	p, _ := xkParam(op, i, "parameters_json_decoded").(map[string]any)
	if v, ok := p["address"]; ok {
		return ptr(xkStr(v))
	}
	if p != nil {
		return ptr(xkCanonAny(p))
	}
	return nil
}

func xkParamSymbol(op map[string]any, i int) *string {
	p, _ := xkParam(op, i, "parameters_json_decoded").(map[string]any)
	if v, ok := p["symbol"]; ok {
		return ptr(xkStr(v))
	}
	return nil
}

func xkDecodedParam(op map[string]any, i int) any { return xkParam(op, i, "parameters_decoded") }

func xkFirst(ex *xkExport) map[string]any {
	if ex == nil || len(ex.rows) == 0 {
		return nil
	}
	return ex.rows[0]
}

func xkFirstParquet(ex *xkExport) map[string]any {
	if ex == nil || len(ex.parquet) == 0 {
		return nil
	}
	return ex.parquet[0]
}

func xkStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return fmt.Sprint(x)
	default:
		return xkCanonAny(x)
	}
}

func xkInt(v any) int64 {
	n, ok := v.(json.Number)
	if !ok {
		return -1
	}
	i, err := n.Int64()
	if err != nil {
		return -1
	}
	return i
}

func xkCanon(raw json.RawMessage) string {
	return xkCanonAny(xkDecodeAny(raw))
}

func xkCanonAny(v any) string {
	b, err := json.Marshal(v) // map keys sorted; json.Number kept verbatim
	xkDie(err)
	return string(b)
}

// xkMapKeysCanon / xkKeysOf compare only the keys of an XDR-JSON ScMap.
func xkMapKeysCanon(canonMap string) string {
	return *xkKeysOf(&canonMap)
}

func xkKeysOf(canonMap *string) *string {
	if canonMap == nil {
		return nil
	}
	entries, _ := xkDecodeAny([]byte(*canonMap)).([]any)
	var keys []any
	for _, e := range entries {
		em, _ := e.(map[string]any)
		keys = append(keys, em["key"])
	}
	return ptr(xkCanonAny(keys))
}

func xkDecode(b []byte) map[string]any {
	m, _ := xkDecodeAny(b).(map[string]any)
	return m
}

func xkDecodeAny(b []byte) any {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil
	}
	return v
}

func xkReadLines(path string) []json.RawMessage {
	f, err := os.Open(path)
	xkDie(err)
	defer f.Close()
	var out []json.RawMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		out = append(out, append(json.RawMessage(nil), line...))
	}
	xkDie(sc.Err())
	return out
}

func xkMentions(line []byte, ids []string) bool {
	for _, id := range ids {
		if bytes.Contains(line, []byte(`"`+id+`"`)) {
			return true
		}
	}
	return false
}

func xkReadTrim(path string) string {
	b, err := os.ReadFile(path)
	xkDie(err)
	return strings.TrimSpace(string(b))
}

func xkIsHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

func xkPanicText(p any) string {
	if f, ok := p.(xkFatal); ok {
		return "logger.Fatal: " + f.msg
	}
	return fmt.Sprintf("panic: %v", p)
}

func xkDie(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "xdrkit_replay:", err)
		os.Exit(1)
	}
}
