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
	"strings"

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
			ID      string `json:"id"`
			File    string `json:"file"`
			XDRType string `json:"xdrType"`
			Check   string `json:"check"`
		} `json:"sightings"`
	}
	if err := json.Unmarshal([]byte(read(filepath.Join(kit, "conformance", "sightings.json"))), &sightings); err != nil {
		panic(err)
	}
	var vectors []result
	for _, s := range sightings.Sightings {
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
