package stats

import "math"

// NormalQuantile is the standard normal distribution's quantile function.
func NormalQuantile(p float64) float64 { return math.Sqrt2 * math.Erfinv(2*p-1) }

// TQuantile is Student's t distribution's quantile function with df degrees of freedom, found by bisection on the
// distribution function (exact to about 1e-12).
func TQuantile(p float64, df int) float64 {
	switch {
	case df < 1 || math.IsNaN(p) || p <= 0 || p >= 1:
		return math.NaN()
	case p < 0.5:
		return -TQuantile(1-p, df)
	case p == 0.5:
		return 0
	}
	lo, hi := 0.0, 1.0
	for tCDF(hi, df) < p {
		hi *= 2
	}
	for range 200 {
		mid := (lo + hi) / 2
		if tCDF(mid, df) < p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// tCDF is Student's t distribution function for t ≥ 0.
func tCDF(t float64, df int) float64 {
	nu := float64(df)
	return 1 - 0.5*incompleteBeta(nu/(nu+t*t), nu/2, 0.5)
}

// incompleteBeta is the regularized incomplete beta function I_x(a, b), by its continued fraction (Lentz's method).
func incompleteBeta(x, a, b float64) float64 {
	switch {
	case x <= 0:
		return 0
	case x >= 1:
		return 1
	}
	lga, _ := math.Lgamma(a)
	lgb, _ := math.Lgamma(b)
	lgab, _ := math.Lgamma(a + b)
	front := math.Exp(lgab - lga - lgb + a*math.Log(x) + b*math.Log(1-x))
	if x > (a+1)/(a+b+2) { // the fraction converges fast on this side only
		return 1 - front*betaFraction(1-x, b, a)/b
	}
	return front * betaFraction(x, a, b) / a
}

func betaFraction(x, a, b float64) float64 {
	const tiny, eps = 1e-300, 1e-15
	c, d := 1.0, 1-(a+b)*x/(a+1)
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= 1000; m++ {
		mf := float64(m)
		for _, num := range []float64{mf * (b - mf) * x / ((a + 2*mf - 1) * (a + 2*mf)), -(a + mf) * (a + b + mf) * x / ((a + 2*mf) * (a + 2*mf + 1))} {
			d = 1 + num*d
			if math.Abs(d) < tiny {
				d = tiny
			}
			c = 1 + num/c
			if math.Abs(c) < tiny {
				c = tiny
			}
			d = 1 / d
			h *= d * c
		}
		if math.Abs(d*c-1) < eps {
			break
		}
	}
	return h
}
