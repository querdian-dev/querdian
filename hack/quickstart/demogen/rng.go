package main

import (
	"hash/fnv"
	"math"
)

// rng is a small deterministic generator (splitmix64). The demo data must be reproducible for a
// given seed, so nothing here reads the clock or the operating system.
type rng struct{ state uint64 }

// newRNG derives an independent stream from the global seed and a label.
func newRNG(seed uint64, label string) *rng {
	h := fnv.New64a()
	_, _ = h.Write([]byte(label))
	return &rng{state: seed*0x9e3779b97f4a7c15 ^ h.Sum64()}
}

func (r *rng) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// float returns a number in [0, 1).
func (r *rng) float() float64 { return float64(r.next()>>11) / (1 << 53) }

// norm returns a standard normal sample (Box-Muller).
func (r *rng) norm() float64 {
	u := r.float()
	if u < 1e-12 {
		u = 1e-12
	}
	return math.Sqrt(-2*math.Log(u)) * math.Cos(2*math.Pi*r.float())
}

// exp returns an exponential sample with the given mean.
func (r *rng) exp(mean float64) float64 {
	u := r.float()
	if u < 1e-12 {
		u = 1e-12
	}
	return -mean * math.Log(u)
}

// intn returns an integer in [0, n).
func (r *rng) intn(n int) int { return int(r.next() % uint64(n)) }

// poisson returns a Poisson sample (Knuth for small means, normal approximation otherwise).
func (r *rng) poisson(mean float64) int {
	if mean <= 0 {
		return 0
	}
	if mean > 30 {
		v := int(math.Round(mean + math.Sqrt(mean)*r.norm()))
		if v < 0 {
			return 0
		}
		return v
	}
	limit := math.Exp(-mean)
	k, p := 0, 1.0
	for {
		p *= r.float()
		if p <= limit {
			return k
		}
		k++
	}
}

// hex returns n random bytes as lower-case hex; the first nibble is never zero so that IDs keep
// their full length when a backend normalises leading zeros.
func (r *rng) hex(n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, 2*n)
	for i := range b {
		b[i] = digits[r.intn(16)]
	}
	if b[0] == '0' {
		b[0] = digits[1+r.intn(15)]
	}
	return string(b)
}

// phi is the standard normal CDF.
func phi(x float64) float64 { return 0.5 * (1 + math.Erf(x/math.Sqrt2)) }
