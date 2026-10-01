package stats

import "math"

// Wilson is the 95% Wilson score interval of k successes in n trials, clamped to 0..1 (the judge pilot's). With no
// trials it is the whole range, 0 to 1. Unlike the normal approximation it stays inside 0..1 and is not empty at 0 or n.
func Wilson(k, n int) (low, high float64) {
	if n <= 0 {
		return 0, 1
	}
	const z = 1.959964
	nf, p := float64(n), float64(k)/float64(n)
	d := 1 + z*z/nf
	c := p + z*z/(2*nf)
	r := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf))
	low, high = math.Max(0, (c-r)/d), math.Min(1, (c+r)/d)
	// Exact at the ends: rounding leaves about 1e-18 where the formula gives 0 (k = 0) or 1 (k = n).
	if k == 0 {
		low = 0
	}
	if k == n {
		high = 1
	}
	return low, high
}

// BinomialTwoSided is the exact two-sided p-value of k successes in n at p = 0.5: the total probability of outcomes no
// likelier than k's (within the pilot's 1e-12), as the pilot's.
func BinomialTwoSided(k, n int) float64 {
	if n == 0 {
		return 1
	}
	prob := func(i int) float64 {
		a, _ := math.Lgamma(float64(n + 1))
		b, _ := math.Lgamma(float64(i + 1))
		c, _ := math.Lgamma(float64(n - i + 1))
		return math.Exp(a - b - c - float64(n)*math.Ln2)
	}
	pk, sum := prob(k), 0.0
	for i := 0; i <= n; i++ {
		if p := prob(i); p <= pk+1e-12 {
			sum += p
		}
	}
	return math.Min(1, sum)
}
