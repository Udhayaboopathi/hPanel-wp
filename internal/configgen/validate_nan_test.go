package configgen

import (
	"math"
	"testing"
)

// ValidateResources must reject NaN and infinity (both previously
// passed every comparison, after which generation could emit
// "NaN" CPU values into a site compose file) and enforce the
// documented 0.1 CPU minimum.
func TestValidateResourcesRejectsNonFinite(t *testing.T) {
	for _, cpu := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 0, 0.05, -1, 4.01} {
		if err := ValidateResources(cpu, 512); err == nil {
			t.Errorf("cpu=%v: expected rejection", cpu)
		}
	}
	for _, cpu := range []float64{0.1, 0.5, 1, 4} {
		if err := ValidateResources(cpu, 512); err != nil {
			t.Errorf("cpu=%v: expected acceptance, got %v", cpu, err)
		}
	}
}
