// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/internal/src"
	"cmp"
	"fmt"
	"io"
	"slices"
	"sync/atomic"
)

// FuncID identifies a source function within a coroutine plan.
//
// The initial schema combines the compiler link symbol, definition ABI, and
// wrapper role. A complete cross-package schema will also need explicit
// closure, instantiation, and version identities.
type FuncID string

func (id FuncID) String() string {
	return string(id)
}

// SiteID identifies a planned coroutine operation within a function. Ordinal
// is the operation's deterministic traversal order before SSA construction.
type SiteID struct {
	Func    FuncID
	Ordinal uint32
}

func (id SiteID) String() string {
	return fmt.Sprintf("%s:%d", id.Func, id.Ordinal)
}

// SiteKind describes the control operation performed at a site.
type SiteKind uint8

const (
	SiteAwait SiteKind = iota + 1
	SitePark
	SiteSpawn
	SiteDispatch
)

func (kind SiteKind) String() string {
	switch kind {
	case SiteAwait:
		return "await"
	case SitePark:
		return "park"
	case SiteSpawn:
		return "spawn"
	case SiteDispatch:
		return "dispatch"
	default:
		return fmt.Sprintf("SiteKind(%d)", kind)
	}
}

// OperationKind identifies the target-neutral operation recipe used by a park
// site. Await, spawn, and dispatch sites have OperationNone.
type OperationKind uint8

const (
	OperationNone OperationKind = iota
	OperationChannelSend
	OperationChannelReceive
	OperationChannelSelect
	OperationChannelRange
	OperationTimer
)

func (kind OperationKind) String() string {
	switch kind {
	case OperationNone:
		return "-"
	case OperationChannelSend:
		return "channel-send"
	case OperationChannelReceive:
		return "channel-receive"
	case OperationChannelSelect:
		return "channel-select"
	case OperationChannelRange:
		return "channel-range"
	case OperationTimer:
		return "timer"
	default:
		return fmt.Sprintf("OperationKind(%d)", kind)
	}
}

// SitePlan is the semantic input for one coroutine lowering site. Freeze
// constructs these plans before backend work starts. Target backends must not
// infer Kind or Operation from symbol names or raw SSA shape.
type SitePlan struct {
	ID        SiteID
	Kind      SiteKind
	Operation OperationKind
	Pos       src.XPos
	Callee    FuncID
}

// FuncPlan is the coroutine lowering plan for one source function. Freeze
// constructs it before backend work starts.
type FuncPlan struct {
	ID     FuncID
	Effect Effect
	Sites  []SitePlan
}

// Plan is the immutable package plan shared by SSA construction and target
// backends.
type Plan struct {
	funcs  []*FuncPlan
	byFunc map[*ir.Func]*FuncPlan
}

// Lookup returns the lowering plan for fn.
func (p *Plan) Lookup(fn *ir.Func) (*FuncPlan, bool) {
	if p == nil {
		return nil, false
	}
	fp, ok := p.byFunc[fn]
	return fp, ok
}

// Funcs returns the function plans in FuncID order.
func (p *Plan) Funcs() []*FuncPlan {
	if p == nil {
		return nil
	}
	return slices.Clone(p.funcs)
}

// Dump writes a deterministic representation of the lowering sites in p.
func (p *Plan) Dump(w io.Writer) {
	for _, fp := range p.funcs {
		for _, site := range fp.Sites {
			callee := site.Callee.String()
			if callee == "" {
				callee = "-"
			}
			fmt.Fprintf(w, "coro: site=%s kind=%s operation=%s callee=%s line=%s:%s\n",
				site.ID, site.Kind, site.Operation, callee,
				site.Pos.LineNumber(), site.Pos.ColumnNumber())
		}
	}
}

type siteCandidateKind uint8

const (
	operationCandidate siteCandidateKind = iota
	callCandidate
)

type siteCandidate struct {
	kind      siteCandidateKind
	ordinal   uint32
	pos       src.XPos
	reason    suspendReason
	callIndex int
}

// Freeze converts the mutable effect analysis into an immutable lowering
// plan.
func (a *Analysis) Freeze() (*Plan, error) {
	if a == nil {
		return nil, fmt.Errorf("nil coroutine analysis")
	}

	funcs := make([]*funcInfo, 0, len(a.funcs))
	for _, info := range a.funcs {
		funcs = append(funcs, info)
	}
	slices.SortFunc(funcs, func(a, b *funcInfo) int {
		return cmp.Compare(funcID(a.fn), funcID(b.fn))
	})

	plan := &Plan{
		funcs:  make([]*FuncPlan, 0, len(funcs)),
		byFunc: make(map[*ir.Func]*FuncPlan, len(funcs)),
	}
	ids := make(map[FuncID]struct{}, len(funcs))
	for _, info := range funcs {
		id := funcID(info.fn)
		if id == "" {
			return nil, fmt.Errorf("function has no stable identity")
		}
		fp := &FuncPlan{ID: id, Effect: info.effect}
		for _, candidate := range info.sites {
			site, include := a.freezeSite(info, candidate, id)
			if include {
				fp.Sites = append(fp.Sites, site)
			}
		}
		// Phase 1 emits only functions that may suspend. The Go compiler can
		// retain multiple plain declarations for one link symbol while
		// constructing wrappers. Reject duplicate identities once a function
		// participates in coroutine lowering; later phases must extend the
		// identity schema before emitting those wrapper families.
		if info.effect == MaySuspend || len(fp.Sites) != 0 {
			if _, exists := ids[id]; exists {
				return nil, fmt.Errorf("duplicate coroutine function identity %q", id)
			}
			ids[id] = struct{}{}
		}
		plan.funcs = append(plan.funcs, fp)
		plan.byFunc[info.fn] = fp
	}
	return plan, nil
}

func (a *Analysis) freezeSite(info *funcInfo, candidate siteCandidate, id FuncID) (SitePlan, bool) {
	site := SitePlan{
		ID:  SiteID{Func: id, Ordinal: candidate.ordinal},
		Pos: candidate.pos,
	}
	switch candidate.kind {
	case operationCandidate:
		site.Kind = SitePark
		site.Operation = operationForReason(candidate.reason)
	case callCandidate:
		if candidate.callIndex < 0 || candidate.callIndex >= len(info.calls) {
			panic("invalid coroutine call candidate")
		}
		call := info.calls[candidate.callIndex]
		if call.callee != nil {
			site.Callee = funcID(call.callee)
		} else if call.calleeName != "<dynamic>" && call.calleeName != "<interface>" {
			site.Callee = FuncID(call.calleeName)
		}
		switch {
		case call.kind == goCall:
			site.Kind = SiteSpawn
		case call.unknown:
			site.Kind = SiteDispatch
		case a.callMaySuspend(call):
			site.Kind = SiteAwait
		default:
			return SitePlan{}, false
		}
	default:
		panic("invalid coroutine site candidate")
	}
	return site, true
}

func operationForReason(reason suspendReason) OperationKind {
	switch reason {
	case channelSend:
		return OperationChannelSend
	case channelReceive:
		return OperationChannelReceive
	case channelSelect:
		return OperationChannelSelect
	case channelRange:
		return OperationChannelRange
	case timerWait:
		return OperationTimer
	default:
		panic("suspend reason has no coroutine operation recipe")
	}
}

func funcID(fn *ir.Func) FuncID {
	if fn == nil || fn.Nname == nil {
		return ""
	}
	id := fmt.Sprintf("%s,%d", fn.LinksymABI(fn.ABI).Name, fn.ABI)
	switch {
	case fn.ABIWrapper():
		id += "/abi-wrapper"
	case fn.Wrapper():
		id += "/wrapper"
	}
	return FuncID(id)
}

var currentPlan atomic.Pointer[Plan]

// SetCurrentPlan installs the immutable package plan used by concurrent
// backend workers.
func SetCurrentPlan(plan *Plan) {
	currentPlan.Store(plan)
}

// CurrentPlan returns the package plan installed before backend work started.
func CurrentPlan() *Plan {
	return currentPlan.Load()
}
