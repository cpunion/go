// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"fmt"
)

// ExecFlags describe execution constraints independent of suspension.
type ExecFlags uint32

const (
	NeedsPreempt ExecFlags = 1 << iota
	MayBlockThread
	NeedsSystemABI
	ThreadAffine
)

func (f ExecFlags) String() string {
	if f == 0 {
		return "-"
	}
	names := make([]string, 0, 4)
	for _, flag := range []struct {
		flag ExecFlags
		name string
	}{
		{NeedsPreempt, "preempt"},
		{MayBlockThread, "block-thread"},
		{NeedsSystemABI, "system-abi"},
		{ThreadAffine, "thread-affine"},
	} {
		if f&flag.flag != 0 {
			names = append(names, flag.name)
			f &^= flag.flag
		}
	}
	if f != 0 {
		names = append(names, fmt.Sprintf("ExecFlags(%#x)", uint32(f)))
	}
	return joinNames(names)
}

// TerminalFlags describe non-local control that must cross coroutine frames.
// They are independent of suspension and physical execution constraints.
type TerminalFlags uint8

const (
	MayPanic TerminalFlags = 1 << iota
	UsesRecover
	MayGoexit
)

func (f TerminalFlags) String() string {
	if f == 0 {
		return "-"
	}
	names := make([]string, 0, 3)
	for _, flag := range []struct {
		flag TerminalFlags
		name string
	}{
		{MayPanic, "panic"},
		{UsesRecover, "recover"},
		{MayGoexit, "goexit"},
	} {
		if f&flag.flag != 0 {
			names = append(names, flag.name)
			f &^= flag.flag
		}
	}
	if f != 0 {
		names = append(names, fmt.Sprintf("TerminalFlags(%#x)", uint8(f)))
	}
	return joinNames(names)
}

// ForeignCallClass describes how a typed foreign call interacts with an
// executor.
type ForeignCallClass uint8

const (
	NotForeign ForeignCallClass = iota
	DirectNoBlock
	DirectMayBlock
	AsyncOperation
)

func (c ForeignCallClass) String() string {
	switch c {
	case NotForeign:
		return "-"
	case DirectNoBlock:
		return "direct-noblock"
	case DirectMayBlock:
		return "direct-mayblock"
	case AsyncOperation:
		return "async-operation"
	default:
		return fmt.Sprintf("ForeignCallClass(%d)", c)
	}
}

// PrimaryKind is the physical primary body selected for a source function.
type PrimaryKind uint8

const (
	PlainPrimary PrimaryKind = iota
	CoroPrimary
)

func (k PrimaryKind) String() string {
	switch k {
	case PlainPrimary:
		return "plain"
	case CoroPrimary:
		return "coro"
	default:
		return fmt.Sprintf("PrimaryKind(%d)", k)
	}
}

// SiteKind describes one compiler-owned coroutine transition.
type SiteKind uint8

const (
	SiteInvalid SiteKind = iota
	SiteYield
	SiteAwait
	SiteSpawn
	SiteChannel
	SiteTimer
	SiteFile
	SitePoll
	SiteForeign
	SitePanic
	SiteGoexit
)

func (k SiteKind) String() string {
	switch k {
	case SiteInvalid:
		return "invalid"
	case SiteYield:
		return "yield"
	case SiteAwait:
		return "await"
	case SiteSpawn:
		return "spawn"
	case SiteChannel:
		return "channel"
	case SiteTimer:
		return "timer"
	case SiteFile:
		return "file"
	case SitePoll:
		return "poll"
	case SiteForeign:
		return "foreign"
	case SitePanic:
		return "panic"
	case SiteGoexit:
		return "goexit"
	default:
		return fmt.Sprintf("SiteKind(%d)", k)
	}
}

// SiteID identifies a coroutine site within one function. Zero is invalid.
type SiteID uint32

// Site is a frozen transition plan. Node is retained only during compilation;
// it is not part of the cross-package summary.
type Site struct {
	ID      SiteID
	Kind    SiteKind
	Node    ir.Node
	Foreign ForeignCallClass
}

// FactoryABI identifies a compiler-generated coroutine entry ABI. A zero
// value means that callers must use the ordinary Go entry.
type FactoryABI uint8

const (
	NoFactory FactoryABI = iota
	// FactoryABI1 is available for non-closure, non-generic functions and
	// concrete methods. A method receiver is the first explicit factory
	// parameter. A variadic parameter uses its normalized slice type. Each
	// result is returned through a typed pointer.
	FactoryABI1
	// FactoryABI2 returns an explicit typed-frame pointer followed by a
	// static resume function. The frame is opaque outside the defining
	// package and is passed back to the matching frame-aware runtime entry.
	FactoryABI2
	// FactoryABI3 accepts the current resume context before the source
	// parameters. The factory uses it to reuse a matching typed frame while
	// preserving the opaque frame boundary between packages.
	FactoryABI3
)

// DeferABI identifies a compiler-verified entry contract for invoking a
// statically resolved named function during coroutine defer cleanup.
type DeferABI uint8

const (
	NoDeferABI DeferABI = iota
	// DeferABI1 uses the ordinary entry for a plain primary and FactoryABI1
	// for a coroutine primary. The coroutine entry and every
	// coroutine-primary child used during cleanup are proven not to suspend,
	// require a constrained executor, or leave a detached stackless task.
	DeferABI1
)

// FuncSummary is the portion of a function plan exported across packages.
type FuncSummary struct {
	Effect   Effect
	Exec     ExecFlags
	Terminal TerminalFlags
	Factory  FactoryABI
	Defer    DeferABI
}

func (s FuncSummary) Primary() PrimaryKind {
	if s.Effect == MaySuspend || s.Exec&(NeedsPreempt|NeedsSystemABI) != 0 ||
		s.Terminal&(MayPanic|MayGoexit) != 0 {
		return CoroPrimary
	}
	return PlainPrimary
}

// OperationRecipe describes an exact compiler-owned operation declaration.
// It is keyed by a fully qualified static function identity, not inferred from
// a call's spelling or from a code address.
type OperationRecipe struct {
	Kind     SiteKind
	Effect   Effect
	Exec     ExecFlags
	Terminal TerminalFlags
	Foreign  ForeignCallClass
	Direct   string
	Errno    bool
}

var operationRecipes = map[string]OperationRecipe{
	"runtime.Gosched": {
		Kind:   SiteYield,
		Effect: MaySuspend,
	},
	"time.Sleep": {
		Kind:   SiteTimer,
		Effect: MaySuspend,
	},
	"runtime.Goexit": {
		Kind:     SiteGoexit,
		Terminal: MayGoexit,
	},
	"runtime/coro.FileRead": {
		Kind:   SiteFile,
		Effect: MaySuspend,
	},
	"runtime/coro.SocketRead": {
		Kind:   SitePoll,
		Effect: MaySuspend,
	},
	"os.(*File).Read": {
		Kind:   SiteFile,
		Effect: MaySuspend,
	},
	"net.(*TCPConn).Read": {
		Kind:   SitePoll,
		Effect: MaySuspend,
	},
	"net.(*conn).Read": {
		Kind:   SitePoll,
		Effect: MaySuspend,
	},
	"runtime/coro.DirectAdd": {
		Kind:    SiteForeign,
		Exec:    NeedsSystemABI,
		Foreign: DirectNoBlock,
	},
	"runtime/coro.DirectBlock": {
		Kind:    SiteForeign,
		Exec:    NeedsSystemABI | MayBlockThread,
		Foreign: DirectMayBlock,
	},
	"runtime/coro.AsyncDouble": {
		Kind:    SiteForeign,
		Effect:  MaySuspend,
		Exec:    NeedsSystemABI,
		Foreign: AsyncOperation,
	},
}

func operationRecipe(fn *ir.Func) (OperationRecipe, bool) {
	if fn == nil || fn.Nname == nil {
		return OperationRecipe{}, false
	}
	recipe, ok := operationRecipes[symbolName(fn.Nname)]
	return recipe, ok
}

func joinNames(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	result := names[0]
	for _, name := range names[1:] {
		result += "," + name
	}
	return result
}
