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
// until a coroutine ABI and stable site plan consume that constraint. A
// restricted vertical-slice example can emit a standalone LLVM coroutine
// module, but normal compilation still continues through the native backend.
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

// callKind describes how a call is executed.
type callKind uint8

const (
	directCall callKind = iota
	deferCall
	goCall
)

func (k callKind) String() string {
	switch k {
	case directCall:
		return "direct"
	case deferCall:
		return "defer"
	case goCall:
		return "go"
	default:
		return fmt.Sprintf("callKind(%d)", k)
	}
}

// suspendReason describes why a function may suspend locally.
type suspendReason uint8

const (
	channelSend suspendReason = iota
	channelReceive
	channelSelect
	channelRange
	timerWait
	unknownCall
)

func (r suspendReason) String() string {
	switch r {
	case channelSend:
		return "channel-send"
	case channelReceive:
		return "channel-receive"
	case channelSelect:
		return "channel-select"
	case channelRange:
		return "channel-range"
	case timerWait:
		return "timer-wait"
	case unknownCall:
		return "unknown-call"
	default:
		return fmt.Sprintf("suspendReason(%d)", r)
	}
}

// callEdge describes a call from one function to another. callee is nil for a
// dynamic call. unknown is true when the callee has no effect summary in this
// package.
type callEdge struct {
	kind       callKind
	callee     *ir.Func
	calleeName string
	imported   Effect
	unknown    bool
	operation  OperationKind
}

// funcInfo is the coroutine analysis result for one function.
type funcInfo struct {
	fn        *ir.Func
	local     Effect
	effect    Effect
	recursive bool
	reasons   []suspendReason
	calls     []callEdge
	sites     []siteCandidate
}

// Analysis contains coroutine effect information for one package.
type Analysis struct {
	funcs map[*ir.Func]*funcInfo
}

// PublishSummaries makes this package's final effects available to the
// Unified IR export writer.
func (a *Analysis) PublishSummaries() {
	for fn, info := range a.funcs {
		SetSummary(fn, info.effect)
	}
}

// Analyze computes coroutine effects for funcs.
func Analyze(funcs []*ir.Func) *Analysis {
	a := &Analysis{funcs: make(map[*ir.Func]*funcInfo, len(funcs))}
	for _, fn := range funcs {
		a.funcs[fn] = &funcInfo{fn: fn}
	}
	for _, info := range a.funcs {
		a.scan(info)
	}

	ir.VisitFuncsBottomUp(funcs, func(funcs []*ir.Func, recursive bool) {
		for _, fn := range funcs {
			if info := a.funcs[fn]; info != nil {
				info.recursive = recursive
			}
		}

		// A component can contain mutually recursive functions and closures.
		// Iterate to a fixed point so effects flow across every cycle.
		for changed := true; changed; {
			changed = false
			for _, fn := range funcs {
				info := a.funcs[fn]
				if info == nil || info.effect == MaySuspend {
					continue
				}
				if info.local == MaySuspend || a.callsSuspending(info) {
					info.effect = MaySuspend
					changed = true
				}
			}
		}
	})

	return a
}

func (a *Analysis) scan(info *funcInfo) {
	callKinds := make(map[*ir.CallExpr]callKind)
	ir.Visit(info.fn, func(n ir.Node) {
		if n, ok := n.(*ir.GoDeferStmt); ok {
			if call, ok := n.Call.(*ir.CallExpr); ok {
				if n.Op() == ir.OGO {
					callKinds[call] = goCall
				} else {
					callKinds[call] = deferCall
				}
			}
		}
	})

	recordReason := func(reason suspendReason) {
		info.local = MaySuspend
		if !slices.Contains(info.reasons, reason) {
			info.reasons = append(info.reasons, reason)
		}
	}
	addOperation := func(reason suspendReason, node ir.Node) {
		recordReason(reason)
		info.sites = append(info.sites, siteCandidate{
			kind:    operationCandidate,
			ordinal: uint32(len(info.sites)),
			pos:     node.Pos(),
			reason:  reason,
		})
	}

	ir.Visit(info.fn, func(n ir.Node) {
		switch n.Op() {
		case ir.OSEND:
			addOperation(channelSend, n)
		case ir.ORECV:
			addOperation(channelReceive, n)
		case ir.OSELECT:
			// This is intentionally conservative for the PoC: a select with
			// a default case does not block, but treating it as a seed is safe.
			addOperation(channelSelect, n)
		case ir.ORANGE:
			n := n.(*ir.RangeStmt)
			if n.X != nil && n.X.Type() != nil && n.X.Type().IsChan() {
				addOperation(channelRange, n)
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

		kind := directCall
		if k, ok := callKinds[call]; ok {
			kind = k
		}

		edge := callEdge{kind: kind}
		if call.Op() == ir.OCALLFUNC {
			if name := ir.StaticCalleeName(ir.StaticValue(call.Fun)); name != nil {
				edge.callee = name.Func
				edge.calleeName = symbolName(name)
				if _, local := a.funcs[edge.callee]; !local {
					var known bool
					edge.imported, known = Summary(edge.callee)
					edge.unknown = !known
				}
			} else {
				edge.calleeName = "<dynamic>"
				edge.unknown = true
			}
		} else {
			edge.calleeName = "<interface>"
			edge.unknown = true
		}
		recipe, hasOperation := lookupOperation(edge.calleeName)
		if hasOperation {
			edge.imported = recipe.effect
			edge.unknown = false
			edge.operation = recipe.kind
		}
		callIndex := len(info.calls)
		info.calls = append(info.calls, edge)
		if hasOperation && kind != goCall {
			addOperation(recipe.reason, call)
		} else {
			info.sites = append(info.sites, siteCandidate{
				kind:      callCandidate,
				ordinal:   uint32(len(info.sites)),
				pos:       call.Pos(),
				callIndex: callIndex,
			})
		}
		if edge.unknown && kind != goCall {
			recordReason(unknownCall)
		}
	})
}

func (a *Analysis) callsSuspending(info *funcInfo) bool {
	for _, call := range info.calls {
		if call.kind == goCall {
			continue
		}
		if a.callMaySuspend(call) {
			return true
		}
	}
	return false
}

func (a *Analysis) callMaySuspend(call callEdge) bool {
	if call.operation != OperationNone {
		return true
	}
	if call.unknown {
		return true
	}
	if callee := a.funcs[call.callee]; callee != nil {
		return callee.effect == MaySuspend
	}
	return call.imported == MaySuspend
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

// Dump writes a deterministic, human-readable representation of a.
func (a *Analysis) Dump(w io.Writer) {
	funcs := make([]*funcInfo, 0, len(a.funcs))
	for _, info := range a.funcs {
		funcs = append(funcs, info)
	}
	slices.SortFunc(funcs, func(a, b *funcInfo) int {
		return strings.Compare(ir.PkgFuncName(a.fn), ir.PkgFuncName(b.fn))
	})

	for _, info := range funcs {
		reasons := make([]string, len(info.reasons))
		for i, reason := range info.reasons {
			reasons[i] = reason.String()
		}
		slices.Sort(reasons)
		if len(reasons) == 0 {
			reasons = append(reasons, "-")
		}
		fmt.Fprintf(w, "coro: func=%s effect=%s local=%s recursive=%t reasons=%s\n",
			ir.PkgFuncName(info.fn), info.effect, info.local,
			info.recursive, strings.Join(reasons, ","))

		calls := slices.Clone(info.calls)
		slices.SortFunc(calls, func(a, b callEdge) int {
			if n := strings.Compare(a.kind.String(), b.kind.String()); n != 0 {
				return n
			}
			return strings.Compare(a.calleeName, b.calleeName)
		})
		for _, call := range calls {
			effect := "unknown"
			if !call.unknown {
				effect = a.callEffect(call).String()
			}
			fmt.Fprintf(w, "coro: edge=%s caller=%s callee=%s unknown=%t effect=%s\n",
				call.kind, ir.PkgFuncName(info.fn), call.calleeName, call.unknown, effect)
		}
	}
}

func (a *Analysis) callEffect(call callEdge) Effect {
	if call.operation != OperationNone {
		return MaySuspend
	}
	if callee := a.funcs[call.callee]; callee != nil {
		return callee.effect
	}
	return call.imported
}
