// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssagen

import (
	"cmd/compile/internal/coro"
	"cmd/compile/internal/ir"
	"internal/abi"
	"internal/buildcfg"
)

func emitCoroStackInfo(fn *ir.Func) {
	if buildcfg.Experiment.Coro && coro.NativeStackEntry(fn) {
		fn.LSym.Func().FuncFlag |= abi.FuncFlagCoroStack
	}
}
