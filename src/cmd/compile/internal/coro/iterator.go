// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
)

// The range rewriter calls this compiler-owned declaration to diagnose an
// invalid yield protocol. It can panic, but cannot suspend. Match the private
// runtime declaration and its complete signature, rather than a source name.
func rangeStatePanicRecipe(fn *ir.Func) (OperationRecipe, bool) {
	if fn == nil || fn.Nname == nil || fn.Nname.Sym() == nil ||
		ir.Pkgs.Runtime == nil || fn.Nname.Sym().Pkg != ir.Pkgs.Runtime ||
		fn.Nname.Sym().Name != "panicrangestate" {
		return OperationRecipe{}, false
	}
	sig := fn.Type()
	if sig == nil || sig.Kind() != types.TFUNC || sig.HasShape() ||
		sig.NumRecvs() != 0 || sig.NumParams() != 1 || sig.NumResults() != 0 ||
		sig.IsVariadic() || sig.Param(0).Type != types.Types[types.TINT] {
		return OperationRecipe{}, false
	}
	return OperationRecipe{Terminal: MayPanic}, true
}

// A generated yield body has protocol branches and a panic helper even when
// the user's loop body is plain. A sealed iterator still needs its typed
// capture factory. The regular candidate checks own control flow, stack
// budgets and defer ownership; this is not an ordinary-call adapter.
func rangeBodyFactorySupported(function *Function) bool {
	if function == nil || function.Func == nil || function.Func.RangeParent == nil ||
		!capturedFactorySupported(function.Func) || function.Effect != NoSuspend ||
		function.Exec != 0 || function.Terminal&^MayPanic != 0 {
		return false
	}
	sig := function.Func.Type()
	return sig.NumRecvs() == 0 && sig.NumParams() <= 2 &&
		sig.NumResults() == 1 && sig.Result(0).Type == types.Types[types.TBOOL]
}
