// Package expr is a tiny, deterministic integer expression language for
// configurable game formulas (upgrade costs, durations, derived
// properties). It is deliberately not general purpose: no loops, no
// assignment, no strings, no user-defined functions, bounded size.
//
// # Syntax
//
//	expr     = sum
//	sum      = product { ("+" | "-") product }
//	product  = unary { ("*" | "/" | "%") unary }
//	unary    = "-" unary | primary
//	primary  = integer | identifier | call | "(" expr ")"
//	call     = identifier "(" expr { "," expr } ")"
//
// Identifiers are [a-z_][a-z0-9_]*. Functions: min(a, b, ...),
// max(a, b, ...), clamp(x, lo, hi), abs(x), pow(x, n), sqrt(x).
//
// # Semantics (total: evaluation never fails)
//
// All values are int64. "/" and "%" truncate toward zero like Go; division
// or modulo by zero yields 0. Every operation saturates at the int64 range
// instead of overflowing. pow(x, n) with n < 0 is 0; sqrt(x) is the floor
// square root, and 0 for x < 0. Because evaluation cannot fail and uses no
// floating point, results are identical on every platform.
package expr

import (
	"fmt"
	"math"
	"math/bits"
	"slices"
	"strconv"
	"strings"
)

// Limits on formula size, checked at parse time.
const (
	MaxLength = 512
	MaxNodes  = 128
	MaxDepth  = 32
)

type opKind uint8

const (
	opConst opKind = iota
	opVar
	opNeg
	opAdd
	opSub
	opMul
	opDiv
	opMod
	opMin
	opMax
	opClamp
	opAbs
	opPow
	opSqrt
)

var funcs = map[string]struct {
	op       opKind
	min, max int // arity bounds (max < 0: unbounded)
}{
	"min":   {opMin, 2, -1},
	"max":   {opMax, 2, -1},
	"clamp": {opClamp, 3, 3},
	"abs":   {opAbs, 1, 1},
	"pow":   {opPow, 2, 2},
	"sqrt":  {opSqrt, 1, 1},
}

type node struct {
	op   opKind
	val  int64  // opConst
	name string // opVar
	slot int    // opVar after Compile
	args []*node
}

// Expr is a parsed formula.
type Expr struct {
	src  string
	root *node
	vars []string
}

// Parse parses a formula.
func Parse(src string) (*Expr, error) {
	if len(src) > MaxLength {
		return nil, fmt.Errorf("formula longer than %d characters", MaxLength)
	}
	p := &parser{src: src}
	p.next()
	root, err := p.parseSum(0)
	if err != nil {
		return nil, fmt.Errorf("formula %q: %w", src, err)
	}
	if p.tok.kind != tEOF {
		return nil, fmt.Errorf("formula %q: unexpected %q at offset %d", src, p.tok.text, p.tok.pos)
	}
	if p.nodes > MaxNodes {
		return nil, fmt.Errorf("formula %q: more than %d terms", src, MaxNodes)
	}
	e := &Expr{src: src, root: root}
	seen := map[string]bool{}
	walk(root, func(n *node) {
		if n.op == opVar && !seen[n.name] {
			seen[n.name] = true
			e.vars = append(e.vars, n.name)
		}
	})
	slices.Sort(e.vars)
	return e, nil
}

// MustParse is Parse for formulas known to be valid (tests, constants).
func MustParse(src string) *Expr {
	e, err := Parse(src)
	if err != nil {
		panic(err)
	}
	return e
}

// String returns the source text.
func (e *Expr) String() string { return e.src }

// Vars returns the referenced variable names, sorted.
func (e *Expr) Vars() []string { return slices.Clone(e.vars) }

func walk(n *node, fn func(*node)) {
	fn(n)
	for _, a := range n.args {
		walk(a, fn)
	}
}

// Program is a formula compiled against a fixed variable layout.
type Program struct {
	root *node
	src  string
}

// Compile binds every variable to a slot index. slot returns false for
// names that are not allowed, which makes Compile fail; this is how callers
// restrict which variables a formula may use.
func (e *Expr) Compile(slot func(name string) (int, bool)) (*Program, error) {
	var bad []string
	root := clone(e.root)
	walk(root, func(n *node) {
		if n.op != opVar {
			return
		}
		i, ok := slot(n.name)
		if !ok {
			bad = append(bad, n.name)
			return
		}
		n.slot = i
	})
	if len(bad) > 0 {
		slices.Sort(bad)
		bad = slices.Compact(bad)
		return nil, fmt.Errorf("formula %q: unknown variable(s) %s", e.src, strings.Join(bad, ", "))
	}
	return &Program{root: root, src: e.src}, nil
}

// CompileVars compiles with the given variable names mapped to slots
// 0..len(names)-1.
func (e *Expr) CompileVars(names ...string) (*Program, error) {
	return e.Compile(func(name string) (int, bool) {
		i := slices.Index(names, name)
		return i, i >= 0
	})
}

func clone(n *node) *node {
	c := *n
	c.args = make([]*node, len(n.args))
	for i, a := range n.args {
		c.args[i] = clone(a)
	}
	return &c
}

// String returns the source text.
func (p *Program) String() string { return p.src }

// Eval evaluates the program with env[slot] as variable values.
func (p *Program) Eval(env []int64) int64 { return eval(p.root, env) }

func eval(n *node, env []int64) int64 {
	switch n.op {
	case opConst:
		return n.val
	case opVar:
		return env[n.slot]
	case opNeg:
		return sub(0, eval(n.args[0], env))
	case opAdd:
		return add(eval(n.args[0], env), eval(n.args[1], env))
	case opSub:
		return sub(eval(n.args[0], env), eval(n.args[1], env))
	case opMul:
		return mul(eval(n.args[0], env), eval(n.args[1], env))
	case opDiv:
		a, b := eval(n.args[0], env), eval(n.args[1], env)
		if b == 0 {
			return 0
		}
		if a == math.MinInt64 && b == -1 {
			return math.MaxInt64
		}
		return a / b
	case opMod:
		a, b := eval(n.args[0], env), eval(n.args[1], env)
		if b == 0 || b == -1 {
			return 0
		}
		return a % b
	case opMin, opMax:
		r := eval(n.args[0], env)
		for _, a := range n.args[1:] {
			v := eval(a, env)
			if (n.op == opMin && v < r) || (n.op == opMax && v > r) {
				r = v
			}
		}
		return r
	case opClamp:
		x, lo, hi := eval(n.args[0], env), eval(n.args[1], env), eval(n.args[2], env)
		return max(lo, min(x, hi))
	case opAbs:
		x := eval(n.args[0], env)
		if x < 0 {
			return sub(0, x)
		}
		return x
	case opPow:
		x, k := eval(n.args[0], env), eval(n.args[1], env)
		switch {
		case k < 0:
			return 0
		case k == 0:
			return 1
		case x == 0 || x == 1:
			return x
		case x == -1:
			if k%2 == 0 {
				return 1
			}
			return -1
		}
		// |x| >= 2 saturates within 64 multiplications, so the loop is
		// bounded regardless of k.
		if k > 64 {
			if x < 0 && k%2 == 1 {
				return math.MinInt64
			}
			return math.MaxInt64
		}
		r := int64(1)
		for range k {
			r = mul(r, x)
		}
		return r
	case opSqrt:
		x := eval(n.args[0], env)
		if x <= 0 {
			return 0
		}
		r := int64(math.Sqrt(float64(x)))
		// Correct the float estimate exactly with integer arithmetic.
		for r*r > x {
			r--
		}
		for (r+1)*(r+1) <= x && r+1 <= 3037000499 {
			r++
		}
		return r
	}
	panic("expr: unknown op")
}

func add(a, b int64) int64 {
	s := a + b
	if (a > 0 && b > 0 && s < 0) || (a < 0 && b < 0 && s >= 0) {
		if a > 0 {
			return math.MaxInt64
		}
		return math.MinInt64
	}
	return s
}

func sub(a, b int64) int64 {
	if b == math.MinInt64 {
		if a >= 0 {
			return math.MaxInt64
		}
		return a - b
	}
	return add(a, -b)
}

func mul(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	neg := (a < 0) != (b < 0)
	ua, ub := absU(a), absU(b)
	hi, lo := bits.Mul64(ua, ub)
	limit := uint64(math.MaxInt64)
	if neg {
		limit++ // |MinInt64|
	}
	if hi != 0 || lo > limit {
		if neg {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	if neg {
		return -int64(lo-1) - 1
	}
	return int64(lo)
}

func absU(a int64) uint64 {
	if a < 0 {
		return uint64(-(a + 1)) + 1
	}
	return uint64(a)
}

// ---- parser -------------------------------------------------------------

type tokKind uint8

const (
	tEOF tokKind = iota
	tNum
	tIdent
	tOp
)

type token struct {
	kind tokKind
	text string
	pos  int
}

type parser struct {
	src   string
	pos   int
	tok   token
	nodes int
	err   error
}

func (p *parser) next() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t' || p.src[p.pos] == '\n') {
		p.pos++
	}
	start := p.pos
	if p.pos >= len(p.src) {
		p.tok = token{kind: tEOF, pos: start}
		return
	}
	c := p.src[p.pos]
	switch {
	case c >= '0' && c <= '9':
		for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			p.pos++
		}
		p.tok = token{kind: tNum, text: p.src[start:p.pos], pos: start}
	case c == '_' || (c >= 'a' && c <= 'z'):
		for p.pos < len(p.src) {
			c := p.src[p.pos]
			if c != '_' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
				break
			}
			p.pos++
		}
		p.tok = token{kind: tIdent, text: p.src[start:p.pos], pos: start}
	case strings.IndexByte("+-*/%(),", c) >= 0:
		p.pos++
		p.tok = token{kind: tOp, text: string(c), pos: start}
	default:
		p.tok = token{kind: tOp, text: string(c), pos: start}
		p.pos++
		if p.err == nil {
			p.err = fmt.Errorf("invalid character %q at offset %d", c, start)
		}
	}
}

func (p *parser) mk(op opKind, args ...*node) *node {
	p.nodes++
	return &node{op: op, args: args}
}

func (p *parser) expect(text string) error {
	if p.err != nil {
		return p.err
	}
	if p.tok.kind != tOp || p.tok.text != text {
		return fmt.Errorf("expected %q at offset %d", text, p.tok.pos)
	}
	p.next()
	return nil
}

func (p *parser) parseSum(depth int) (*node, error) {
	if depth > MaxDepth {
		return nil, fmt.Errorf("nested deeper than %d", MaxDepth)
	}
	left, err := p.parseProduct(depth)
	if err != nil {
		return nil, err
	}
	for p.err == nil && p.tok.kind == tOp && (p.tok.text == "+" || p.tok.text == "-") {
		op := opAdd
		if p.tok.text == "-" {
			op = opSub
		}
		p.next()
		right, err := p.parseProduct(depth)
		if err != nil {
			return nil, err
		}
		left = p.mk(op, left, right)
	}
	return left, p.err
}

func (p *parser) parseProduct(depth int) (*node, error) {
	left, err := p.parseUnary(depth)
	if err != nil {
		return nil, err
	}
	for p.err == nil && p.tok.kind == tOp && strings.Contains("*/%", p.tok.text) {
		op := map[string]opKind{"*": opMul, "/": opDiv, "%": opMod}[p.tok.text]
		p.next()
		right, err := p.parseUnary(depth)
		if err != nil {
			return nil, err
		}
		left = p.mk(op, left, right)
	}
	return left, p.err
}

func (p *parser) parseUnary(depth int) (*node, error) {
	if p.err == nil && p.tok.kind == tOp && p.tok.text == "-" {
		p.next()
		if depth > MaxDepth {
			return nil, fmt.Errorf("nested deeper than %d", MaxDepth)
		}
		inner, err := p.parseUnary(depth + 1)
		if err != nil {
			return nil, err
		}
		return p.mk(opNeg, inner), nil
	}
	return p.parsePrimary(depth)
}

func (p *parser) parsePrimary(depth int) (*node, error) {
	if p.err != nil {
		return nil, p.err
	}
	t := p.tok
	switch t.kind {
	case tNum:
		v, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("number %s out of range", t.text)
		}
		p.next()
		n := p.mk(opConst)
		n.val = v
		return n, nil
	case tIdent:
		p.next()
		if p.err == nil && p.tok.kind == tOp && p.tok.text == "(" {
			f, ok := funcs[t.text]
			if !ok {
				return nil, fmt.Errorf("unknown function %q", t.text)
			}
			p.next()
			var args []*node
			for {
				a, err := p.parseSum(depth + 1)
				if err != nil {
					return nil, err
				}
				args = append(args, a)
				if p.tok.kind == tOp && p.tok.text == "," {
					p.next()
					continue
				}
				break
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
			if len(args) < f.min || (f.max >= 0 && len(args) > f.max) {
				return nil, fmt.Errorf("%s() takes %s arguments, got %d", t.text, arity(f.min, f.max), len(args))
			}
			return p.mk(f.op, args...), nil
		}
		if _, isFunc := funcs[t.text]; isFunc {
			return nil, fmt.Errorf("%q is a function; call it with parentheses", t.text)
		}
		n := p.mk(opVar)
		n.name = t.text
		return n, nil
	case tOp:
		if t.text == "(" {
			p.next()
			inner, err := p.parseSum(depth + 1)
			if err != nil {
				return nil, err
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
			return inner, nil
		}
		return nil, fmt.Errorf("unexpected %q at offset %d", t.text, t.pos)
	}
	return nil, fmt.Errorf("unexpected end of formula")
}

func arity(lo, hi int) string {
	switch {
	case hi < 0:
		return fmt.Sprintf("at least %d", lo)
	case lo == hi:
		return strconv.Itoa(lo)
	}
	return fmt.Sprintf("%d..%d", lo, hi)
}
