// Conformance runner for go-stellar-sdk.
// Decodes every corpus file and every sighting vector, re-encodes it and compares bytes,
// then runs the behaviour check of each sighting.
// Usage: go run . <kit-dir> <out.json>
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

type result struct {
	Entry              string `json:"entry,omitempty"`
	Sighting           string `json:"sighting,omitempty"`
	Path               string `json:"path,omitempty"`
	File               string `json:"file,omitempty"`
	XDRType            string `json:"xdrType"`
	Decode             string `json:"decode"`
	Error              string `json:"error,omitempty"`
	RoundTripIdentical bool   `json:"roundTripIdentical"`
	Check              string `json:"check,omitempty"`
	Result             string `json:"result,omitempty"`
	Detail             string `json:"detail,omitempty"`
}

// apiResult is the outcome of an API check, which has no vector to decode.
type apiResult struct {
	Sighting string `json:"sighting"`
	Check    string `json:"check"`
	Result   string `json:"result"`
	Detail   string `json:"detail"`
}

func newValue(xdrType string) (any, error) {
	switch xdrType {
	case "TransactionEnvelope":
		return &xdr.TransactionEnvelope{}, nil
	case "TransactionResult":
		return &xdr.TransactionResult{}, nil
	case "TransactionMeta":
		return &xdr.TransactionMeta{}, nil
	case "LedgerCloseMeta":
		return &xdr.LedgerCloseMeta{}, nil
	case "ScVal":
		return &xdr.ScVal{}, nil
	}
	return nil, fmt.Errorf("no Go type mapped for %s", xdrType)
}

func roundTrip(xdrType, b64 string) (result, any) {
	r := result{XDRType: xdrType}
	v, err := newValue(xdrType)
	if err != nil {
		r.Decode, r.Error = "error", err.Error()
		return r, nil
	}
	if err := xdr.SafeUnmarshalBase64(b64, v); err != nil {
		r.Decode, r.Error = "error", err.Error()
		return r, nil
	}
	again, err := xdr.MarshalBase64(v)
	if err != nil {
		r.Decode, r.Error = "error", "re-encode: "+err.Error()
		return r, nil
	}
	r.Decode, r.RoundTripIdentical = "ok", again == b64
	return r, v
}

// apiChecks run the API checks of sightings.json (entries with "inputs" instead of a vector file).
// Each returns "defect", "ok" or "n/a" and a short detail.
var apiChecks = map[string]func(json.RawMessage) (string, string){
	"asset-pair-ordered-by-issuer-bytes": assetPairOrder,
	"scmap-keys-sorted-like-host": func(json.RawMessage) (string, string) {
		return "n/a", "no sorted-map constructor in go-stellar-sdk; xdr.ScMap is a slice kept in the order given"
	},
	"muxed-id-encodes-the-id-given": func(json.RawMessage) (string, string) {
		return "n/a", "muxed IDs are uint64 in xdr and strkey (xdr.MuxedAccountFromAccountId, strkey.MuxedAccount.SetID) and txnbuild takes M-addresses, so no rejected ID can be passed"
	},
	"offer-id-encodes-the-id-given": offerID,
}

// js-stellar-sdk#1766: assets with the same code order by issuer key bytes.
func assetPairOrder(raw json.RawMessage) (string, string) {
	var in struct {
		Code   string `json:"code"`
		First  string `json:"issuerFirstByBytes"`
		Second string `json:"issuerSecondByBytes"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		panic(err)
	}
	first := txnbuild.CreditAsset{Code: in.Code, Issuer: in.First}
	second := txnbuild.CreditAsset{Code: in.Code, Issuer: in.Second}
	var failures []string
	if _, err := txnbuild.NewLiquidityPoolId(first, second); err != nil {
		failures = append(failures, "NewLiquidityPoolId(first, second) fails: "+err.Error())
	}
	if _, err := txnbuild.NewLiquidityPoolId(second, first); err == nil {
		failures = append(failures, "NewLiquidityPoolId(second, first) accepted")
	}
	return verdict(failures, "NewLiquidityPoolId accepts (first, second) and fails for (second, first)")
}

// js-stellar-sdk#1785: ManageSellOffer encodes the offer ID it was given, or fails validation.
// OfferID is an int64, so only the IDs that are canonical int64 decimals can be passed.
func offerID(raw json.RawMessage) (string, string) {
	var in struct {
		Issuer   string   `json:"issuer"`
		Accepted []string `json:"accepted"`
		Rejected []string `json:"rejected"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		panic(err)
	}
	op := func(id int64) *txnbuild.ManageSellOffer {
		return &txnbuild.ManageSellOffer{Selling: txnbuild.NativeAsset{}, Buying: txnbuild.CreditAsset{Code: "USD", Issuer: in.Issuer},
			Amount: "1", Price: xdr.Price{N: 1, D: 1}, OfferID: id}
	}
	var failures, tested []string
	for _, s := range in.Accepted {
		id, ok := canonicalInt64(s)
		if !ok {
			continue
		}
		o := op(id)
		x, err := o.BuildXDR()
		if err == nil {
			err = o.Validate()
		}
		if err != nil {
			failures = append(failures, s+" fails: "+err.Error())
		} else if got := int64(x.Body.MustManageSellOfferOp().OfferId); got != id {
			failures = append(failures, fmt.Sprintf("%s encodes %d", s, got))
		}
	}
	for _, s := range in.Rejected {
		id, ok := canonicalInt64(s)
		if !ok {
			continue
		}
		tested = append(tested, s)
		if op(id).Validate() == nil {
			failures = append(failures, s+" passes Validate()")
		}
	}
	if len(tested) == 0 {
		return "n/a", "no rejected ID fits OfferID int64"
	}
	return verdict(failures, "OfferID is int64; Validate(), which NewTransaction runs, fails for "+strings.Join(tested, ", "))
}

func canonicalInt64(s string) (int64, bool) {
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil && strconv.FormatInt(v, 10) == s
}

func verdict(failures []string, okDetail string) (string, string) {
	if len(failures) > 0 {
		return "defect", strings.Join(failures, "; ")
	}
	return "ok", okDetail
}

func read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return strings.TrimSpace(string(b))
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run . <kit-dir> <out.json>")
		os.Exit(2)
	}
	kit, outPath := os.Args[1], os.Args[2]

	var manifest struct {
		Entries []struct {
			ID    string `json:"id"`
			Files []struct {
				Path    string `json:"path"`
				XDRType string `json:"xdrType"`
			} `json:"files"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(read(filepath.Join(kit, "corpus", "manifest.json"))), &manifest); err != nil {
		panic(err)
	}
	var corpus []result
	for _, e := range manifest.Entries {
		for _, f := range e.Files {
			r, _ := roundTrip(f.XDRType, read(filepath.Join(kit, f.Path)))
			r.Entry, r.Path = e.ID, f.Path
			corpus = append(corpus, r)
		}
	}

	var sightings struct {
		Sightings []struct {
			ID      string          `json:"id"`
			File    string          `json:"file"`
			XDRType string          `json:"xdrType"`
			Check   string          `json:"check"`
			Inputs  json.RawMessage `json:"inputs"`
		} `json:"sightings"`
	}
	if err := json.Unmarshal([]byte(read(filepath.Join(kit, "conformance", "sightings.json"))), &sightings); err != nil {
		panic(err)
	}
	var vectors []any
	for _, s := range sightings.Sightings {
		if s.File == "" {
			r := apiResult{Sighting: s.ID, Check: s.Check}
			r.Result, r.Detail = apiChecks[s.Check](s.Inputs)
			vectors = append(vectors, r)
			continue
		}
		r, _ := roundTrip(s.XDRType, read(filepath.Join(kit, "conformance", s.File)))
		r.Sighting, r.File, r.Check, r.Result = s.ID, s.File, s.Check, "n/a"
		if s.Check == "native-map-conversion-keeps-every-entry" {
			// go-stellar-sdk has no generic ScVal-to-native conversion: ScMap decodes as a slice
			// and keeps every entry, so this check does not apply.
			r.Detail = "no native map conversion in go-stellar-sdk; ScMap is a slice of entries"
		}
		vectors = append(vectors, r)
	}

	version := "unknown"
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == "github.com/stellar/go-stellar-sdk" {
				version = d.Version
			}
		}
	}
	out, _ := json.MarshalIndent(map[string]any{"sdk": "go-stellar-sdk", "version": version, "corpus": corpus, "vectors": vectors}, "", "  ")
	if err := os.WriteFile(outPath, append(out, '\n'), 0o644); err != nil {
		panic(err)
	}
	ok := 0
	for _, c := range corpus {
		if c.RoundTripIdentical {
			ok++
		}
	}
	fmt.Printf("go-stellar-sdk %s: %d/%d corpus files round-trip\n", version, ok, len(corpus))
}
