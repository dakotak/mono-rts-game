package ai

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
)

// Order is a command the bot wants to send.
type Order struct {
	// Key identifies what the order controls (e.g. "auto:123",
	// "clone:bank"). An order identical to the last one sent for its key is
	// not sent again until Refresh ticks pass or the last one failed.
	Key      string
	Payload  proto.Message
	Units    int // unit-commands charged by the server
	Priority int // higher first
	Desc     string
	// OneShot marks actions (clone, transfer, upgrade) rather than standing
	// orders (automation, destinations, tags): once the server reports the
	// result, the same order may be sent again.
	OneShot bool
	// Essential moves (refueling, retreat, clearing the bank) skip the
	// energy guard that drops units unable to afford a move.
	Essential bool
}

// Outgoing is an order ready to submit.
type Outgoing struct {
	ID      string
	Order   Order
	Payload proto.Message
}

// Priorities.
const (
	PrioLifecycle = 100
	PrioTactical  = 80
	PrioEconomy   = 50
	PrioRoles     = 40
	PrioExplore   = 30
)

// Refresh is how long an identical order is suppressed.
const Refresh = 600

// OneShotRefresh suppresses a repeated one-shot order whose result has not
// arrived yet.
const OneShotRefresh = 60

// SoftBudget caps unit-commands per tick when the server sets no budget,
// to stay a polite client.
const SoftBudget = 200

// CommandStats counts command activity.
type CommandStats struct {
	Submitted      int            `json:"submitted"`
	Deduplicated   int            `json:"deduplicated"`
	Deferred       int            `json:"deferred"`
	Rejected       int            `json:"rejected"`
	Succeeded      int            `json:"succeeded"`
	Failed         int            `json:"failed"`
	UnitsCharged   int            `json:"units_charged"`
	FailuresByCode map[string]int `json:"failures_by_code"`
	// ByKind counts submissions by order kind (the description's first
	// words, without IDs or positions).
	ByKind map[string]int `json:"by_kind"`
}

// Commander turns orders into submissions: priority order, the server's
// per-tick budget (never more), deduplication and result tracking.
type Commander struct {
	budget  int
	nonce   string
	seq     uint64
	queue   []Order
	queued  map[string]int // key -> index in queue
	last    map[string]sent
	pending map[string]Order
	Stats   CommandStats
}

type sent struct {
	fp   []byte
	tick uint64
}

func newCommander(budget int, nonce string) *Commander {
	if budget <= 0 {
		budget = SoftBudget
	}
	return &Commander{budget: budget, nonce: nonce, last: map[string]sent{}, pending: map[string]Order{},
		Stats: CommandStats{FailuresByCode: map[string]int{}, ByKind: map[string]int{}}}
}

var fpOpts = proto.MarshalOptions{Deterministic: true}

// Issue queues an order unless it repeats the last one for its key.
func (c *Commander) Issue(tick uint64, o Order) {
	fp, _ := fpOpts.Marshal(o.Payload)
	refresh := uint64(Refresh)
	if o.OneShot {
		refresh = OneShotRefresh
	}
	if s, ok := c.last[o.Key]; ok && bytes.Equal(s.fp, fp) && tick < s.tick+refresh {
		c.Stats.Deduplicated++
		return
	}
	// A later order for the same key this tick supersedes the earlier one
	// (e.g. "cancel automation" followed by the new automation).
	if i, ok := c.queued[o.Key]; ok {
		c.queue[i] = o
		return
	}
	if c.queued == nil {
		c.queued = map[string]int{}
	}
	c.queued[o.Key] = len(c.queue)
	c.queue = append(c.queue, o)
}

// Forget allows the order for key to be sent again.
func (c *Commander) Forget(key string) { delete(c.last, key) }

// ForgetOlder allows the order for key to be sent again if it was last
// sent at least age ticks ago (the observed state shows it did not stick).
func (c *Commander) ForgetOlder(key string, tick, age uint64) {
	if s, ok := c.last[key]; ok && tick >= s.tick+age {
		delete(c.last, key)
	}
}

// Flush returns this tick's submissions: highest priority first (stable),
// within the budget. Orders that do not fit are dropped and replanned.
func (c *Commander) Flush(tick uint64) []Outgoing {
	slices.SortStableFunc(c.queue, func(a, b Order) int { return cmp.Compare(b.Priority, a.Priority) })
	left := c.budget
	var out []Outgoing
	for _, o := range c.queue {
		if o.Units > left {
			c.Stats.Deferred++
			continue
		}
		left -= o.Units
		c.seq++
		id := fmt.Sprintf("%s-%d", c.nonce, c.seq)
		fp, _ := fpOpts.Marshal(o.Payload)
		c.last[o.Key] = sent{fp: fp, tick: tick}
		c.pending[id] = o
		c.Stats.Submitted++
		c.Stats.UnitsCharged += o.Units
		c.Stats.ByKind[kindOf(o.Desc)]++
		out = append(out, Outgoing{ID: id, Order: o, Payload: o.Payload})
	}
	c.queue = c.queue[:0]
	clear(c.queued)
	return out
}

// Rejected records a submission the server did not accept.
func (c *Commander) Rejected(id string) {
	if o, ok := c.pending[id]; ok {
		delete(c.pending, id)
		c.Forget(o.Key)
		c.Stats.Rejected++
	}
}

// Results processes execution results; it returns the failed orders.
func (c *Commander) Results(rs []*rtsv1.CommandResult) []Failure {
	var fails []Failure
	for _, r := range rs {
		o, ok := c.pending[r.GetCommandId()]
		if !ok {
			continue
		}
		delete(c.pending, r.GetCommandId())
		if r.GetStatus() == rtsv1.CommandStatus_COMMAND_STATUS_FAILED {
			c.Forget(o.Key)
			c.Stats.Failed++
			c.Stats.FailuresByCode[r.GetErrorCode()]++
			fails = append(fails, Failure{Order: o, Code: r.GetErrorCode(), Message: r.GetErrorMessage()})
			continue
		}
		c.Stats.Succeeded++
		if o.OneShot {
			c.Forget(o.Key)
		}
	}
	return fails
}

// Pending returns the number of commands awaiting results.
func (c *Commander) Pending() int { return len(c.pending) }

// Failure is a failed order.
type Failure struct {
	Order         Order
	Code, Message string
}

// kindOf strips identifiers and coordinates from an order description.
func kindOf(desc string) string {
	var words []string
	for _, w := range strings.Fields(desc) {
		if strings.ContainsAny(w, "0123456789(") {
			break
		}
		words = append(words, w)
	}
	return strings.Join(words, " ")
}
