// Command mockrpc serves the xdr-kit corpus as a mock Stellar RPC endpoint, so an
// indexer can ingest corpus ledgers through the same getLedgers call it uses
// against a real RPC server.
//
//	go run . -corpus ../../corpus [-addr 127.0.0.1:0]
//
// It speaks JSON-RPC 2.0 over HTTP POST and implements getLedgers, getLatestLedger
// and getHealth with the request and response types of go-stellar-sdk/protocols/rpc,
// the package stellar-rpc itself uses. Only base64 xdrFormat is supported.
//
// The mock holds only the corpus ledgers, so its sequence range is sparse: a page
// holds the next corpus ledgers at or after startLedger (or after the cursor).
// At startup it keeps one small index row per ledger (sequence, close time, file
// path); each request reads and decodes at most `limit` ledger files.
//
// It listens on a loopback address only and prints its URL as the first line of
// stdout.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Same defaults as stellar-rpc's default-ledgers-limit and max-ledgers-limit options.
const (
	defaultLimit = 50
	maxLimit     = 200
)

// JSON-RPC 2.0 error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

type ledgerRef struct {
	seq       uint32
	closeTime int64
	path      string
}

type server struct {
	ledgers []ledgerRef // sorted by sequence, one row per distinct ledger
}

func main() {
	corpusDir := flag.String("corpus", "", "path to xdr-kit/corpus")
	addr := flag.String("addr", "127.0.0.1:0", "listen address; must be a loopback IP")
	flag.Parse()
	if *corpusDir == "" {
		fail(errors.New("-corpus is required"))
	}
	if err := checkLoopback(*addr); err != nil {
		fail(err)
	}
	s, err := loadCorpus(*corpusDir)
	if err != nil {
		fail(err)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fail(err)
	}
	fmt.Printf("http://%s\n", ln.Addr())
	first, last := s.ledgers[0], s.ledgers[len(s.ledgers)-1]
	fmt.Fprintf(os.Stderr, "mockrpc: %d ledgers, %d..%d\n", len(s.ledgers), first.seq, last.seq)
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	fail(srv.Serve(ln))
}

func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("refusing to listen on %q: use a loopback IP such as 127.0.0.1", addr)
	}
	return nil
}

// loadCorpus indexes the LedgerCloseMeta file of every manifest entry. Each file is
// decoded once to check its sequence against the manifest and read its close time.
func loadCorpus(corpusDir string) (*server, error) {
	raw, err := os.ReadFile(filepath.Join(corpusDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m struct {
		Entries []struct {
			ID     string `json:"id"`
			Source struct {
				Ledger uint32 `json:"ledger"`
			} `json:"source"`
			Files []struct {
				Path    string `json:"path"`
				XDRType string `json:"xdrType"`
				SHA256  string `json:"sha256"`
			} `json:"files"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	root := filepath.Dir(filepath.Clean(corpusDir)) // manifest paths start at the repo root
	bySeq := map[uint32]string{}                    // sequence -> sha256, to catch conflicting files
	s := &server{}
	for _, e := range m.Entries {
		for _, f := range e.Files {
			if f.XDRType != "LedgerCloseMeta" {
				continue
			}
			if sha, seen := bySeq[e.Source.Ledger]; seen {
				if sha != f.SHA256 {
					return nil, fmt.Errorf("%s: ledger %d already served from a different file", e.ID, e.Source.Ledger)
				}
				continue
			}
			path := filepath.Join(root, filepath.FromSlash(f.Path))
			info, _, err := readLedger(path)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.ID, err)
			}
			if info.Sequence != e.Source.Ledger {
				return nil, fmt.Errorf("%s: file holds ledger %d, manifest says %d", e.ID, info.Sequence, e.Source.Ledger)
			}
			bySeq[info.Sequence] = f.SHA256
			s.ledgers = append(s.ledgers, ledgerRef{seq: info.Sequence, closeTime: info.LedgerCloseTime, path: path})
		}
	}
	if len(s.ledgers) == 0 {
		return nil, errors.New("manifest lists no LedgerCloseMeta files")
	}
	sort.Slice(s.ledgers, func(i, j int) bool { return s.ledgers[i].seq < s.ledgers[j].seq })
	return s, nil
}

// readLedger builds the getLedgers item for one LedgerCloseMeta file. metadataXdr is
// the file content as captured; headerXdr is its LedgerHeaderHistoryEntry.
func readLedger(path string) (protocol.LedgerInfo, uint32, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return protocol.LedgerInfo{}, 0, err
	}
	b64 := strings.TrimSpace(string(raw))
	var lcm xdr.LedgerCloseMeta
	if err := xdr.SafeUnmarshalBase64(b64, &lcm); err != nil {
		return protocol.LedgerInfo{}, 0, fmt.Errorf("decoding %s: %w", path, err)
	}
	header, err := xdr.MarshalBase64(lcm.LedgerHeaderHistoryEntry())
	if err != nil {
		return protocol.LedgerInfo{}, 0, err
	}
	return protocol.LedgerInfo{
		Hash:            lcm.LedgerHash().HexString(),
		Sequence:        lcm.LedgerSequence(),
		LedgerCloseTime: lcm.LedgerCloseTime(),
		LedgerHeader:    header,
		LedgerMetadata:  b64,
	}, lcm.ProtocolVersion(), nil
}

// --- JSON-RPC ---------------------------------------------------------------------

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "JSON-RPC requests must use POST", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null")}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	switch {
	case err != nil:
		resp.Error = &rpcError{codeParseError, err.Error()}
	case json.Unmarshal(body, &req) != nil:
		resp.Error = &rpcError{codeParseError, "body is not a JSON-RPC request object"}
	case req.JSONRPC != "2.0" || req.Method == "":
		resp.Error = &rpcError{codeInvalidRequest, `expected "jsonrpc": "2.0" and a method`}
	default:
		if len(req.ID) > 0 {
			resp.ID = req.ID
		}
		resp.Result, err = s.call(req.Method, req.Params)
		if err != nil {
			resp.Result = nil
			if !errors.As(err, &resp.Error) {
				resp.Error = &rpcError{codeInternalError, err.Error()}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *server) call(method string, params json.RawMessage) (any, error) {
	switch method {
	case protocol.GetHealthMethodName:
		return s.getHealth(), nil
	case protocol.GetLatestLedgerMethodName:
		return s.getLatestLedger()
	case protocol.GetLedgersMethodName:
		var req protocol.GetLedgersRequest
		if len(params) > 0 && string(params) != "null" {
			if err := json.Unmarshal(params, &req); err != nil {
				return nil, &rpcError{codeInvalidParams, err.Error()}
			}
		}
		return s.getLedgers(req)
	default:
		return nil, &rpcError{codeMethodNotFound, "method not found: " + method}
	}
}

func (s *server) getHealth() protocol.GetHealthResponse {
	first, last := s.ledgers[0], s.ledgers[len(s.ledgers)-1]
	return protocol.GetHealthResponse{
		Status:                "healthy",
		LatestLedger:          last.seq,
		LatestLedgerCloseTime: last.closeTime,
		OldestLedger:          first.seq,
		OldestLedgerCloseTime: first.closeTime,
		LedgerRetentionWindow: last.seq - first.seq + 1,
	}
}

func (s *server) getLatestLedger() (protocol.GetLatestLedgerResponse, error) {
	info, version, err := readLedger(s.ledgers[len(s.ledgers)-1].path)
	if err != nil {
		return protocol.GetLatestLedgerResponse{}, err
	}
	return protocol.GetLatestLedgerResponse{
		Hash:            info.Hash,
		ProtocolVersion: version,
		Sequence:        info.Sequence,
		LedgerCloseTime: info.LedgerCloseTime,
		LedgerHeader:    info.LedgerHeader,
		LedgerMetadata:  info.LedgerMetadata,
	}, nil
}

// getLedgers follows stellar-rpc: startLedger and cursor are exclusive, the cursor is
// the sequence of the last ledger returned, and the next page starts after it.
func (s *server) getLedgers(req protocol.GetLedgersRequest) (protocol.GetLedgersResponse, error) {
	first, last := s.ledgers[0], s.ledgers[len(s.ledgers)-1]
	if req.Format == protocol.FormatJSON {
		return protocol.GetLedgersResponse{}, &rpcError{codeInvalidParams, "the mock serves xdrFormat base64 only"}
	}
	if err := req.Validate(maxLimit, protocol.LedgerSeqRange{FirstLedger: first.seq, LastLedger: last.seq}); err != nil {
		return protocol.GetLedgersResponse{}, &rpcError{codeInvalidParams, err.Error()}
	}
	start, limit := req.StartLedger, uint(defaultLimit)
	if p := req.Pagination; p != nil {
		if p.Limit > 0 {
			limit = p.Limit
		}
		if p.Cursor != "" {
			c, err := strconv.ParseUint(p.Cursor, 10, 32)
			if err != nil || uint32(c)+1 < first.seq {
				return protocol.GetLedgersResponse{}, &rpcError{codeInvalidParams, fmt.Sprintf("invalid cursor %q", p.Cursor)}
			}
			start = uint32(c) + 1
		}
	}

	resp := protocol.GetLedgersResponse{
		Ledgers:               []protocol.LedgerInfo{},
		LatestLedger:          last.seq,
		LatestLedgerCloseTime: last.closeTime,
		OldestLedger:          first.seq,
		OldestLedgerCloseTime: first.closeTime,
	}
	for i := sort.Search(len(s.ledgers), func(i int) bool { return s.ledgers[i].seq >= start }); i < len(s.ledgers) && uint(len(resp.Ledgers)) < limit; i++ {
		info, _, err := readLedger(s.ledgers[i].path)
		if err != nil {
			return protocol.GetLedgersResponse{}, err
		}
		resp.Ledgers = append(resp.Ledgers, info)
	}
	switch {
	case len(resp.Ledgers) > 0:
		resp.Cursor = strconv.FormatUint(uint64(resp.Ledgers[len(resp.Ledgers)-1].Sequence), 10)
	case req.Pagination != nil && req.Pagination.Cursor != "":
		resp.Cursor = req.Pagination.Cursor
	default:
		resp.Cursor = strconv.FormatUint(uint64(start-1), 10)
	}
	return resp, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mockrpc:", err)
	os.Exit(1)
}
