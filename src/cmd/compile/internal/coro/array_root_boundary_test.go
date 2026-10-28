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

func TestRootArrayCallerBoundary(t *testing.T) {
	prepareLowerTest(t)
	oldTarget, oldPkg, oldFunc := typecheck.Target, types.LocalPkg, ir.CurFunc
	defer func() { typecheck.Target, types.LocalPkg, ir.CurFunc = oldTarget, oldPkg, oldFunc }()
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"multiple-callers", true},
		{"reader-write", false},
		{"effectful-input", false},
		{"not-array", false},
		{"self-spawn", false},
		{"abi1", false},
		{"no-self-fusion", false},
		{"no-incoming-fusion", false},
		{"deferred-cleanup", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pos := src.NoXPos
			pkg := types.NewPkg("example.com/arrayboundary/"+tc.name, "arrayboundary")
			types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
			integer, octet := types.Types[types.TINT], types.Types[types.TUINT8]
			makeFn := func(name string, input, output *types.Type) *ir.Func {
				fn := ir.NewFunc(pos, pos, pkg.Lookup(name), types.NewSignature(nil,
					[]*types.Field{types.NewField(pos, pkg.Lookup("n"), input)},
					[]*types.Field{types.NewField(pos, nil, output)}))
				fn.DeclareParams(true)
				return fn
			}
			call := func(fn *ir.Func, argument ir.Node) *ir.CallExpr {
				n := ir.NewCallExpr(pos, ir.OCALLFUNC, fn.Nname, ir.Nodes{argument})
				n.SetType(fn.Type().Result(0).Type)
				n.SetTypecheck(1)
				return n
			}
			ret := func(value ir.Node) *ir.ReturnStmt {
				n := ir.NewReturnStmt(pos, ir.Nodes{value})
				n.SetTypecheck(1)
				return n
			}
			index := func(base ir.Node) *ir.IndexExpr {
				n := ir.NewIndexExpr(pos, base, typedInt(pos, integer, 0))
				n.SetType(octet)
				n.SetTypecheck(1)
				return n
			}
			arrayType := types.NewArray(octet, 5120)
			reader := makeFn("read", types.NewPtr(arrayType), integer)
			input := reader.Type().Param(0).Nname.(*ir.Name)
			value := ir.NewConvExpr(pos, ir.OCONV, integer, index(input))
			value.SetTypecheck(1)
			reader.Body = ir.Nodes{ret(value)}
			fn := makeFn("recursive", integer, integer)
			n := fn.Type().Param(0).Nname.(*ir.Name)
			array := typecheck.TempAt(pos, fn, arrayType)
			array.SetAddrtaken(true)
			zero := ir.NewAssignStmt(pos, array, nil)
			zero.SetInit(ir.Nodes{ir.NewDecl(pos, ir.ODCL, array)})
			converted := ir.NewConvExpr(pos, ir.OCONV, octet, n)
			converted.SetTypecheck(1)
			write := ir.NewAssignStmt(pos, index(array), converted)
			self := call(fn, n)
			read := call(reader, typecheck.NodAddr(array))
			sum := ir.NewBinaryExpr(pos, ir.OADD, self, read)
			sum.SetType(integer)
			sum.SetTypecheck(1)
			fn.Body = ir.Nodes{zero, write, ret(sum)}
			child := &lowerCandidate{function: &Function{Func: fn, Edges: []Edge{
				{Node: self, Callee: fn}, {Node: read, Callee: reader},
			}}, factoryABI: FactoryABI3, fusedSelf: true, fusedResume: true, fusedFrame: true,
				fusedAwaits: map[*ir.CallExpr]bool{self: true}}
			switch tc.name {
			case "reader-write":
				reader.Body = append(ir.Nodes{ir.NewAssignStmt(pos, index(input), typedInt(pos, octet, 3))}, reader.Body...)
			case "effectful-input":
				write.Y = call(makeFn("opaque", integer, octet), n)
			case "not-array":
				fn.Body = ir.Nodes{ret(self)}
			case "self-spawn":
				child.selfSpawn = true
				fn.Body = append(fn.Body[:2], ir.NewGoDeferStmt(pos, ir.OGO, self), ret(read))
			case "abi1":
				child.factoryABI = FactoryABI1
			case "no-self-fusion":
				child.fusedSelf = false
			case "no-incoming-fusion":
				child.fusedResume = false
			case "deferred-cleanup":
				child.defers = []*lowerDefer{{}}
			}
			oldResume, oldSelf := child.fusedResume, child.fusedSelf
			candidates := map[*ir.Func]*lowerCandidate{fn: child}
			var incoming []*ir.CallExpr
			var parents []*lowerCandidate
			for _, name := range []string{"caller1", "caller2"} {
				parent := makeFn(name, integer, integer)
				arg := parent.Type().Param(0).Nname.(*ir.Name)
				entry, parentSelf := call(fn, arg), call(parent, arg)
				parent.Body = ir.Nodes{ret(entry)}
				candidate := &lowerCandidate{function: &Function{Func: parent, Edges: []Edge{
					{Node: entry, Callee: fn}, {Node: parentSelf, Callee: parent},
				}}, factoryABI: FactoryABI3,
					fusedAwaits:   map[*ir.CallExpr]bool{entry: true, parentSelf: true},
					fusedRequests: map[*ir.CallExpr]bool{entry: true}}
				candidates[parent] = candidate
				incoming, parents = append(incoming, entry), append(parents, candidate)
			}
			preferRootArrayFrames(candidates)
			if child.fusedResume != (oldResume && !tc.want) || !child.fusedFrame || child.fusedSelf != oldSelf {
				t.Fatal("incorrect child header/fusion choice")
			}
			for i, parent := range parents {
				if parent.fusedAwaits[incoming[i]] == tc.want || parent.fusedRequests[incoming[i]] == tc.want {
					t.Fatal("incoming request and fused await must change together")
				}
				if len(parent.fusedAwaits) != 2-boolInt(tc.want) {
					t.Fatal("unrelated parent self-await was changed")
				}
			}
			if !child.fusedAwaits[self] {
				t.Fatal("child's serial self-await was removed")
			}
		})
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
