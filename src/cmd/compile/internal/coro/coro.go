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
// The current experiment exports the analysis result. Its provisional result
// observes the program before inlining, but does not constrain the inliner
// until a coroutine ABI and stable site plan consume that constraint. It does
// not yet change the native backend or generate coroutine state machines.
package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/ssa"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
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
const SummaryVersion uint64 = 2

// The compiler processes one package per process, so summaries are package
// compilation state and do not require synchronization.
var summaries = make(map[*ir.Func]FuncSummary)

var dumpMu sync.Mutex

// SetSummary records a function summary read from Unified IR export data.
func SetSummary(fn *ir.Func, summary FuncSummary) {
	if fn != nil {
		summaries[fn] = summary
	}
}

// Summary returns the recorded cross-package summary for fn.
func Summary(fn *ir.Func) (FuncSummary, bool) {
	summary, ok := summaries[fn]
	return summary, ok
}

// DumpPreLowerSSA reports the SSA shape presented at the target lowering
// boundary. The report-only PoC always continues into the native backend.
func DumpPreLowerSSA(w io.Writer, f *ssa.Func) {
	values := 0
	for _, block := range f.Blocks {
		values += len(block.Values)
	}

	// Backend compilation is parallel, so keep each diagnostic line intact.
	dumpMu.Lock()
	defer dumpMu.Unlock()
	fmt.Fprintf(w, "coro: phase=pre-lower-ssa func=%s blocks=%d values=%d action=continue-native\n",
		f.NameABI(), len(f.Blocks), values)
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
	SchedulerYield
	TimerWait
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
	case SchedulerYield:
		return "scheduler-yield"
	case TimerWait:
		return "timer-wait"
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
	Callee     *ir.Func
	CalleeName string
	Imported   FuncSummary
	Unknown    bool
	Recipe     OperationRecipe
	Node       *ir.CallExpr
}

// Function is the coroutine analysis result for one function.
type Function struct {
	Func      *ir.Func
	Local     Effect
	Effect    Effect
	LocalExec ExecFlags
	Exec      ExecFlags
	Primary   PrimaryKind
	Recursive bool
	Seeds     []Seed
	Edges     []Edge
	Sites     []Site
}

// Plan is the result of analyzing one package.
type Plan struct {
	Functions map[*ir.Func]*Function
}

// PublishSummaries makes this package's final effects available to the
// Unified IR export writer.
func (p *Plan) PublishSummaries() {
	for fn, function := range p.Functions {
		SetSummary(fn, FuncSummary{Effect: function.Effect, Exec: function.Exec})
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
	})

	// Some compiler-generated closures are package functions but are not
	// reachable from VisitFuncsBottomUp roots. Iterate over the complete
	// package plan so effects and execution flags also reach those functions.
	for changed := true; changed; {
		changed = false
		for _, function := range p.Functions {
			if function.Effect != MaySuspend &&
				(function.Local == MaySuspend || p.callsSuspending(function)) {
				function.Effect = MaySuspend
				changed = true
			}
			exec := function.LocalExec | p.calledExec(function)
			if exec != function.Exec {
				function.Exec = exec
				changed = true
			}
		}
	}

	for _, function := range p.Functions {
		function.Primary = (FuncSummary{
			Effect: function.Effect,
			Exec:   function.Exec,
		}).Primary()
		for _, edge := range function.Edges {
			if edge.Kind != GoCall && edge.Recipe.Kind == SiteInvalid &&
				p.edgeMaySuspend(edge) {
				function.Sites = append(function.Sites, Site{
					ID:   SiteID(len(function.Sites) + 1),
					Kind: SiteAwait,
					Node: edge.Node,
				})
			}
		}
	}

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
	addSite := func(kind SiteKind, node ir.Node, foreign ForeignCallClass) {
		function.Sites = append(function.Sites, Site{
			ID:      SiteID(len(function.Sites) + 1),
			Kind:    kind,
			Node:    node,
			Foreign: foreign,
		})
	}

	ir.Visit(function.Func, func(n ir.Node) {
		switch n.Op() {
		case ir.OSEND:
			addSeed(ChannelSend)
			addSite(SiteChannel, n, NotForeign)
		case ir.ORECV:
			addSeed(ChannelReceive)
			addSite(SiteChannel, n, NotForeign)
		case ir.OSELECT:
			// This is intentionally conservative for the PoC: a select with
			// a default case does not block, but treating it as a seed is safe.
			addSeed(ChannelSelect)
			addSite(SiteChannel, n, NotForeign)
		case ir.ORANGE:
			n := n.(*ir.RangeStmt)
			if n.X != nil && n.X.Type() != nil && n.X.Type().IsChan() {
				addSeed(ChannelRange)
				addSite(SiteChannel, n, NotForeign)
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

		edge := Edge{Kind: kind, Node: call}
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
		if recipe, ok := operationRecipe(edge.Callee); ok {
			edge.Recipe = recipe
			edge.Imported = FuncSummary{Effect: recipe.Effect, Exec: recipe.Exec}
			edge.Unknown = false
			function.LocalExec |= recipe.Exec
			if recipe.Effect == MaySuspend && kind != GoCall {
				switch recipe.Kind {
				case SiteYield:
					addSeed(SchedulerYield)
				case SiteTimer:
					addSeed(TimerWait)
				}
			}
			if kind != GoCall {
				addSite(recipe.Kind, call, recipe.Foreign)
			}
		}
		if kind == GoCall {
			addSite(SiteSpawn, call, edge.Recipe.Foreign)
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
	return edge.Imported.Effect == MaySuspend
}

func (p *Plan) calledExec(function *Function) ExecFlags {
	var flags ExecFlags
	for _, edge := range function.Edges {
		if edge.Kind == GoCall {
			continue
		}
		if callee := p.Functions[edge.Callee]; callee != nil {
			flags |= callee.Exec
		} else if !edge.Unknown {
			flags |= edge.Imported.Exec
		}
	}
	return flags
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
		fmt.Fprintf(w, "coro: func=%s effect=%s local=%s recursive=%t seeds=%s primary=%s exec=%s\n",
			ir.PkgFuncName(function.Func), function.Effect, function.Local,
			function.Recursive, strings.Join(seeds, ","), function.Primary, function.Exec)

		for _, site := range function.Sites {
			fmt.Fprintf(w, "coro: site=%d func=%s kind=%s foreign=%s\n",
				site.ID, ir.PkgFuncName(function.Func), site.Kind, site.Foreign)
		}

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
	return edge.Imported.Effect
}
