// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
)

// notifySignature checks the sync declaration, not a user-defined layout that
// resembles notifyList. The runtime and sync retain their existing size check.
func notifySignature(typ *types.Type) bool {
	if typ == nil || typ.Kind() != types.TFUNC || typ.Recv() != nil ||
		typ.IsVariadic() || typ.NumParams() != 2 || typ.NumResults() != 0 ||
		!types.Identical(typ.Param(1).Type, types.Types[types.TUINT32]) {
		return false
	}
	list := typ.Param(0).Type
	if !list.IsPtr() || list.Elem().Kind() != types.TSTRUCT {
		return false
	}
	sym := list.Elem().Sym()
	return sym != nil && sym.Pkg.Path == "sync" && sym.Name == "notifyList"
}

func notifyOperation(call *ir.CallExpr) string {
	if call != nil &&
		symbolName(ir.StaticCalleeName(ir.StaticValue(call.Fun))) == "sync.runtime_notifyListWait" &&
		notifySignature(call.Fun.Type()) && len(call.Args) == 2 {
		return "coroNotifyListWait"
	}
	return ""
}

// waitOperation selects runtime entries sharing the frame-owned retry ABI.
func waitOperation(call *ir.CallExpr) string {
	if operation := notifyOperation(call); operation != "" {
		return operation
	}
	return semaphoreOperation(call)
}
