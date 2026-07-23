package evalharness

import (
	"math"
	"testing"
)

// Reference values below were computed with scipy before this test was
// written (not derived from the implementation):
//
//	$ uv run --with scipy python3 -c "
//	import scipy.stats as st
//	import numpy as np
//	deltas = [0.1, 0.2, 0.05, 0.15, 0.1]
//	res = st.ttest_1samp(deltas, 0)
//	n = len(deltas); df = n - 1
//	mean = np.mean(deltas); sd = np.std(deltas, ddof=1)
//	se = sd / np.sqrt(n); q = st.t.ppf(0.975, df)
//	print(mean, res.statistic, res.pvalue, q, mean - q*se, mean + q*se)
//	print(st.t.ppf(0.975, 10), st.t.ppf(0.975, 100), st.t.ppf(0.975, 195))
//	"
//	mean=0.12 t=4.706787243316416 p=0.009261696759514423
//	q(df=4)=2.7764451051977934
//	CI = [0.049214261150886396, 0.1907857388491136]
//	t.ppf(0.975, 10)=2.228138851986274
//	t.ppf(0.975, 100)=1.983971518523552
//	t.ppf(0.975, 195)=1.9722040512684431

func TestPairedTTest_KnownValues(t *testing.T) {
	deltas := []float64{0.1, 0.2, 0.05, 0.15, 0.1}
	got := PairedTTest(deltas)

	if got.N != 5 {
		t.Fatalf("N = %d, want 5", got.N)
	}
	if !approxEqual(t, got.MeanDelta, 0.12) {
		t.Errorf("MeanDelta = %v, want 0.12", got.MeanDelta)
	}
	wantP := 0.009261696759514423
	if math.Abs(got.P-wantP) > 1e-6 {
		t.Errorf("P = %v, want %v (tol 1e-6)", got.P, wantP)
	}
	wantLow, wantHigh := 0.049214261150886396, 0.1907857388491136
	if math.Abs(got.CI95Low-wantLow) > 1e-6 {
		t.Errorf("CI95Low = %v, want %v (tol 1e-6)", got.CI95Low, wantLow)
	}
	if math.Abs(got.CI95High-wantHigh) > 1e-6 {
		t.Errorf("CI95High = %v, want %v (tol 1e-6)", got.CI95High, wantHigh)
	}
	// CI must bracket the mean.
	if got.CI95Low > got.MeanDelta || got.CI95High < got.MeanDelta {
		t.Errorf("CI [%v, %v] does not bracket mean %v", got.CI95Low, got.CI95High, got.MeanDelta)
	}
	// scipy says p < 0.05, so the CI must exclude 0.
	if got.CI95Low <= 0 {
		t.Errorf("CI95Low = %v, want > 0 (p=%v < 0.05 means CI should exclude 0)", got.CI95Low, got.P)
	}
}

func TestPairedTTest_SymmetricDeltasPNearOne(t *testing.T) {
	deltas := []float64{0.1, -0.1, 0.05, -0.05}
	got := PairedTTest(deltas)

	if !approxEqual(t, got.MeanDelta, 0.0) {
		t.Errorf("MeanDelta = %v, want 0", got.MeanDelta)
	}
	if math.Abs(got.P-1.0) > 1e-12 {
		t.Errorf("P = %v, want 1.0 (tol 1e-12)", got.P)
	}
}

func TestPairedTTest_ZeroVarianceZeroMean(t *testing.T) {
	got := PairedTTest([]float64{0, 0, 0})

	if got.P != 1.0 {
		t.Errorf("P = %v, want 1.0", got.P)
	}
	if !approxEqual(t, got.MeanDelta, 0.0) {
		t.Errorf("MeanDelta = %v, want 0", got.MeanDelta)
	}
}

func TestPairedTTest_ZeroVarianceNonzeroMean(t *testing.T) {
	got := PairedTTest([]float64{0.5, 0.5, 0.5})

	if got.P != 0.0 {
		t.Errorf("P = %v, want 0.0", got.P)
	}
	if got.CI95Low != 0.5 || got.CI95High != 0.5 {
		t.Errorf("CI = [%v, %v], want [0.5, 0.5]", got.CI95Low, got.CI95High)
	}
}

func TestPairedTTest_TooFew(t *testing.T) {
	got := PairedTTest([]float64{0.3})

	if got.N != 1 {
		t.Errorf("N = %d, want 1", got.N)
	}
	if got.P != 1.0 {
		t.Errorf("P = %v, want 1.0", got.P)
	}
}

func TestPairedTTest_EmptyInput(t *testing.T) {
	got := PairedTTest(nil)

	if got.N != 0 {
		t.Errorf("N = %d, want 0", got.N)
	}
	if got.P != 1.0 {
		t.Errorf("P = %v, want 1.0", got.P)
	}
}

func TestTQuantile975_MatchesTables(t *testing.T) {
	cases := []struct {
		df   float64
		want float64
		tol  float64
	}{
		{10, 2.228139, 1e-4},
		{100, 1.983972, 1e-4},
		{195, 1.9721, 1e-3},
	}
	for _, c := range cases {
		got := tQuantile975(c.df)
		if math.Abs(got-c.want) > c.tol {
			t.Errorf("tQuantile975(%v) = %v, want %v (tol %v)", c.df, got, c.want, c.tol)
		}
	}
}
