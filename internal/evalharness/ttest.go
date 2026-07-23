package evalharness

import "math"

// TTestResult is a two-sided paired t-test over per-query metric deltas.
type TTestResult struct {
	N         int     `json:"n"`
	MeanDelta float64 `json:"mean_delta"`
	CI95Low   float64 `json:"ci95_low"`
	CI95High  float64 `json:"ci95_high"`
	P         float64 `json:"p"`
}

// PairedTTest computes a two-sided paired t-test on deltas (b minus a,
// computed by the caller) — the significance test behind `bsearch eval
// compare`'s headline verdict.
//
// Degenerate inputs never divide by zero:
//   - n < 2: not enough pairs to estimate variance. Returns P 1.0 (nothing to
//     test) with CI collapsed to the mean (0 for n == 0).
//   - zero variance, zero mean: deltas are all exactly 0 (both runs scored
//     identically — common with discrete IR metrics like recall@10 on a
//     small golden set). Returns P 1.0, CI == {0, 0}.
//   - zero variance, non-zero mean: deltas are all exactly equal but
//     non-zero (e.g. every query gained the same recall point). The
//     difference is certain, not statistically inferred: returns P 0.0, CI
//     collapsed to the (single) delta value.
func PairedTTest(deltas []float64) TTestResult {
	n := len(deltas)
	if n == 0 {
		return TTestResult{N: 0, P: 1}
	}

	mean := 0.0
	for _, d := range deltas {
		mean += d
	}
	mean /= float64(n)

	if n < 2 {
		return TTestResult{N: n, MeanDelta: mean, CI95Low: mean, CI95High: mean, P: 1}
	}

	variance := 0.0
	for _, d := range deltas {
		diff := d - mean
		variance += diff * diff
	}
	variance /= float64(n - 1)
	sd := math.Sqrt(variance)

	if sd == 0 {
		if mean == 0 {
			return TTestResult{N: n, MeanDelta: mean, CI95Low: 0, CI95High: 0, P: 1}
		}
		return TTestResult{N: n, MeanDelta: mean, CI95Low: mean, CI95High: mean, P: 0}
	}

	se := sd / math.Sqrt(float64(n))
	tStat := mean / se
	df := float64(n - 1)

	p := 2 * studentTCDFUpper(math.Abs(tStat), df)
	p = min(max(p, 0), 1)

	q := tQuantile975(df)
	return TTestResult{
		N:         n,
		MeanDelta: mean,
		CI95Low:   mean - q*se,
		CI95High:  mean + q*se,
		P:         p,
	}
}

// studentTCDFUpper returns P(T > t) for t >= 0 with df degrees of freedom,
// via the regularized incomplete beta function: P(T > t) =
// 0.5 * I_{df/(df+t²)}(df/2, 1/2). Two-sided p = 2 * upper tail.
func studentTCDFUpper(t, df float64) float64 {
	x := df / (df + t*t)
	return 0.5 * regIncBeta(df/2, 0.5, x)
}

// regIncBeta is the regularized incomplete beta I_x(a,b), continued-fraction
// evaluation (Numerical Recipes 6.4). Accurate to ~1e-10 for the a,b used
// here.
func regIncBeta(a, b, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	lbeta, _ := math.Lgamma(a + b)
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	front := math.Exp(lbeta - la - lb + a*math.Log(x) + b*math.Log(1-x))
	if x < (a+1)/(a+b+2) {
		return front * betaCF(a, b, x) / a
	}
	return 1 - front*betaCF(b, a, 1-x)/b
}

func betaCF(a, b, x float64) float64 {
	const maxIter = 200
	const eps = 3e-14
	const fpmin = 1e-300
	qab, qap, qam := a+b, a+1, a-1
	c := 1.0
	d := 1 - qab*x/qap
	if math.Abs(d) < fpmin {
		d = fpmin
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIter; m++ {
		m2 := 2 * m
		aa := float64(m) * (b - float64(m)) * x / ((qam + float64(m2)) * (a + float64(m2)))
		d = 1 + aa*d
		if math.Abs(d) < fpmin {
			d = fpmin
		}
		c = 1 + aa/c
		if math.Abs(c) < fpmin {
			c = fpmin
		}
		d = 1 / d
		h *= d * c
		aa = -(a + float64(m)) * (qab + float64(m)) * x / ((a + float64(m2)) * (qap + float64(m2)))
		d = 1 + aa*d
		if math.Abs(d) < fpmin {
			d = fpmin
		}
		c = 1 + aa/c
		if math.Abs(c) < fpmin {
			c = fpmin
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < eps {
			break
		}
	}
	return h
}

// tQuantile975 finds t with P(T <= t) = 0.975 by bisection on the CDF —
// good to 1e-9, plenty for a 95% CI.
func tQuantile975(df float64) float64 {
	lo, hi := 0.0, 100.0
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		if 1-studentTCDFUpper(mid, df) < 0.975 {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}
