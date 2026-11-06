// Package fairshare splits a limited integer amount among claimants
// deterministically. It is shared by mechanics that draw from one source
// (mining a well, salvaging a wreck).
package fairshare

import (
	"cmp"
	"slices"
)

// Claim is one claimant's request.
type Claim struct {
	ID   uint64 // tie-breaker only
	Want int64
}

// Split divides avail max-min fairly: claims are visited from the smallest
// request to the largest (ties by ID) and each receives
// min(want, remaining / claimants left). The result is indexed like claims,
// never exceeds a claim's want, and sums to at most avail; nothing is lost
// to rounding except what no claimant wants. Integer remainders favour
// later (larger, or higher-ID) claimants by at most 1 each.
func Split(claims []Claim, avail int64) []int64 {
	order := make([]int, len(claims))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int {
		return cmp.Or(cmp.Compare(claims[a].Want, claims[b].Want), cmp.Compare(claims[a].ID, claims[b].ID))
	})
	grants := make([]int64, len(claims))
	remaining := max(avail, 0)
	for k, i := range order {
		g := min(max(claims[i].Want, 0), remaining/int64(len(order)-k))
		grants[i] = g
		remaining -= g
	}
	return grants
}
