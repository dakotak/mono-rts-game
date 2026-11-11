package ai

import (
	"testing"

	rtsv1 "github.com/dakotaknutson/unit-strategy-game/gen/rts/v1"
	"github.com/dakotaknutson/unit-strategy-game/internal/bot/state"
)

const foe2 = uint32(11)

// Two opponents are tracked separately: the bot targets one of them, not
// the empty middle between them.
func TestIntelTracksOpponentsSeparately(t *testing.T) {
	es := homeBase()
	for i := range 3 {
		es = append(es, unit(uint64(900+i), foe, 40+int32(i), 5, nil))
		es = append(es, unit(uint64(950+i), foe2, 5+int32(i), 28, nil))
	}
	b := newBot(t, 10, observe(t, 100, es, nil))
	b.home = state.Pos{X: 5, Y: 5}
	b.updateIntel()
	if len(b.intel) != 2 {
		t.Fatalf("intel on %d opponents, want 2", len(b.intel))
	}
	g := b.enemyBaseGuess()
	near := func(p state.Pos) bool { return state.Cheb(g, p) <= 3 }
	if !near(state.Pos{X: 41, Y: 5}) && !near(state.Pos{X: 6, Y: 28}) {
		t.Fatalf("enemy base guess = %v, want one opponent's territory", g)
	}
	// The closer one (Cheb 23 vs 36) is the target.
	if !near(state.Pos{X: 6, Y: 28}) {
		t.Fatalf("target = %v, want the nearer opponent at (6,28)", g)
	}
}

// Enemy units next to our base (raiders, scouts) do not place their base
// there; the bot guesses further out along that direction.
func TestIntelIgnoresSightingsNearHome(t *testing.T) {
	es := append(homeBase(), unit(900, foe, 12, 5, nil))
	b := newBot(t, 10, observe(t, 100, es, nil))
	b.home = state.Pos{X: 5, Y: 5}
	b.updateIntel()
	in := b.intel[foe]
	if in == nil {
		t.Fatal("opponent seen near home not recorded")
	}
	if state.Cheb(in.base, b.home) <= homeRadius {
		t.Fatalf("base guess %v is next to our home", in.base)
	}
	// A later sighting away from our base replaces the guess.
	b.K.BeginTick()
	b.K.ApplyUpdate(&rtsv1.TickUpdate{Tick: 101, Changed: []*rtsv1.Entity{unit(900, foe, 40, 20, nil)}})
	b.updateIntel()
	if got := b.intel[foe].base; got != (state.Pos{X: 40, Y: 20}) {
		t.Fatalf("base after a real sighting = %v, want (40,20)", got)
	}
	// An army standing there and seeing nobody forgets it.
	b.K.BeginTick()
	b.K.ApplyUpdate(&rtsv1.TickUpdate{Tick: 102, Removed: []uint64{900}})
	b.clearOpponentAt(state.Pos{X: 40, Y: 20})
	if len(b.intel) != 0 {
		t.Fatal("empty territory not forgotten")
	}
}

// A mustering army is fueled at the bank before clones and upgrades, and
// never pooled into the bank; scouts are refueled rather than drained.
func TestMusterFuelsFightersFirst(t *testing.T) {
	es := homeBase()
	b := newBot(t, 10, observe(t, 400, es, nil))
	b.Step()
	bank := b.bank()
	if bank == nil {
		t.Fatal("no bank")
	}
	var fighter, scout *state.Entity
	for _, u := range b.K.OwnUnits() {
		if u.ID != bank.ID && fighter == nil {
			fighter = u
		} else if u.ID != bank.ID && scout == nil {
			scout = u
		}
	}
	b.Roles[fighter.ID], b.Roles[scout.ID] = RoleFighter, RoleScout
	b.byRoleDirty = true
	b.muster, b.musterFuel = true, 70
	fighter.Attrs["energy.stored"] = iv(20)
	if !b.wantsFuel(fighter) {
		t.Fatal("mustering fighter with 20 energy does not want fuel")
	}
	fighter.Attrs["energy.stored"] = iv(80)
	if b.wantsFuel(fighter) {
		t.Fatal("fighter already fueled for the march still wants fuel")
	}
	b.muster = false
	fighter.Attrs["energy.stored"] = iv(20)
	if b.wantsFuel(fighter) {
		t.Fatal("fighter wants march fuel without a muster")
	}
	scout.Attrs["energy.stored"] = iv(30)
	if !b.wantsFuel(scout) {
		t.Fatal("scout at 30% does not want fuel")
	}
}
