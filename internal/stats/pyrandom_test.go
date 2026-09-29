package stats

import "math/bits"

// pyRandom is Python's random.Random (MT19937, seeded with a small int), as far as random.choice uses it: tests use it
// to reproduce the Phase 0 spike's bootstrap draw for draw.
type pyRandom struct {
	mt  [624]uint32
	idx int
}

func newPyRandom(seed uint32) *pyRandom {
	r := &pyRandom{idx: 624}
	r.mt[0] = 19650218
	for i := 1; i < 624; i++ {
		r.mt[i] = 1812433253*(r.mt[i-1]^(r.mt[i-1]>>30)) + uint32(i)
	}
	// init_by_array with the key [seed], as Python seeds from an int.
	i, j := 1, 0
	for k := 624; k > 0; k-- {
		r.mt[i] = (r.mt[i] ^ ((r.mt[i-1] ^ (r.mt[i-1] >> 30)) * 1664525)) + seed + uint32(j)
		i++
		j = 0 // one key word
		if i >= 624 {
			r.mt[0], i = r.mt[623], 1
		}
	}
	for k := 623; k > 0; k-- {
		r.mt[i] = (r.mt[i] ^ ((r.mt[i-1] ^ (r.mt[i-1] >> 30)) * 1566083941)) - uint32(i)
		i++
		if i >= 624 {
			r.mt[0], i = r.mt[623], 1
		}
	}
	r.mt[0] = 0x80000000
	return r
}

func (r *pyRandom) uint32() uint32 {
	if r.idx >= 624 {
		for i := range 624 {
			y := r.mt[i]&0x80000000 | r.mt[(i+1)%624]&0x7fffffff
			r.mt[i] = r.mt[(i+397)%624] ^ y>>1
			if y&1 != 0 {
				r.mt[i] ^= 0x9908b0df
			}
		}
		r.idx = 0
	}
	y := r.mt[r.idx]
	r.idx++
	y ^= y >> 11
	y ^= y << 7 & 0x9d2c5680
	y ^= y << 15 & 0xefc60000
	y ^= y >> 18
	return y
}

// IntN is random.choice's _randbelow: the fewest bits that hold n, redrawn until below n.
func (r *pyRandom) IntN(n int) int {
	k := bits.Len(uint(n))
	for {
		if v := int(r.uint32() >> (32 - k)); v < n {
			return v
		}
	}
}
