// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"testing"
)

func TestZeroInitializedFrameLocals(t *testing.T) {
	prepareLowerTest(t)
	for _, mode := range []string{"zero", "pointer-zero", "write", "read", "address", "init", "scalar"} {
		t.Run(mode, func(t *testing.T) {
			fn := testFunc("frameLocals")
			fn.DeclareParams(true)
			typ := types.NewArray(types.Types[types.TUINT8], 5120)
			if mode == "pointer-zero" {
				typ = types.NewArray(types.NewPtr(types.Types[types.TINT]), 4)
			}
			if mode == "scalar" {
				typ = types.Types[types.TINT]
			}
			local := typecheck.TempAt(src.NoXPos, fn, typ)
			other := typecheck.TempAt(src.NoXPos, fn, typ)
			declarations := ir.Nodes{
				ir.NewDecl(src.NoXPos, ir.ODCL, local),
				ir.NewAssignStmt(src.NoXPos, local, ir.NewZero(src.NoXPos, typ)),
			}
			write := ir.NewAssignStmt(src.NoXPos, local, other)
			switch mode {
			case "write":
				declarations.Append(write)
			case "read":
				declarations.Append(ir.NewAssignStmt(src.NoXPos, other, local))
			case "address":
				pointer := typecheck.TempAt(src.NoXPos, fn, types.NewPtr(typ))
				declarations.Append(ir.NewAssignStmt(src.NoXPos, pointer, typecheck.NodAddr(local)))
			case "init":
				state := typecheck.TempAt(src.NoXPos, fn, types.Types[types.TINT])
				assignment := ir.NewAssignStmt(src.NoXPos, state, ir.NewZero(src.NoXPos, state.Type()))
				assignment.PtrInit().Append(write)
				declarations.Append(assignment)
			}
			if got, want := zeroInitializedFrameLocals(declarations)[local], mode == "zero" || mode == "pointer-zero"; got != want {
				t.Fatalf("zeroInitializedFrameLocals includes local = %t, want %t", got, want)
			}
		})
	}
}

func TestExplicitFrameArrayInitializer(t *testing.T) {
	prepareLowerTest(t)
	oldTarget, oldLocalPkg := typecheck.Target, types.LocalPkg
	defer func() { typecheck.Target, types.LocalPkg = oldTarget, oldLocalPkg }()
	pkg := types.NewPkg("example.com/coro/frameinit", "frameinit")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	fn := newLowerTestFunc(pkg, "recursive")
	typ := types.NewArray(types.Types[types.TUINT8], 5120)
	local := fn.NewLocal(src.NoXPos, pkg.Lookup("data"), typ)
	sink := ir.NewNameAt(src.NoXPos, pkg.Lookup("sink"), typ)
	sink.Class = ir.PEXTERN
	call := newLowerTestCall(fn)
	fn.Body = ir.Nodes{
		ir.NewDecl(src.NoXPos, ir.ODCL, local),
		ir.NewAssignStmt(src.NoXPos, local, ir.NewZero(src.NoXPos, typ)),
		call,
		ir.NewAssignStmt(src.NoXPos, sink, local),
		newLowerTestReturn(),
	}
	function := &Function{
		Func: fn, Local: MaySuspend, Effect: MaySuspend, Primary: CoroPrimary,
		Edges: []Edge{{Kind: DirectCall, Callee: fn, CalleeName: symbolName(fn.Nname), Node: call}},
		Sites: []Site{{ID: 1, Kind: SiteAwait, Node: call}},
	}
	result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{fn: function}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Lowered != 1 || result.Skipped != 0 || function.Factory != FactoryABI3 {
		t.Fatalf("Lower = %+v, factory = %v, want one explicit frame", result, function.Factory)
	}
	initializers := 0
	for _, factory := range typecheck.Target.Funcs {
		if factory.Sym() != resumeFactorySymbol(fn) {
			continue
		}
		ir.VisitList(factory.Body, func(node ir.Node) {
			assignment, ok := node.(*ir.AssignStmt)
			if !ok {
				return
			}
			if _, ok := assignment.X.(*ir.SelectorExpr); ok &&
				assignment.X.Type().IsArray() && ir.IsZero(assignment.Y) {
				initializers++
			}
		})
	}
	if initializers != 1 {
		t.Fatalf("direct array field zero initializers = %d, want 1", initializers)
	}
}
