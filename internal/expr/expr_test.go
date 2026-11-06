package expr

import (
	"math"
	"strings"
	"testing"
)

func eval1(t *testing.T, src string, vars map[string]int64) int64 {
	t.Helper()
	e, err := Parse(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	var names []string
	var env []int64
	for _, v := range e.Vars() {
		names = append(names, v)
		env = append(env, vars[v])
	}
	p, err := e.CompileVars(names...)
	if err != nil {
		t.Fatal(err)
	}
	return p.Eval(env)
}

func TestEvaluation(t *testing.T) {
	cases := []struct {
		src  string
		vars map[string]int64
		want int64
	}{
		{"1 + 2 * 3", nil, 7},
		{"(1 + 2) * 3", nil, 9},
		{"10 - 4 - 3", nil, 3}, // left associative
		{"100 / 7 / 2", nil, 7},
		{"-7 / 2", nil, -3}, // truncates toward zero
		{"-7 % 3", nil, -1},
		{"--5", nil, 5},
		{"2 * -3", nil, -6},
		{"v * v * 4 + v * 10", map[string]int64{"v": 3}, 66},
		{"min(5, 2, 9)", nil, 2},
		{"max(5, 2, 9)", nil, 9},
		{"clamp(mass / propulsion, 1, 20)", map[string]int64{"mass": 45, "propulsion": 2}, 20},
		{"clamp(mass / propulsion, 1, 20)", map[string]int64{"mass": 45, "propulsion": 9}, 5},
		{"abs(-12)", nil, 12},
		{"pow(3, 4)", nil, 81},
		{"pow(-2, 3)", nil, -8},
		{"pow(5, 0)", nil, 1},
		{"pow(5, -1)", nil, 0},
		{"sqrt(99)", nil, 9},
		{"sqrt(100)", nil, 10},
		{"sqrt(-4)", nil, 0},
		{"sqrt(9223372036854775807)", nil, 3037000499},
		{"  a_1 +b2  ", map[string]int64{"a_1": 1, "b2": 2}, 3},
	}
	for _, c := range cases {
		if got := eval1(t, c.src, c.vars); got != c.want {
			t.Errorf("%s = %d, want %d", c.src, got, c.want)
		}
	}
}

func TestTotalSemantics(t *testing.T) {
	cases := []struct {
		src  string
		want int64
	}{
		{"5 / 0", 0},
		{"5 % 0", 0},
		{"x / 0", 0},
		{"9223372036854775807 + 1", math.MaxInt64},
		{"-9223372036854775807 - 10", math.MinInt64},
		{"9223372036854775807 * 2", math.MaxInt64},
		{"9223372036854775807 * -2", math.MinInt64},
		{"pow(2, 63)", math.MaxInt64},
		{"pow(2, 1000000000000)", math.MaxInt64},
		{"pow(-2, 1000000000001)", math.MinInt64},
		{"pow(-1, 1000000000001)", -1},
		{"abs(-9223372036854775807 - 1)", math.MaxInt64},
		{"(-9223372036854775807 - 1) / -1", math.MaxInt64},
	}
	for _, c := range cases {
		if got := eval1(t, c.src, map[string]int64{"x": 3}); got != c.want {
			t.Errorf("%s = %d, want %d", c.src, got, c.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, src := range []string{
		"", "1 +", "(1", "1)", "1 2", "foo(1)", "min(1)", "clamp(1,2)", "abs(1,2)",
		"min", "a.b", "X", "1.5", "a = 1", "99999999999999999999", "#",
		strings.Repeat("(", 40) + "1" + strings.Repeat(")", 40),
		strings.Repeat("1+", 200) + "1",
		strings.Repeat("x", MaxLength+1),
	} {
		if _, err := Parse(src); err == nil {
			t.Errorf("Parse(%q) accepted", src)
		}
	}
}

func TestCompileRestrictsVariables(t *testing.T) {
	e := MustParse("v * cost + mass")
	if got := strings.Join(e.Vars(), ","); got != "cost,mass,v" {
		t.Fatalf("vars = %s", got)
	}
	if _, err := e.CompileVars("v", "mass"); err == nil || !strings.Contains(err.Error(), "cost") {
		t.Fatalf("unknown variable accepted: %v", err)
	}
	p, err := e.CompileVars("mass", "cost", "v")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Eval([]int64{1, 10, 2}); got != 21 {
		t.Fatalf("eval = %d", got)
	}
}
