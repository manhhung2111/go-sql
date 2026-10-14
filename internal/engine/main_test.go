package engine

import (
	"os"
	"testing"
)

// The engine's tables in tests are a few rows on one page, which the page-count
// rule would always scan. Run the package with every range a lookup, and let
// the tests of the rule itself set the real factor.
func TestMain(m *testing.M) {
	shippedRangeCostFactor = rangeCostFactor
	rangeCostFactor = 0
	os.Exit(m.Run())
}

// shippedRangeCostFactor is the factor the engine ships with, before TestMain
// overrides it.
var shippedRangeCostFactor int

// withCostFactor sets rangeCostFactor for one test.
func withCostFactor(t *testing.T, factor int) {
	t.Helper()
	saved := rangeCostFactor
	rangeCostFactor = factor
	t.Cleanup(func() { rangeCostFactor = saved })
}
