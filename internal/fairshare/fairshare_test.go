package fairshare

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

func TestSplit(t *testing.T) {
	cases := []struct {
		claims []Claim
		avail  int64
		want   string
	}{
		{[]Claim{{1, 4}, {2, 4}, {3, 4}}, 5, "[1 2 2]"},
		{[]Claim{{1, 1}, {2, 10}}, 6, "[1 5]"},
		{[]Claim{{1, 3}, {2, 3}}, 100, "[3 3]"},
		{[]Claim{{1, 3}}, 0, "[0]"},
		{nil, 5, "[]"},
		{[]Claim{{1, -2}, {2, 3}}, 2, "[0 2]"},
	}
	for _, c := range cases {
		if got := fmt.Sprint(Split(c.claims, c.avail)); got != c.want {
			t.Errorf("Split(%v, %d) = %s, want %s", c.claims, c.avail, got, c.want)
		}
	}
}

func TestSplitInvariants(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		n := 1 + r.IntN(8)
		claims := make([]Claim, n)
		var wantSum int64
		for i := range claims {
			claims[i] = Claim{ID: uint64(r.IntN(100)), Want: int64(r.IntN(20))}
			wantSum += claims[i].Want
		}
		avail := int64(r.IntN(60))
		g := Split(claims, avail)
		var sum int64
		for i, x := range g {
			if x < 0 || x > claims[i].Want {
				t.Fatalf("grant %d outside [0, %d]", x, claims[i].Want)
			}
			sum += x
		}
		if sum != min(avail, wantSum) {
			t.Fatalf("granted %d of %d available (wanted %d)", sum, avail, wantSum)
		}
	}
}
