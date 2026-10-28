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

func TestRootArrayReconstructionProof(t *testing.T) {
	prepareLowerTest(t)
	oldTarget, oldPkg, oldFunc := typecheck.Target, types.LocalPkg, ir.CurFunc
	defer func() { typecheck.Target, types.LocalPkg, ir.CurFunc = oldTarget, oldPkg, oldFunc }()
	for _, tc := range []struct {
		name string
		size int64
		want bool
	}{
		{"eligible", 5120, true},
		{"lower-boundary", 512, true},
		{"below-boundary", 511, false},
		{"upper-boundary", 8192, true},
		{"small", 64, false},
		{"too-large", 8193, false},
		{"escape", 5120, false},
		{"direct-read", 5120, false},
		{"later-write", 5120, false},
		{"helper-write", 5120, false},
		{"helper-identity", 5120, false},
		{"helper-division", 5120, false},
		{"effectful-input", 5120, false},
		{"negative-index", 5120, false},
		{"out-of-bounds-index", 5120, false},
		{"dynamic-index", 5120, false},
		{"two-reads", 5120, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pos := src.NoXPos
			pkg := types.NewPkg("example.com/coro/arrayroot/"+tc.name, "arrayroot")
			types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
			integer, octet := types.Types[types.TINT], types.Types[types.TUINT8]
			arrayType := types.NewArray(octet, tc.size)
			makeFunction := func(name string, input, output *types.Type) *ir.Func {
				fn := ir.NewFunc(pos, pos, pkg.Lookup(name), types.NewSignature(nil,
					[]*types.Field{types.NewField(pos, pkg.Lookup("arg"), input)},
					[]*types.Field{types.NewField(pos, nil, output)}))
				fn.DeclareParams(true)
				typecheck.Target.Funcs = append(typecheck.Target.Funcs, fn)
				return fn
			}
			fn := makeFunction("sequence", integer, integer)
			helper := makeFunction("read", types.NewPtr(arrayType), integer)
			parameter := helper.Type().Param(0).Nname.(*ir.Name)
			index := func(base ir.Node, offset int64) *ir.IndexExpr {
				x := ir.NewIndexExpr(pos, base, typedInt(pos, integer, offset))
				x.SetType(octet)
				x.SetTypecheck(1)
				return x
			}
			convert := func(typ *types.Type, value ir.Node) *ir.ConvExpr {
				x := ir.NewConvExpr(pos, ir.OCONV, typ, value)
				x.SetTypecheck(1)
				return x
			}
			ret := func(value ir.Node) *ir.ReturnStmt {
				x := ir.NewReturnStmt(pos, ir.Nodes{value})
				x.SetTypecheck(1)
				return x
			}
			assign := func(left, right ir.Node) *ir.AssignStmt {
				x := ir.NewAssignStmt(pos, left, right)
				x.SetTypecheck(1)
				return x
			}
			helper.Body = ir.Nodes{ret(convert(integer, index(parameter, 0)))}
			array := typecheck.TempAt(pos, fn, arrayType)
			array.SetAddrtaken(true)
			zero := assign(array, nil)
			zero.SetInit(ir.Nodes{ir.NewDecl(pos, ir.ODCL, array)})
			write := assign(index(array, 0), convert(octet, fn.Type().Param(0).Nname.(*ir.Name)))
			call := ir.NewCallExpr(pos, ir.OCALLFUNC, helper.Nname, ir.Nodes{typecheck.NodAddr(array)})
			call.SetType(integer)
			call.SetTypecheck(1)
			fn.Body = ir.Nodes{zero, write, ret(call)}
			function := &Function{Func: fn, Edges: []Edge{{Kind: DirectCall, Node: call, Callee: helper}}}
			switch tc.name {
			case "negative-index":
				write.X.(*ir.IndexExpr).Index = typedInt(pos, integer, -1)
			case "out-of-bounds-index":
				write.X.(*ir.IndexExpr).Index = typedInt(pos, integer, tc.size)
			case "dynamic-index":
				write.X.(*ir.IndexExpr).Index = fn.Type().Param(0).Nname.(*ir.Name)
			case "escape":
				sink := ir.NewNameAt(pos, pkg.Lookup("sink"), types.NewPtr(arrayType))
				sink.Class = ir.PEXTERN
				sink.SetTypecheck(1)
				fn.Body = append(fn.Body[:2], assign(sink, typecheck.NodAddr(array)), ret(call))
			case "direct-read":
				x := ir.NewBinaryExpr(pos, ir.OADD, call, convert(integer, index(array, 1)))
				x.SetType(integer)
				x.SetTypecheck(1)
				fn.Body[2] = ret(x)
			case "later-write":
				fn.Body = append(fn.Body[:2], assign(index(array, 1), typedInt(pos, octet, 2)), ret(call))
			case "helper-write":
				helper.Body = append(ir.Nodes{assign(index(parameter, 1), typedInt(pos, octet, 2))}, helper.Body...)
			case "helper-identity":
				pointer := ir.NewConvExpr(pos, ir.OCONVNOP, types.Types[types.TUNSAFEPTR], parameter)
				pointer.SetTypecheck(1)
				helper.Body[0] = ret(convert(integer, convert(types.Types[types.TUINTPTR], pointer)))
			case "helper-division":
				x := ir.NewBinaryExpr(pos, ir.ODIV, index(parameter, 0), index(parameter, 1))
				x.SetType(octet)
				x.SetTypecheck(1)
				helper.Body[0] = ret(convert(integer, x))
			case "effectful-input":
				opaque := makeFunction("opaque", integer, octet)
				x := ir.NewCallExpr(pos, ir.OCALLFUNC, opaque.Nname, ir.Nodes{fn.Type().Param(0).Nname.(*ir.Name)})
				x.SetType(octet)
				x.SetTypecheck(1)
				write.Y = x
			case "two-reads":
				second := ir.NewCallExpr(pos, ir.OCALLFUNC, helper.Nname, ir.Nodes{typecheck.NodAddr(array)})
				second.SetType(integer)
				second.SetTypecheck(1)
				x := ir.NewBinaryExpr(pos, ir.OADD, call, second)
				x.SetType(integer)
				x.SetTypecheck(1)
				fn.Body[2] = ret(x)
				function.Edges = append(function.Edges, Edge{Kind: DirectCall, Node: second, Callee: helper})
			}
			plan := planRootArray(function)
			if (plan != nil) != tc.want {
				t.Fatalf("planRootArray present = %v, want %v", plan != nil, tc.want)
			}
			if plan != nil && (plan.array != array || plan.zero != zero || plan.write != write || plan.writeIndex != 0) {
				t.Fatal("reconstruction proof lost the source array or original evaluation points")
			}
			if plan == nil {
				return
			}
			public := newResumeFactory(fn, FactoryABI3)
			value := typecheck.TempAt(pos, public, integer)
			candidate := &lowerCandidate{function: function, factory: public,
				factoryABI: FactoryABI3, fusedSelf: true,
				parameters:   map[*ir.Name]*ir.Name{fn.Type().Param(0).Nname.(*ir.Name): public.Type().Param(1).Nname.(*ir.Name)},
				resultValues: []*ir.Name{value},
				resultPtrs:   []*ir.Name{public.Type().Param(2).Nname.(*ir.Name)},
			}
			prepareRootArray(candidate)
			lowered := candidate.rootArray
			if lowered == nil || candidate.factory == public || resumeFactoryABI(public, fn) != FactoryABI3 {
				t.Fatal("lost private implementation or changed the public factory ABI")
			}
			if candidate.factory.Type().NumParams() != public.Type().NumParams()+1 || !lowered.root.Type().IsBoolean() {
				t.Fatal("missing explicit independent-lineage selector")
			}
			if write.X != lowered.snapshot || write.Y == nil || fn.Body[0] != write || value.Curfn != candidate.factory {
				t.Fatal("lost the source conversion/evaluation point or result owner")
			}
			for call, original := range plan.uses {
				if call.Fun != original.Nname || call.Args[0] != lowered.buffer || len(call.Init()) != 1 || call.Init()[0].Typecheck() == 0 {
					t.Fatal("lost original readonly leaf or typed reconstruction")
				}
			}
			for _, name := range fn.Dcl {
				if name.Canonical() == array {
					t.Fatal("large source array still occupies an invocation frame")
				}
			}
		})
	}
}
