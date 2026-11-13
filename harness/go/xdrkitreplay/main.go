// Command xdrkitreplay is the xdr-kit reference adapter for stellar/wallet-backend.
//
// It replays real corpus entries (core-emitted LedgerCloseMeta captured from testnet)
// through wallet-backend's own indexer (internal/indexer: participants, effects,
// SAC events, token transfer, trustlines, accounts...) and then runs the exact
// driver.Valuer encode that StateModel/TransactionModel/OperationModel BatchCopy
// apply before COPY, plus the matching sql.Scanner read-back. No database is used.
//
// It must live inside the wallet-backend module (internal packages), e.g. at
// internal/xdrkitreplay/main.go, and is run with:
//
//	go run ./internal/xdrkitreplay -corpus <xdr-kit/corpus> -rpc <mock RPC URL> -label <commit> -out <file.json>
//
// With -rpc, each entry's ledger is fetched from the harness mock RPC (harness/mockrpc)
// through go-stellar-sdk's RPCLedgerBackend, the ledger backend wallet-backend's ingest
// builds when LedgerBackendType is "rpc" (internal/ingest/ledger_backend.go). Without
// -rpc, the ledger is read from the entry's ledger-close-meta.b64 file.
//
// Per entry it reports one outcome in the RFP vocabulary:
//
//	pass       the entry's shape is indexed and every stored address reads back intact
//	mishandled the shape is indexed but a stored value is altered or cannot be read back
//	dropped    the shape never reaches storage: no row is produced, the ledger cannot be
//	           decoded or processed, or the row fails to encode (the persist transaction
//	           rolls back and ingestion halts at that ledger)
package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/alitto/pond/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stellar/wallet-backend/internal/indexer"
	"github.com/stellar/wallet-backend/internal/indexer/types"
	"github.com/stellar/wallet-backend/internal/metrics"
)

type manifest struct {
	Entries []struct {
		ID      string `json:"id"`
		Network struct {
			Passphrase string `json:"passphrase"`
		} `json:"network"`
		Source struct {
			Ledger uint32 `json:"ledger"`
			TxHash string `json:"txHash"`
		} `json:"source"`
		Sightings []string `json:"sightings"`
		Expected  struct {
			Values []struct {
				Pointer string `json:"pointer"`
				Value   any    `json:"value"`
			} `json:"values"`
		} `json:"expected"`
	} `json:"entries"`
}

// storedValue is one address-like column value pushed through Value() and Scan().
type storedValue struct {
	Where    string `json:"where"`
	Column   string `json:"column"`
	GoType   string `json:"goType"`
	In       string `json:"in"`
	Out      string `json:"out,omitempty"`
	OutBytes int    `json:"storedBytes,omitempty"`
	Status   string `json:"status"` // exact | reduced-to-base | altered | undecodable | encode-error | scan-error
	Err      string `json:"error,omitempty"`
}

type entryResult struct {
	ID           string         `json:"id"`
	Outcome      string         `json:"outcome"`
	Stage        string         `json:"stage"`
	Detail       string         `json:"detail"`
	Evidence     map[string]any `json:"evidence,omitempty"`
	StoredValues []storedValue  `json:"storedValues,omitempty"`
}

type runResult struct {
	Label            string        `json:"label"`
	Module           string        `json:"module"`
	VCSRevision      string        `json:"vcsRevision,omitempty"`
	GoStellarSDK     string        `json:"goStellarSdk"`
	GoStellarSDKRepl string        `json:"goStellarSdkReplace,omitempty"`
	LedgerSource     string        `json:"ledgerSource"` // "getLedgers" or "file"
	Entries          []entryResult `json:"entries"`
}

func main() {
	corpus := flag.String("corpus", "", "path to xdr-kit/corpus")
	label := flag.String("label", "", "commit label for the report")
	out := flag.String("out", "", "output JSON file (default stdout)")
	only := flag.String("entry", "", "run only this entry id")
	commit := flag.String("commit", "", "full commit sha of the checkout")
	rpcURL := flag.String("rpc", "", "mock RPC URL serving the corpus over getLedgers (default: read the .b64 files)")
	flag.Parse()
	if *corpus == "" {
		fmt.Fprintln(os.Stderr, "-corpus is required")
		os.Exit(2)
	}

	raw, err := os.ReadFile(filepath.Join(*corpus, "manifest.json"))
	must(err)
	var m manifest
	must(json.Unmarshal(raw, &m))

	rr := runResult{Label: *label, VCSRevision: *commit, LedgerSource: "file"}
	if *rpcURL != "" {
		rr.LedgerSource = "getLedgers"
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rr.Module = bi.Main.Path
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && rr.VCSRevision == "" {
				rr.VCSRevision = s.Value
			}
		}
		for _, d := range bi.Deps {
			if d.Path == "github.com/stellar/go-stellar-sdk" {
				rr.GoStellarSDK = d.Version
				if d.Replace != nil {
					rr.GoStellarSDKRepl = d.Replace.Path + "@" + d.Replace.Version
				}
			}
		}
	}

	for _, e := range m.Entries {
		if *only != "" && e.ID != *only {
			continue
		}
		dir := filepath.Join(*corpus, "entries", e.ID)
		res := runEntry(*rpcURL, dir, e.ID, e.Source.Ledger, e.Network.Passphrase, e.Source.TxHash, expectedStrings(e.Expected.Values))
		rr.Entries = append(rr.Entries, res)
	}

	enc, err := json.MarshalIndent(rr, "", "  ")
	must(err)
	if *out == "" {
		fmt.Println(string(enc))
		return
	}
	must(os.WriteFile(*out, append(enc, '\n'), 0o644))
	for _, r := range rr.Entries {
		fmt.Printf("%-48s %-10s %s\n", r.ID, r.Outcome, r.Detail)
	}
}

func expectedStrings(vals []struct {
	Pointer string `json:"pointer"`
	Value   any    `json:"value"`
},
) map[string]string {
	out := map[string]string{}
	for _, v := range vals {
		if s, ok := v.Value.(string); ok {
			out[v.Pointer] = s
		}
	}
	return out
}

func runEntry(rpcURL, dir, id string, ledger uint32, passphrase, txHash string, expected map[string]string) entryResult {
	res := entryResult{ID: id, Evidence: map[string]any{}}

	// 1. Get and decode the full-ledger LedgerCloseMeta exactly as captured from core.
	lcm, err := getLedger(rpcURL, dir, ledger)
	if err != nil {
		// Record whether the per-transaction XDR still decodes, to locate the break.
		parts := map[string]string{}
		for name, target := range map[string]any{"envelope.b64": &xdr.TransactionEnvelope{}, "result.b64": &xdr.TransactionResult{}, "meta.b64": &xdr.TransactionMeta{}} {
			raw, rerr := os.ReadFile(filepath.Join(dir, name))
			must(rerr)
			if derr := xdr.SafeUnmarshalBase64(strings.TrimSpace(string(raw)), target); derr != nil {
				parts[name] = derr.Error()
			} else {
				parts[name] = "decodes"
			}
		}
		res.Evidence["perTransactionXdr"] = parts
		return dropped(res, "decode-ledger-close-meta", fmt.Sprintf("pinned XDR cannot decode the LedgerCloseMeta: %v", err))
	}
	res.Evidence["ledgerCloseMetaVersion"] = lcm.V

	// 2. Pull our transaction out with the SDK's ingest reader.
	reader, err := ingest.NewLedgerTransactionReaderFromLedgerCloseMeta(passphrase, lcm)
	if err != nil {
		return dropped(res, "ingest-reader", fmt.Sprintf("ingest reader rejects the ledger: %v", err))
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
	if tx == nil {
		return dropped(res, "ingest-reader", "transaction hash not found in the ledger by the ingest reader")
	}
	res.Evidence["txMetaVersion"] = tx.UnsafeMeta.V

	// 3. Run wallet-backend's own indexer over the transaction.
	idx := newIndexer(passphrase)
	buf := indexer.NewIndexerBuffer()
	if _, err := idx.ProcessLedgerTransactions(context.Background(), []ingest.LedgerTransaction{*tx}, buf); err != nil {
		return dropped(res, "indexer", fmt.Sprintf("indexer fails on the transaction: %v", err))
	}
	scs := buf.GetStateChanges()
	res.Evidence["stateChanges"] = summarize(scs)

	// 4. Encode what BatchCopy would write and read it back.
	for i, sc := range scs {
		res.StoredValues = append(res.StoredValues, storeStateChange(i, sc)...)
	}
	for _, p := range participantSet(buf) {
		res.StoredValues = append(res.StoredValues, roundTrip("participants", "account_id (transactions_accounts / operations_accounts)", reflect.ValueOf(types.AddressBytea(p))))
	}

	// 5. Entry-specific check of the shape this entry was captured for.
	switch id {
	case "cap73-sac-created-trustline":
		return checkSACTrustline(res, dir, scs, buf)
	case "cap40-signed-payload-signer-max-payload":
		return checkSignedPayloadSigner(res, scs, expected)
	case "cap27-muxed-source-and-destination-id-extremes":
		return checkMuxedPayment(res, tx, scs)
	case "cap67-sac-transfer-to-muxed-address-max-id":
		return checkMuxedSACTransfer(res, dir, scs)
	default:
		return genericCheck(res)
	}
}

// getLedger returns the entry's LedgerCloseMeta. A fetch error ends the run; the
// returned error is always the XDR decode error, so both inputs report the same detail.
func getLedger(rpcURL, dir string, seq uint32) (xdr.LedgerCloseMeta, error) {
	var lcm xdr.LedgerCloseMeta
	if rpcURL == "" {
		b64, err := os.ReadFile(filepath.Join(dir, "ledger-close-meta.b64"))
		must(err)
		return lcm, xdr.SafeUnmarshalBase64(strings.TrimSpace(string(b64)), &lcm)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// BufferSize 1: one getLedgers call per entry, holding one ledger at a time.
	backend := ledgerbackend.NewRPCLedgerBackend(ledgerbackend.RPCLedgerBackendOptions{RPCServerURL: rpcURL, BufferSize: 1})
	defer backend.Close()
	must(backend.PrepareRange(ctx, ledgerbackend.BoundedRange(seq, seq))) // calls getHealth and getLedgers
	lcm, err := backend.GetLedger(ctx, seq)
	const decodeFailed = "failed to unmarshal cached ledger: " // RPCLedgerBackend's wrapper for XDR errors
	if err != nil && !strings.HasPrefix(err.Error(), decodeFailed) {
		must(err)
	}
	if inner := errors.Unwrap(err); inner != nil {
		err = inner
	}
	return lcm, err
}

// --- entry checks ---------------------------------------------------------------

func checkSACTrustline(res entryResult, dir string, scs []types.StateChange, buf *indexer.IndexerBuffer) entryResult {
	var capture struct {
		Asset   string `json:"asset"`
		Trustor string `json:"trustor"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "capture.json"))
	must(err)
	must(json.Unmarshal(raw, &capture))
	res.Evidence["trustor"] = capture.Trustor
	res.Evidence["asset"] = capture.Asset

	// trustline_balances path (TrustlinesProcessor), reported as context.
	var balanceRows []string
	for k, c := range buf.GetTrustlineChanges() {
		balanceRows = append(balanceRows, fmt.Sprintf("%+v -> %+v", k, c))
	}
	sort.Strings(balanceRows)
	res.Evidence["trustlineBalanceChanges"] = balanceRows

	var add, auth bool
	for _, sc := range scs {
		if string(sc.AccountID) != capture.Trustor {
			continue
		}
		if sc.StateChangeCategory == types.StateChangeCategoryTrustline && sc.StateChangeReason == types.StateChangeReasonAdd {
			add = true
		}
		if sc.StateChangeCategory == types.StateChangeCategoryBalanceAuthorization {
			auth = true
		}
	}
	res.Evidence["trustlineAddStateChange"] = add
	res.Evidence["balanceAuthorizationStateChange"] = auth
	if !add {
		res.Outcome, res.Stage = "dropped", "state_changes"
		res.Detail = fmt.Sprintf("no TRUSTLINE/ADD state change for the SAC-created trustline (trustline_balances rows: %d)", len(balanceRows))
		return res
	}
	if bad := firstBad(res.StoredValues); bad != nil {
		return classifyBad(res, bad)
	}
	res.Outcome, res.Stage = "pass", "state_changes"
	res.Detail = fmt.Sprintf("TRUSTLINE/ADD recorded for the trustor (BALANCE_AUTHORIZATION: %v)", auth)
	return res
}

func checkSignedPayloadSigner(res entryResult, scs []types.StateChange, expected map[string]string) entryResult {
	var want string
	for p, v := range expected {
		if strings.HasSuffix(p, "/signers/0/key") {
			want = v
		}
	}
	res.Evidence["expectedSignerKey"] = want
	var found []storedValue
	for i, sc := range scs {
		if sc.StateChangeCategory != types.StateChangeCategorySigner {
			continue
		}
		f := reflect.ValueOf(sc).FieldByName("SignerAccountID")
		if !f.IsValid() {
			continue
		}
		if stringOf(f) == want {
			found = append(found, roundTrip(fmt.Sprintf("state_changes[%d] SIGNER/%s", i, sc.StateChangeReason), "signer_account_id", f))
		}
	}
	if len(found) == 0 {
		res.Outcome, res.Stage = "dropped", "state_changes"
		res.Detail = "no SIGNER state change carries the signed-payload signer key"
		return res
	}
	res.Evidence["signerRoundTrip"] = found
	for _, f := range found {
		switch f.Status {
		case "exact":
		case "encode-error":
			res.Outcome, res.Stage = "dropped", "persist-encode"
			res.Detail = "signer_account_id fails to encode, so the COPY and the whole persist transaction fail and ingestion halts at this ledger: " + f.Err
			return res
		default:
			res.Outcome, res.Stage = "mishandled", "persist-roundtrip"
			res.Detail = fmt.Sprintf("signer_account_id is stored as %d bytes and reads back as %q (%s), not the 64-byte-payload P key", f.OutBytes, f.Out, f.Status)
			return res
		}
	}
	if bad := firstBad(res.StoredValues); bad != nil {
		return classifyBad(res, bad)
	}
	res.Outcome, res.Stage = "pass", "persist-roundtrip"
	res.Detail = "SIGNER state change stores the full P key and reads it back unchanged"
	return res
}

func checkMuxedPayment(res entryResult, tx *ingest.LedgerTransaction, scs []types.StateChange) entryResult {
	src := tx.Envelope.SourceAccount()
	op := tx.Envelope.Operations()[0]
	if op.SourceAccount != nil {
		src = *op.SourceAccount
	}
	srcM, _ := src.GetAddress()
	srcG := src.ToAccountId().Address()
	var dstM, dstG string
	if p, ok := op.Body.GetPaymentOp(); ok {
		dstM, _ = p.Destination.GetAddress()
		dstG = p.Destination.ToAccountId().Address()
	}
	res.Evidence["source"] = map[string]string{"muxed": srcM, "base": srcG}
	res.Evidence["destination"] = map[string]string{"muxed": dstM, "base": dstG}

	var debit, credit bool
	var toMuxed []string
	for _, sc := range scs {
		if sc.StateChangeCategory != types.StateChangeCategoryBalance {
			continue
		}
		if string(sc.AccountID) == srcG && sc.StateChangeReason == types.StateChangeReasonDebit && sc.OperationID != 0 {
			debit = true
		}
		if string(sc.AccountID) == dstG && sc.StateChangeReason == types.StateChangeReasonCredit {
			credit = true
			if sc.ToMuxedID.Valid {
				toMuxed = append(toMuxed, sc.ToMuxedID.String)
			}
		}
	}
	res.Evidence["debitForBaseSource"] = debit
	res.Evidence["creditForBaseDestination"] = credit
	res.Evidence["creditToMuxedID"] = toMuxed
	var muxedIn []storedValue
	for _, v := range res.StoredValues {
		if strings.HasPrefix(v.In, "M") {
			muxedIn = append(muxedIn, v)
		}
	}
	res.Evidence["muxedValuesReachingAddressColumns"] = len(muxedIn)
	if !debit || !credit {
		res.Outcome, res.Stage = "dropped", "state_changes"
		res.Detail = fmt.Sprintf("payment balance state changes missing (debit=%v credit=%v)", debit, credit)
		return res
	}
	if bad := firstBad(res.StoredValues); bad != nil {
		return classifyBad(res, bad)
	}
	res.Outcome, res.Stage = "pass", "persist-roundtrip"
	res.Detail = fmt.Sprintf("payment indexed under base accounts; %d M-address values reached an address column; all stored addresses read back", len(muxedIn))
	return res
}

// checkMuxedSACTransfer: a SAC transfer() whose `to` is an M address (CAP-0067). Core emits the
// SEP-41 transfer event with the base G account in the topic and `to_muxed_id` in the data map.
func checkMuxedSACTransfer(res entryResult, dir string, scs []types.StateChange) entryResult {
	var capture struct {
		From               string `json:"from"`
		DestinationAccount string `json:"destinationAccount"`
		MuxedDestinationID string `json:"muxedDestinationId"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "capture.json"))
	must(err)
	must(json.Unmarshal(raw, &capture))
	res.Evidence["expected"] = capture

	var debit, credit bool
	var toMuxed []string
	for _, sc := range scs {
		if sc.StateChangeCategory != types.StateChangeCategoryBalance {
			continue
		}
		if string(sc.AccountID) == capture.From && sc.StateChangeReason == types.StateChangeReasonDebit && sc.OperationID != 0 {
			debit = true
		}
		if string(sc.AccountID) == capture.DestinationAccount && sc.StateChangeReason == types.StateChangeReasonCredit {
			credit = true
			if sc.ToMuxedID.Valid {
				toMuxed = append(toMuxed, sc.ToMuxedID.String)
			}
		}
	}
	res.Evidence["debitForSender"] = debit
	res.Evidence["creditForBaseDestination"] = credit
	res.Evidence["creditToMuxedID"] = toMuxed
	if !debit || !credit {
		res.Outcome, res.Stage = "dropped", "state_changes"
		res.Detail = fmt.Sprintf("transfer balance state changes missing (debit=%v credit=%v)", debit, credit)
		return res
	}
	if bad := firstBad(res.StoredValues); bad != nil {
		return classifyBad(res, bad)
	}
	if len(toMuxed) == 0 {
		res.Outcome, res.Stage = "mishandled", "state_changes"
		res.Detail = "credit recorded for the base account but to_muxed_id " + capture.MuxedDestinationID + " is not kept"
		return res
	}
	for _, id := range toMuxed {
		if id != capture.MuxedDestinationID {
			res.Outcome, res.Stage = "mishandled", "state_changes"
			res.Detail = fmt.Sprintf("to_muxed_id stored as %q, expected %q", id, capture.MuxedDestinationID)
			return res
		}
	}
	res.Outcome, res.Stage = "pass", "persist-roundtrip"
	res.Detail = "credit recorded for the base account with to_muxed_id " + capture.MuxedDestinationID + "; all stored addresses read back"
	return res
}

func genericCheck(res entryResult) entryResult {
	if bad := firstBad(res.StoredValues); bad != nil {
		return classifyBad(res, bad)
	}
	res.Outcome, res.Stage, res.Detail = "pass", "persist-roundtrip", "all stored addresses read back"
	return res
}

func firstBad(vals []storedValue) *storedValue {
	for i := range vals {
		switch vals[i].Status {
		case "exact", "reduced-to-base":
		default:
			return &vals[i]
		}
	}
	return nil
}

func classifyBad(res entryResult, bad *storedValue) entryResult {
	if bad.Status == "encode-error" {
		res.Outcome, res.Stage = "dropped", "persist-encode"
		res.Detail = fmt.Sprintf("%s %s fails to encode, the persist transaction rolls back: %s", bad.Where, bad.Column, bad.Err)
		return res
	}
	res.Outcome, res.Stage = "mishandled", "persist-roundtrip"
	res.Detail = fmt.Sprintf("%s %s: %q stored as %d bytes, reads back as %q (%s)", bad.Where, bad.Column, bad.In, bad.OutBytes, bad.Out, bad.Status)
	return res
}

// --- storage round-trip ---------------------------------------------------------

func storeStateChange(i int, sc types.StateChange) []storedValue {
	var out []storedValue
	v := reflect.ValueOf(sc)
	t := v.Type()
	where := fmt.Sprintf("state_changes[%d] %s/%s", i, sc.StateChangeCategory, sc.StateChangeReason)
	for f := 0; f < t.NumField(); f++ {
		ft := t.Field(f)
		if !strings.HasSuffix(ft.Type.Name(), "Bytea") || strings.Contains(ft.Type.Name(), "Hash") || strings.Contains(ft.Type.Name(), "XDR") {
			continue
		}
		fv := v.Field(f)
		if stringOf(fv) == "" {
			continue
		}
		col := ft.Tag.Get("db")
		if col == "" {
			col = ft.Name
		}
		out = append(out, roundTrip(where, col, fv))
	}
	return out
}

// roundTrip runs Value() (what BatchCopy hands to pgx) and then Scan() into a fresh
// value of the same Go type (what every read path does).
func roundTrip(where, column string, fv reflect.Value) storedValue {
	sv := storedValue{Where: where, Column: column, GoType: fv.Type().Name(), In: stringOf(fv)}
	valuer, ok := fv.Interface().(driver.Valuer)
	if !ok {
		sv.Status, sv.Err = "encode-error", "type is not a driver.Valuer"
		return sv
	}
	dv, err := valuer.Value()
	if err != nil {
		sv.Status, sv.Err = "encode-error", err.Error()
		return sv
	}
	b, _ := dv.([]byte)
	sv.OutBytes = len(b)
	back := reflect.New(fv.Type())
	scanner, ok := back.Interface().(sql.Scanner)
	if !ok {
		sv.Status, sv.Err = "scan-error", "type is not a sql.Scanner"
		return sv
	}
	if err := scanner.Scan(b); err != nil {
		sv.Status, sv.Err = "scan-error", err.Error()
		return sv
	}
	sv.Out = stringOf(back.Elem())
	switch {
	case sv.Out == sv.In:
		sv.Status = "exact"
	case !decodes(sv.Out):
		sv.Status = "undecodable"
	case strings.HasPrefix(sv.In, "M") && sv.Out == baseOfMuxed(sv.In):
		sv.Status = "reduced-to-base"
	default:
		sv.Status = "altered"
	}
	return sv
}

func decodes(s string) bool {
	_, _, err := strkey.DecodeAny(s)
	return err == nil
}

func baseOfMuxed(m string) string {
	var ma xdr.MuxedAccount
	if err := ma.SetAddress(m); err != nil {
		return ""
	}
	return ma.ToAccountId().Address()
}

func stringOf(v reflect.Value) string {
	if s, ok := v.Interface().(fmt.Stringer); ok {
		return s.String()
	}
	if v.Kind() == reflect.String {
		return v.String()
	}
	return ""
}

func participantSet(buf *indexer.IndexerBuffer) []string {
	set := map[string]struct{}{}
	for _, ps := range buf.GetTransactionsParticipants() {
		for p := range ps {
			set[p] = struct{}{}
		}
	}
	for _, ps := range buf.GetOperationsParticipants() {
		for p := range ps {
			set[p] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func summarize(scs []types.StateChange) []string {
	out := make([]string, 0, len(scs))
	for _, sc := range scs {
		s := fmt.Sprintf("%s/%s account=%s", sc.StateChangeCategory, sc.StateChangeReason, sc.AccountID)
		if f := reflect.ValueOf(sc).FieldByName("SignerAccountID"); f.IsValid() && stringOf(f) != "" {
			s += " signer=" + stringOf(f)
		}
		if f := reflect.ValueOf(sc).FieldByName("TokenID"); f.IsValid() && stringOf(f) != "" {
			s += " token=" + stringOf(f)
		}
		if sc.ToMuxedID.Valid {
			s += " toMuxedId=" + sc.ToMuxedID.String
		}
		out = append(out, s)
	}
	return out
}

// newIndexer calls indexer.NewIndexer across commits whose signatures differ only by
// trailing options added later (main adds collectContractData bool); those get their
// zero value, which is the live-ingest default for this processor set.
func newIndexer(passphrase string) *indexer.Indexer {
	fn := reflect.ValueOf(indexer.NewIndexer)
	args := []reflect.Value{
		reflect.ValueOf(passphrase),
		reflect.ValueOf(pond.NewPool(1)),
		reflect.ValueOf(metrics.NewMetrics(prometheus.NewRegistry()).Ingestion),
	}
	for i := len(args); i < fn.Type().NumIn(); i++ {
		args = append(args, reflect.Zero(fn.Type().In(i)))
	}
	out := fn.Call(args)
	if err, _ := out[1].Interface().(error); err != nil {
		must(err)
	}
	return out[0].Interface().(*indexer.Indexer)
}

func dropped(res entryResult, stage, detail string) entryResult {
	res.Outcome, res.Stage, res.Detail = "dropped", stage, detail
	return res
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
