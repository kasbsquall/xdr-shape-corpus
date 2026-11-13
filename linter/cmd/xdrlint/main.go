// Command xdrlint runs the XDR ingestion rules as a go vet style checker.
package main

import (
	"golang.org/x/tools/go/analysis/singlechecker"

	"github.com/kasbsquall/xdr-shape-corpus/linter/depthcap"
)

func main() { singlechecker.Main(depthcap.Analyzer) }
