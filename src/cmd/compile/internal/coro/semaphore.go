// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"go/constant"
)

// semaphoreSignature checks the exact acquire or release declaration.
// A changed declaration must not silently acquire a different runtime ABI.
func semaphoreSignature(name string, typ *types.Type) bool {
	params := 3
	switch name {
	case "sync.runtime_Semacquire", "internal/poll.runtime_Semacquire",
		"internal/poll.runtime_Semrelease":
		params = 1
	case "sync.runtime_SemacquireWaitGroup":
		params = 2
	}
	if typ == nil || typ.Kind() != types.TFUNC || typ.Recv() != nil ||
		typ.IsVariadic() || typ.NumParams() != params || typ.NumResults() != 0 ||
		!types.Identical(typ.Param(0).Type, types.NewPtr(types.Types[types.TUINT32])) {
		return false
	}
	return params == 1 ||
		types.Identical(typ.Param(1).Type, types.Types[types.TBOOL]) &&
			(params == 2 || types.Identical(typ.Param(2).Type, types.Types[types.TINT]))
}

// semaphoreNoHandoff recognizes releases that cannot yield to an ordinary
// waiter. Keep the declaration's conservative effect for every other call.
func semaphoreNoHandoff(call *ir.CallExpr) bool {
	if semaphoreOperation(call) != "coroSemrelease" || call.Args[1] == nil {
		return false
	}
	handoff := ir.StaticValue(call.Args[1])
	return ir.IsConst(handoff, constant.Bool) && !ir.BoolVal(handoff)
}

func semaphoreOperation(call *ir.CallExpr) string {
	if call == nil {
		return ""
	}
	name := symbolName(ir.StaticCalleeName(ir.StaticValue(call.Fun)))
	if !semaphoreSignature(name, call.Fun.Type()) || len(call.Args) != call.Fun.Type().NumParams() {
		return ""
	}
	switch name {
	// Mutex and RWMutex acquisitions use the same profiling flags as well
	// as the same argument ABI; no new runtime entry is needed.
	case "internal/sync.runtime_SemacquireMutex",
		"sync.runtime_SemacquireRWMutexR", "sync.runtime_SemacquireRWMutex":
		return "coroSemacquireMutex"
	case "internal/sync.runtime_Semrelease", "sync.runtime_Semrelease":
		return "coroSemrelease"
	case "sync.runtime_Semacquire", "internal/poll.runtime_Semacquire":
		return "coroSemacquire"
	case "sync.runtime_SemacquireWaitGroup":
		return "coroSemacquireWaitGroup"
	case "internal/poll.runtime_Semrelease":
		return "coroSemreleasePoll"
	}
	return ""
}
