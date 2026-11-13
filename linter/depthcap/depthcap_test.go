package depthcap_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/kasbsquall/xdr-shape-corpus/linter/depthcap"
)

func TestSeededViolations(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), depthcap.Analyzer, "example")
}
