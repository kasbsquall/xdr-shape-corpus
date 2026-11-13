package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/clients/rpcclient"
	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/xdr"
)

const corpusDir = "../../corpus"

// capture is what tools/capture-testnet.mjs recorded from the real testnet RPC.
type capture struct {
	ID              string `json:"id"`
	Ledger          uint32 `json:"ledger"`
	LedgerCloseTime string `json:"ledgerCloseTime"`
}

func start(t *testing.T) (*server, *httptest.Server, map[uint32]capture) {
	t.Helper()
	s, err := loadCorpus(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s) // binds 127.0.0.1
	t.Cleanup(ts.Close)
	caps := map[uint32]capture{}
	paths, _ := filepath.Glob(filepath.Join(corpusDir, "entries", "*", "capture.json"))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var c capture
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		caps[c.Ledger] = c
	}
	if len(caps) != len(s.ledgers) {
		t.Fatalf("%d capture.json files, %d served ledgers", len(caps), len(s.ledgers))
	}
	return s, ts, caps
}

// Walks the whole corpus one ledger per page with go-stellar-sdk's RPC client and
// checks every field of every ledger against the corpus files.
func TestGetLedgersPaginationWithSDKClient(t *testing.T) {
	s, ts, caps := start(t)
	ctx := context.Background()
	cli := rpcclient.NewClient(ts.URL, nil)

	health, err := cli.GetHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != "healthy" || health.OldestLedger != s.ledgers[0].seq || health.LatestLedger != s.ledgers[len(s.ledgers)-1].seq {
		t.Fatalf("getHealth: %+v", health)
	}

	req := protocol.GetLedgersRequest{StartLedger: health.OldestLedger, Pagination: &protocol.LedgerPaginationOptions{Limit: 1}}
	var seen []uint32
	for page := 0; ; page++ {
		resp, err := cli.GetLedgers(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.OldestLedger != health.OldestLedger || resp.LatestLedger != health.LatestLedger ||
			resp.OldestLedgerCloseTime != health.OldestLedgerCloseTime || resp.LatestLedgerCloseTime != health.LatestLedgerCloseTime {
			t.Fatalf("page %d range: %+v", page, resp)
		}
		if len(resp.Ledgers) == 0 {
			if resp.Cursor != req.Pagination.Cursor {
				t.Fatalf("empty page should echo cursor %q, got %q", req.Pagination.Cursor, resp.Cursor)
			}
			break
		}
		if len(resp.Ledgers) != 1 {
			t.Fatalf("limit 1 returned %d ledgers", len(resp.Ledgers))
		}
		l := resp.Ledgers[0]
		if resp.Cursor != strconv.FormatUint(uint64(l.Sequence), 10) {
			t.Fatalf("cursor %q for ledger %d", resp.Cursor, l.Sequence)
		}
		checkLedger(t, l, caps[l.Sequence])
		seen = append(seen, l.Sequence)
		req = protocol.GetLedgersRequest{Pagination: &protocol.LedgerPaginationOptions{Cursor: resp.Cursor, Limit: 1}}
	}
	if len(seen) != len(s.ledgers) {
		t.Fatalf("walked %v, want %d ledgers", seen, len(s.ledgers))
	}
	for i, ref := range s.ledgers {
		if seen[i] != ref.seq {
			t.Fatalf("walked %v, not in sequence order", seen)
		}
	}

	// Default limit returns every corpus ledger in one page.
	all, err := cli.GetLedgers(ctx, protocol.GetLedgersRequest{StartLedger: health.OldestLedger})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Ledgers) != len(s.ledgers) {
		t.Fatalf("default limit returned %d ledgers", len(all.Ledgers))
	}

	latest, err := cli.GetLatestLedger(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Sequence != health.LatestLedger || latest.ProtocolVersion == 0 || latest.Hash != all.Ledgers[len(all.Ledgers)-1].Hash {
		t.Fatalf("getLatestLedger: sequence %d, protocol %d, hash %s", latest.Sequence, latest.ProtocolVersion, latest.Hash)
	}
}

func checkLedger(t *testing.T, l protocol.LedgerInfo, c capture) {
	t.Helper()
	file, err := os.ReadFile(filepath.Join(corpusDir, "entries", c.ID, "ledger-close-meta.b64"))
	if err != nil {
		t.Fatal(err)
	}
	if l.LedgerMetadata != strings.TrimSpace(string(file)) {
		t.Fatalf("%s: metadataXdr is not the captured file", c.ID)
	}
	var lcm xdr.LedgerCloseMeta
	if err := xdr.SafeUnmarshalBase64(l.LedgerMetadata, &lcm); err != nil {
		t.Fatal(err)
	}
	var hdr xdr.LedgerHeaderHistoryEntry
	if err := xdr.SafeUnmarshalBase64(l.LedgerHeader, &hdr); err != nil {
		t.Fatalf("%s: headerXdr is not a LedgerHeaderHistoryEntry: %v", c.ID, err)
	}
	metaRaw, _ := base64.StdEncoding.DecodeString(l.LedgerMetadata)
	hdrRaw, _ := base64.StdEncoding.DecodeString(l.LedgerHeader)
	switch {
	case lcm.LedgerSequence() != l.Sequence || uint32(hdr.Header.LedgerSeq) != l.Sequence:
		t.Fatalf("%s: sequence %d, LCM %d, header %d", c.ID, l.Sequence, lcm.LedgerSequence(), hdr.Header.LedgerSeq)
	case l.Hash != lcm.LedgerHash().HexString() || l.Hash != hdr.Hash.HexString():
		t.Fatalf("%s: hash %s does not match the LCM", c.ID, l.Hash)
	case !bytes.Contains(metaRaw, hdrRaw):
		t.Fatalf("%s: headerXdr bytes are not a slice of the LedgerCloseMeta", c.ID)
	case strconv.FormatInt(l.LedgerCloseTime, 10) != c.LedgerCloseTime:
		t.Fatalf("%s: ledgerCloseTime %d, testnet RPC returned %s at capture", c.ID, l.LedgerCloseTime, c.LedgerCloseTime)
	}
}

// Pins the wire field names, including which close times are JSON strings.
func TestWireFieldNames(t *testing.T) {
	s, ts, _ := start(t)
	body := `{"jsonrpc":"2.0","id":7,"method":"getLedgers","params":{"startLedger":` +
		strconv.FormatUint(uint64(s.ledgers[0].seq), 10) + `,"pagination":{"limit":1},"xdrFormat":"base64"}}`
	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  map[string]json.RawMessage
	}
	post(t, ts.URL, body, &resp)
	if resp.JSONRPC != "2.0" || string(resp.ID) != "7" {
		t.Fatalf("envelope: %+v", resp)
	}
	wantKeys(t, resp.Result, "ledgers", "latestLedger", "latestLedgerCloseTime", "oldestLedger", "oldestLedgerCloseTime", "cursor")
	var ledgers []map[string]json.RawMessage
	if err := json.Unmarshal(resp.Result["ledgers"], &ledgers); err != nil || len(ledgers) != 1 {
		t.Fatalf("ledgers: %v %s", err, resp.Result["ledgers"])
	}
	wantKeys(t, ledgers[0], "hash", "sequence", "ledgerCloseTime", "headerXdr", "metadataXdr")
	if !strings.HasPrefix(string(ledgers[0]["ledgerCloseTime"]), `"`) {
		t.Fatalf("ledgerCloseTime should be a JSON string, got %s", ledgers[0]["ledgerCloseTime"])
	}
	if strings.HasPrefix(string(resp.Result["latestLedgerCloseTime"]), `"`) {
		t.Fatalf("latestLedgerCloseTime should be a JSON number, got %s", resp.Result["latestLedgerCloseTime"])
	}
}

// The ledger backend wallet-backend uses for RPC ingestion reads every corpus ledger.
func TestRPCLedgerBackend(t *testing.T) {
	s, ts, _ := start(t)
	for _, ref := range s.ledgers {
		b := ledgerbackend.NewRPCLedgerBackend(ledgerbackend.RPCLedgerBackendOptions{RPCServerURL: ts.URL, BufferSize: 1})
		ctx := context.Background()
		if err := b.PrepareRange(ctx, ledgerbackend.BoundedRange(ref.seq, ref.seq)); err != nil {
			t.Fatal(err)
		}
		lcm, err := b.GetLedger(ctx, ref.seq)
		if err != nil {
			t.Fatal(err)
		}
		if lcm.LedgerSequence() != ref.seq {
			t.Fatalf("got ledger %d, want %d", lcm.LedgerSequence(), ref.seq)
		}
		b.Close()
	}
}

func TestRejectedRequests(t *testing.T) {
	s, ts, _ := start(t)
	oldest := strconv.FormatUint(uint64(s.ledgers[0].seq), 10)
	for name, c := range map[string]struct {
		body string
		code int
	}{
		"before oldest":        {`{"jsonrpc":"2.0","id":1,"method":"getLedgers","params":{"startLedger":1}}`, codeInvalidParams},
		"cursor and start":     {`{"jsonrpc":"2.0","id":1,"method":"getLedgers","params":{"startLedger":` + oldest + `,"pagination":{"cursor":"` + oldest + `"}}}`, codeInvalidParams},
		"limit over max":       {`{"jsonrpc":"2.0","id":1,"method":"getLedgers","params":{"startLedger":` + oldest + `,"pagination":{"limit":201}}}`, codeInvalidParams},
		"json format":          {`{"jsonrpc":"2.0","id":1,"method":"getLedgers","params":{"startLedger":` + oldest + `,"xdrFormat":"json"}}`, codeInvalidParams},
		"unknown method":       {`{"jsonrpc":"2.0","id":1,"method":"getEvents"}`, codeMethodNotFound},
		"not json-rpc 2.0":     {`{"id":1,"method":"getHealth"}`, codeInvalidRequest},
		"not a request object": {`[1,2]`, codeParseError},
	} {
		var resp rpcResponse
		post(t, ts.URL, c.body, &resp)
		if resp.Error == nil || resp.Error.Code != c.code || resp.Result != nil {
			t.Errorf("%s: got %+v, want error code %d", name, resp, c.code)
		}
	}
}

func TestCheckLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:0": true, "[::1]:8000": true,
		"0.0.0.0:8000": false, ":8000": false, "localhost:8000": false, "192.168.1.5:80": false,
	} {
		if err := checkLoopback(addr); (err == nil) != ok {
			t.Errorf("checkLoopback(%q) = %v", addr, err)
		}
	}
}

func post(t *testing.T, url, body string, out any) {
	t.Helper()
	r, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

func wantKeys(t *testing.T, m map[string]json.RawMessage, keys ...string) {
	t.Helper()
	if len(m) != len(keys) {
		t.Fatalf("got keys %v, want %v", mapKeys(m), keys)
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing key %q in %v", k, mapKeys(m))
		}
	}
}

func mapKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
