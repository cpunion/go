// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package coro implements the front-end effect analysis used by the coro
// experiment.
//
// The analysis computes which functions may suspend. Channel operations are
// suspension seeds. The effect propagates through ordinary calls and defer,
// but not from the target of a go statement back to its caller.
//
// The current experiment exports the analysis result and uses a provisional
// result to constrain inlining. It does not yet change the native backend or
// generate LLVM coroutine operations.
package coro

import (
	"cmd/compile/internal/ir"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Effect describes whether a function may suspend its caller.
type Effect uint8

const (
	// NoSuspend means no suspension path is known.
	NoSuspend Effect = iota
	// MaySuspend means at least one path may suspend.
	MaySuspend
)

func (e Effect) String() string {
	switch e {
	case NoSuspend:
		return "nosuspend"
	case MaySuspend:
		return "may-suspend"
	default:
		return fmt.Sprintf("Effect(%d)", e)
	}
}

// SummaryVersion is the version of the coroutine function summary stored in
// the compiler-private Unified IR extension.
const SummaryVersion uint64 = 1

// The compiler processes one package per process, so summaries are package
// compilation state and do not require synchronization.
var summaries = make(map[*ir.Func]Effect)

// SetSummary records a function effect read from Unified IR export data.
func SetSummary(fn *ir.Func, effect Effect) {
	if fn != nil {
		summaries[fn] = effect
	}
}

// Summary returns the recorded cross-package effect for fn.
func Summary(fn *ir.Func) (Effect, bool) {
	effect, ok := summaries[fn]
	return effect, ok
}

// EdgeKind describes how a call is executed.
type EdgeKind uint8

const (
	DirectCall EdgeKind = iota
	DeferCall
	GoCall
)

func (k EdgeKind) String() string {
	switch k {
	case DirectCall:
		return "direct"
	case DeferCall:
		return "defer"
	case GoCall:
		return "go"
	default:
		return fmt.Sprintf("EdgeKind(%d)", k)
	}
}

// Seed is a reason a function is locally considered suspending.
type Seed uint8

const (
	ChannelSend Seed = iota
	ChannelReceive
	ChannelSelect
	ChannelRange
	UnknownCall
)

func (s Seed) String() string {
	switch s {
	case ChannelSend:
		return "channel-send"
	case ChannelReceive:
		return "channel-receive"
	case ChannelSelect:
		return "channel-select"
	case ChannelRange:
		return "channel-range"
	case UnknownCall:
		return "unknown-call"
	default:
		return fmt.Sprintf("Seed(%d)", s)
	}
}

// Edge is a call from one function to another. Callee is nil for a dynamic
// call. Unknown is true when the callee has no effect summary in this package.
type Edge struct {
	Kind       EdgeKind
	Call       *ir.CallExpr
	Callee     *ir.Func
	CalleeName string
	Imported   Effect
	Unknown    bool
}

// Function is the coroutine analysis result for one function.
type Function struct {
	Func      *ir.Func
	Local     Effect
	Effect    Effect
	Recursive bool
	Seeds     []Seed
	Edges     []Edge
}

// Plan is the result of analyzing one package.
type Plan struct {
	Functions map[*ir.Func]*Function
}

// DisallowInlining temporarily marks every suspending function as not
// inlinable. It returns a function that restores the original pragma bits.
//
// This reuses the inliner's existing eligibility mechanism without making the
// coroutine experiment part of its core policy. The final analysis must run
// after restore.
func (p *Plan) DisallowInlining() func() {
	var changedFuncs []*ir.Func
	var changedCalls []*ir.CallExpr
	for _, function := range p.Functions {
		if function.Effect == MaySuspend && function.Func.Pragma&ir.Noinline == 0 {
			function.Func.Pragma |= ir.Noinline
			changedFuncs = append(changedFuncs, function.Func)
		}
		for _, edge := range function.Edges {
			if edge.Kind == DirectCall && edge.Call != nil && !edge.Call.NoInline && p.edgeMaySuspend(edge) {
				edge.Call.NoInline = true
				changedCalls = append(changedCalls, edge.Call)
			}
		}
	}
	return func() {
		for _, fn := range changedFuncs {
			fn.Pragma &^= ir.Noinline
		}
		for _, call := range changedCalls {
			call.NoInline = false
		}
	}
}

// PublishSummaries makes this package's final effects available to the
// Unified IR export writer.
func (p *Plan) PublishSummaries() {
	for fn, function := range p.Functions {
		SetSummary(fn, function.Effect)
	}
}

// Analyze computes coroutine effects for funcs.
func Analyze(funcs []*ir.Func) *Plan {
	p := &Plan{Functions: make(map[*ir.Func]*Function, len(funcs))}
	for _, fn := range funcs {
		p.Functions[fn] = &Function{Func: fn}
	}
	for _, function := range p.Functions {
		p.scan(function)
	}

	ir.VisitFuncsBottomUp(funcs, func(funcs []*ir.Func, recursive bool) {
		for _, fn := range funcs {
			if function := p.Functions[fn]; function != nil {
				function.Recursive = recursive
			}
		}

		// A component can contain mutually recursive functions and closures.
		// Iterate to a fixed point so effects flow across every cycle.
		for changed := true; changed; {
			changed = false
			for _, fn := range funcs {
				function := p.Functions[fn]
				if function == nil || function.Effect == MaySuspend {
					continue
				}
				if function.Local == MaySuspend || p.callsSuspending(function) {
					function.Effect = MaySuspend
					changed = true
				}
			}
		}
	})

	return p
}

func (p *Plan) scan(function *Function) {
	goDefer := make(map[*ir.CallExpr]EdgeKind)
	ir.Visit(function.Func, func(n ir.Node) {
		if n, ok := n.(*ir.GoDeferStmt); ok {
			if call, ok := n.Call.(*ir.CallExpr); ok {
				if n.Op() == ir.OGO {
					goDefer[call] = GoCall
				} else {
					goDefer[call] = DeferCall
				}
			}
		}
	})

	addSeed := func(seed Seed) {
		function.Local = MaySuspend
		if !slices.Contains(function.Seeds, seed) {
			function.Seeds = append(function.Seeds, seed)
		}
	}

	ir.Visit(function.Func, func(n ir.Node) {
		switch n.Op() {
		case ir.OSEND:
			addSeed(ChannelSend)
		case ir.ORECV:
			addSeed(ChannelReceive)
		case ir.OSELECT:
			// This is intentionally conservative for the PoC: a select with
			// a default case does not block, but treating it as a seed is safe.
			addSeed(ChannelSelect)
		case ir.ORANGE:
			n := n.(*ir.RangeStmt)
			if n.X != nil && n.X.Type() != nil && n.X.Type().IsChan() {
				addSeed(ChannelRange)
			}
		}

		call, ok := n.(*ir.CallExpr)
		if !ok {
			return
		}
		switch call.Op() {
		case ir.OCALLFUNC, ir.OCALLINTER:
		default:
			return
		}

		kind := DirectCall
		if goDeferKind, ok := goDefer[call]; ok {
			kind = goDeferKind
		}

		edge := Edge{Kind: kind, Call: call}
		if call.Op() == ir.OCALLFUNC {
			if name := ir.StaticCalleeName(ir.StaticValue(call.Fun)); name != nil {
				edge.Callee = name.Func
				edge.CalleeName = symbolName(name)
				if _, local := p.Functions[edge.Callee]; !local {
					var known bool
					edge.Imported, known = Summary(edge.Callee)
					edge.Unknown = !known
				}
			} else {
				edge.CalleeName = "<dynamic>"
				edge.Unknown = true
			}
		} else {
			edge.CalleeName = "<interface>"
			edge.Unknown = true
		}
		function.Edges = append(function.Edges, edge)
		if edge.Unknown && kind != GoCall {
			addSeed(UnknownCall)
		}
	})
}

func (p *Plan) callsSuspending(function *Function) bool {
	for _, edge := range function.Edges {
		if edge.Kind == GoCall {
			continue
		}
		if p.edgeMaySuspend(edge) {
			return true
		}
	}
	return false
}

func (p *Plan) edgeMaySuspend(edge Edge) bool {
	if edge.Unknown {
		return true
	}
	if callee := p.Functions[edge.Callee]; callee != nil {
		return callee.Effect == MaySuspend
	}
	return edge.Imported == MaySuspend
}

func symbolName(name *ir.Name) string {
	if name == nil || name.Sym() == nil {
		return "<dynamic>"
	}
	sym := name.Sym()
	if sym.Pkg == nil || sym.Pkg.Path == "" {
		return sym.Name
	}
	return sym.Pkg.Path + "." + sym.Name
}

// Dump writes a deterministic, human-readable representation of p.
func (p *Plan) Dump(w io.Writer) {
	functions := make([]*Function, 0, len(p.Functions))
	for _, function := range p.Functions {
		functions = append(functions, function)
	}
	slices.SortFunc(functions, func(a, b *Function) int {
		return strings.Compare(ir.PkgFuncName(a.Func), ir.PkgFuncName(b.Func))
	})

	for _, function := range functions {
		seeds := make([]string, len(function.Seeds))
		for i, seed := range function.Seeds {
			seeds[i] = seed.String()
		}
		slices.Sort(seeds)
		if len(seeds) == 0 {
			seeds = append(seeds, "-")
		}
		fmt.Fprintf(w, "coro: func=%s effect=%s local=%s recursive=%t seeds=%s\n",
			ir.PkgFuncName(function.Func), function.Effect, function.Local,
			function.Recursive, strings.Join(seeds, ","))

		edges := slices.Clone(function.Edges)
		slices.SortFunc(edges, func(a, b Edge) int {
			if n := strings.Compare(a.Kind.String(), b.Kind.String()); n != 0 {
				return n
			}
			return strings.Compare(a.CalleeName, b.CalleeName)
		})
		for _, edge := range edges {
			effect := "unknown"
			if !edge.Unknown {
				effect = p.edgeEffect(edge).String()
			}
			fmt.Fprintf(w, "coro: edge=%s caller=%s callee=%s unknown=%t effect=%s\n",
				edge.Kind, ir.PkgFuncName(function.Func), edge.CalleeName, edge.Unknown, effect)
		}
	}
}

func (p *Plan) edgeEffect(edge Edge) Effect {
	if callee := p.Functions[edge.Callee]; callee != nil {
		return callee.Effect
	}
	return edge.Imported
}
