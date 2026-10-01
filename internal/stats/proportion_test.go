package stats

import "testing"

func TestWilson(t *testing.T) {
	for _, c := range []struct {
		k, n      int
		low, high float64
	}{
		{4, 6, 0.3000, 0.9032},  // the judge pilot's 4 of 6
		{7, 10, 0.3968, 0.8922}, // a textbook value
		{0, 17, 0, 0.1843},
		{17, 17, 0.8157, 1},
		{0, 0, 0, 1},
	} {
		low, high := Wilson(c.k, c.n)
		if !near(low, c.low, 5e-4) || !near(high, c.high, 5e-4) {
			t.Errorf("Wilson(%d, %d) = %.4f–%.4f, want %.4f–%.4f", c.k, c.n, low, high, c.low, c.high)
		}
	}
}
