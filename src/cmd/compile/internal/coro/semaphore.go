// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
)

// semaphoreSignature is shared by the acquire and release declarations.
// A changed declaration must not silently acquire a different runtime ABI.
func semaphoreSignature(typ *types.Type) bool {
	return typ != nil && typ.Kind() == types.TFUNC && typ.Recv() == nil &&
		!typ.IsVariadic() && typ.NumParams() == 3 && typ.NumResults() == 0 &&
		types.Identical(typ.Param(0).Type, types.NewPtr(types.Types[types.TUINT32])) &&
		types.Identical(typ.Param(1).Type, types.Types[types.TBOOL]) &&
		types.Identical(typ.Param(2).Type, types.Types[types.TINT])
}

func semaphoreOperation(call *ir.CallExpr) string {
	if call == nil || len(call.Args) != 3 || !semaphoreSignature(call.Fun.Type()) {
		return ""
	}
	name := ir.StaticCalleeName(ir.StaticValue(call.Fun))
	switch symbolName(name) {
	case "internal/sync.runtime_SemacquireMutex",
		"sync.runtime_SemacquireRWMutexR", "sync.runtime_SemacquireRWMutex":
		return "coroSemacquireMutex"
	case "internal/sync.runtime_Semrelease", "sync.runtime_Semrelease":
		return "coroSemrelease"
	}
	return ""
}
