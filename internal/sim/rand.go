package sim

// Rand is a small deterministic PRNG (SplitMix64). It is implemented here
// rather than borrowed from math/rand so the sequence is fixed forever,
// independent of Go release changes, and its state is trivially hashable.
type Rand struct {
	state uint64
}

// NewRand returns a generator seeded with seed.
func NewRand(seed uint64) *Rand { return &Rand{state: seed} }

// Uint64 returns the next pseudo-random value.
func (r *Rand) Uint64() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// Intn returns a uniform value in [0, n). It panics if n <= 0.
func (r *Rand) Intn(n int) int {
	if n <= 0 {
		panic("sim: Rand.Intn called with n <= 0")
	}
	un := uint64(n)
	// Rejection sampling removes modulo bias.
	limit := ^uint64(0) - (^uint64(0) % un)
	for {
		v := r.Uint64()
		if v < limit {
			return int(v % un)
		}
	}
}

// State returns the internal state, for hashing and replay checks.
func (r *Rand) State() uint64 { return r.state }
