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
