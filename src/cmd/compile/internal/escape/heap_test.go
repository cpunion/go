// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package escape

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/obj/x86"
	"cmd/internal/src"
	"sync"
	"testing"
)

var initHeapTest sync.Once

func prepareHeapTest() {
	initHeapTest.Do(func() {
		types.PtrSize, types.RegSize, types.MaxWidth = 8, 8, 1<<50
		base.Ctxt = obj.Linknew(&x86.Linkamd64)
		types.LocalPkg = types.NewPkg("example.com/escape/heap", "heap")
		typecheck.InitUniverse()
	})
}

func TestHeapLocals(t *testing.T) {
	prepareHeapTest()
	for _, mode := range []string{"return", "noescape", "uintptr", "uintptr-keepalive", "closure", "closure-value"} {
		t.Run(mode, func(t *testing.T) {
			pkg := types.NewPkg("example.com/escape/heap/"+mode, mode)
			var results []*types.Field
			if mode == "return" {
				results = []*types.Field{types.NewField(src.NoXPos, nil, types.NewPtr(types.Types[types.TINT]))}
			}
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("caller"),
				types.NewSignature(nil, nil, results))
			fn.Nname.Defn = fn
			fn.DeclareParams(true)
			local := fn.NewLocal(src.NoXPos, pkg.Lookup("local"), types.Types[types.TINT])
			fn.Body = ir.Nodes{
				ir.NewDecl(src.NoXPos, ir.ODCL, local),
				ir.NewAssignStmt(src.NoXPos, local, ir.NewZero(src.NoXPos, local.Type())),
			}
			var call *ir.CallExpr
			var conv *ir.ConvExpr
			var callee *ir.Func
			if mode == "closure" || mode == "closure-value" {
				resultType := types.NewPtr(local.Type())
				if mode == "closure-value" {
					resultType = local.Type()
				}
				result := types.NewField(src.NoXPos, nil, resultType)
				closure := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
					types.NewSignature(nil, nil, []*types.Field{result}),
					fn, new(ir.Package), 0)
				closure.DeclareParams(true)
				captured := ir.NewClosureVar(src.NoXPos, closure, local)
				value := ir.Node(captured)
				if mode == "closure" {
					value = typecheck.NodAddr(captured)
				}
				closure.Body = ir.Nodes{ir.NewReturnStmt(src.NoXPos,
					ir.Nodes{value})}
				sink := ir.NewNameAt(src.NoXPos, pkg.Lookup("sink"), closure.Type())
				sink.Class = ir.PEXTERN
				fn.Body.Append(ir.NewAssignStmt(src.NoXPos, sink, closure.OClosure))
			} else {
				argType := types.NewPtr(local.Type())
				if mode == "uintptr" || mode == "uintptr-keepalive" {
					argType = types.Types[types.TUINTPTR]
				}
				param := types.NewField(src.NoXPos, pkg.Lookup("p"), argType)
				var calleeResults []*types.Field
				if mode == "return" {
					calleeResults = []*types.Field{types.NewField(src.NoXPos, nil, argType)}
				}
				callee = ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("callee"),
					types.NewSignature(nil, []*types.Field{param}, calleeResults))
				callee.Nname.Defn = callee
				callee.DeclareParams(true)
				param.Note = "original"
				arg := ir.Node(typecheck.NodAddr(local))
				if mode == "return" {
					callee.Body = ir.Nodes{ir.NewReturnStmt(src.NoXPos,
						ir.Nodes{param.Nname.(*ir.Name)})}
				} else if mode == "noescape" {
					callee.Pragma |= ir.Noescape
				} else {
					if mode == "uintptr" {
						callee.Pragma |= ir.UintptrEscapes
					}
					pointer := ir.NewConvExpr(src.NoXPos, ir.OCONVNOP, types.Types[types.TUNSAFEPTR], arg)
					conv = ir.NewConvExpr(src.NoXPos, ir.OCONVNOP, argType, pointer)
					arg = conv
				}
				call = ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, callee.Nname, ir.Nodes{arg})
				if mode == "return" {
					call.SetType(argType)
					fn.Body.Append(ir.NewReturnStmt(src.NoXPos, ir.Nodes{call}))
				} else {
					fn.Body.Append(call)
				}
			}
			all := []*ir.Func{fn}
			if callee != nil {
				all = append(all, callee)
			}
			for i := 0; i < 2; i++ {
				decls := len(fn.Dcl)
				if got, want := HeapLocals(all)[local], mode == "return" || mode == "uintptr" || mode == "closure"; got != want {
					t.Fatalf("HeapLocals includes local = %t, want %t", got, want)
				}
				if fn.Esc() != escFuncUnknown || fn.ClosureResultsLost() || local.Opt != nil ||
					local.Esc() != ir.EscUnknown || local.Byval() || len(fn.Dcl) != decls {
					t.Fatal("heap analysis changed caller annotations or declarations")
				}
				if callee != nil && (callee.Esc() != escFuncUnknown ||
					callee.Type().Param(0).Note != "original" || callee.Pragma&ir.UintptrKeepAlive != 0) {
					t.Fatal("heap analysis retained callee annotations")
				}
				if conv != nil && (conv.X.Op() != ir.OCONVNOP || len(call.Init()) != 0 || len(call.KeepAlive) != 0) {
					t.Fatal("heap analysis rewrote uintptr argument")
				}
			}
			Funcs(all)
			if got, want := local.Esc() == ir.EscHeap, mode == "return" || mode == "uintptr" || mode == "closure"; got != want {
				t.Fatalf("subsequent Funcs heap allocation = %t, want %t", got, want)
			}
		})
	}
}

func TestHeapLocalsGoDefer(t *testing.T) {
	prepareHeapTest()
	for _, op := range []ir.Op{ir.OGO, ir.ODEFER} {
		t.Run(op.String(), func(t *testing.T) {
			pkg := types.NewPkg("example.com/escape/heap/"+op.String(), "heap")
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("caller"), types.NewSignature(nil, nil, nil))
			fn.Nname.Defn = fn
			fn.DeclareParams(true)
			local := fn.NewLocal(src.NoXPos, pkg.Lookup("local"), types.Types[types.TINT])
			wrapper := ir.NewClosureFunc(src.NoXPos, src.NoXPos, op,
				types.NewSignature(nil, nil, nil), fn, new(ir.Package), 0)
			wrapper.DeclareParams(true)
			captured := ir.NewClosureVar(src.NoXPos, wrapper, local)
			sink := ir.NewNameAt(src.NoXPos, pkg.Lookup("sink"), types.NewPtr(local.Type()))
			sink.Class = ir.PEXTERN
			wrapper.Body = ir.Nodes{ir.NewAssignStmt(src.NoXPos, sink, typecheck.NodAddr(captured))}
			call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, wrapper.OClosure, nil)
			statement := ir.NewGoDeferStmt(src.NoXPos, op, call)
			fn.Body = ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, local), statement}
			addrtaken := local.Addrtaken()
			for i := 0; i < 2; i++ {
				if !HeapLocals([]*ir.Func{fn})[local] {
					t.Fatal("published local does not escape")
				}
				if statement.Esc() != ir.EscUnknown || wrapper.OClosure.IsGoWrap ||
					wrapper.Esc() != escFuncUnknown || wrapper.ClosureResultsLost() ||
					local.Opt != nil || local.Addrtaken() != addrtaken || local.Byval() {
					t.Fatal("heap analysis changed go/defer annotations")
				}
			}
			Funcs([]*ir.Func{fn})
			if local.Esc() != ir.EscHeap {
				t.Fatal("subsequent Funcs lost the published local")
			}
		})
	}
}
