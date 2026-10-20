// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"slices"
	"testing"
)

func TestNamedFunctionTargets(t *testing.T) {
	prepareLowerTest(t)
	for _, test := range []struct {
		name string
		want []string
	}{
		{"bare", []string{"first"}}, {"explicit", []string{"first", "second"}},
		{"converted", []string{"first"}}, {"parallel", []string{"first"}},
		{"return-init", []string{"first", "second"}}, {"copy", []string{"first"}},
		{"chain", []string{"second"}}, {"cycle", []string{"first", "second"}},
		{"nil", nil}, {"nil-and-seed", []string{"first"}}, {"empty-cycle", nil},
		{"unknown", nil}, {"late-unknown", nil}, {"cycle-unknown", nil},
		{"address-flag", nil}, {"address", nil}, {"capture", nil},
		{"range", nil}, {"type-case", nil}, {"multi-assignment", nil},
		{"bodyless", nil}, {"wrong-class", nil}, {"other-function", nil},
		{"missing-name", nil}, {"bad-return", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/namedproof/"+test.name, "p")
			plain := types.NewSignature(nil, nil, nil)
			first := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("first"), plain)
			second := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("second"), plain)
			producer := func(symbol string) (*ir.Func, *ir.Name) {
				field := types.NewField(src.NoXPos, pkg.Lookup("result"), plain)
				fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(symbol), types.NewSignature(nil, nil, []*types.Field{field}))
				fn.DeclareParams(true)
				return fn, field.Nname.(*ir.Name)
			}
			choose, output := producer("choose")
			hop, other := producer("hop")
			parent := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("parent"), plain)
			local := func(fn *ir.Func, symbol string) *ir.Name {
				name := ir.NewNameAt(src.NoXPos, pkg.Lookup(symbol), plain)
				name.Class, name.Curfn = ir.PAUTO, fn
				fn.Dcl = append(fn.Dcl, name)
				return name
			}
			call := func(fn *ir.Func) *ir.CallExpr {
				n := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, fn.Nname, nil)
				n.SetType(plain)
				n.SetTypecheck(1)
				return n
			}
			ret := func(value ir.Node) *ir.ReturnStmt { return ir.NewReturnStmt(src.NoXPos, ir.Nodes{value}) }
			assign := func(lhs, rhs ir.Node) *ir.AssignStmt { return ir.NewAssignStmt(src.NoXPos, lhs, rhs) }
			f, alias := local(parent, "f"), local(parent, "alias")
			parent.Body = ir.Nodes{assign(f, call(choose)), assign(alias, f)}
			choose.Body = ir.Nodes{assign(output, first.Nname), newLowerTestReturn()}
			hop.Body = ir.Nodes{assign(other, second.Nname), newLowerTestReturn()}
			unknown := local(choose, "input")
			unknown.Class = ir.PPARAM
			switch test.name {
			case "explicit":
				choose.Body[1] = ret(second.Nname)
			case "converted":
				choose.Body[0] = assign(output, ir.NewConvExpr(src.NoXPos, ir.OCONVNOP, plain, first.Nname))
			case "parallel":
				choose.Body[0] = ir.NewAssignListStmt(src.NoXPos, ir.OAS2, ir.Nodes{output}, ir.Nodes{first.Nname})
			case "return-init":
				choose.Body[1].(*ir.ReturnStmt).SetInit(ir.Nodes{assign(output, second.Nname)})
			case "copy":
				a := local(choose, "a")
				choose.Body = append(ir.Nodes{assign(a, first.Nname), assign(output, a)}, choose.Body[1])
			case "chain":
				choose.Body[0] = assign(output, call(hop))
			case "cycle", "empty-cycle", "cycle-unknown":
				choose.Body = append(choose.Body, ret(call(hop)))
				hop.Body = append(hop.Body, ret(call(choose)))
				if test.name == "empty-cycle" {
					choose.Body, hop.Body = ir.Nodes{ret(call(hop))}, ir.Nodes{ret(call(choose))}
				} else if test.name == "cycle-unknown" {
					hop.Body = append(hop.Body, assign(other, unknown))
				}
			case "nil", "nil-and-seed":
				choose.Body = ir.Nodes{newLowerTestReturn()}
				if test.name == "nil-and-seed" {
					parent.Body = append(parent.Body, assign(f, first.Nname))
				}
			case "unknown":
				choose.Body[0] = assign(output, unknown)
			case "late-unknown":
				choose.Body = append(choose.Body, assign(output, unknown))
			case "address-flag":
				output.SetAddrtaken(true)
			case "address":
				choose.Body = append(choose.Body, ir.NewAddrExpr(src.NoXPos, output))
			case "capture":
				second.ClosureVars = []*ir.Name{ir.NewClosureVar(src.NoXPos, second, output)}
				choose.Body = append(choose.Body, &ir.ClosureExpr{Func: second})
			case "range":
				stmt := ir.NewRangeStmt(src.NoXPos, nil, nil, nil, nil, false)
				stmt.Value = output
				choose.Body = append(choose.Body, stmt)
			case "type-case":
				clause := ir.NewCaseStmt(src.NoXPos, nil, choose.Body)
				clause.Var = output
				choose.Body = ir.Nodes{ir.NewSwitchStmt(src.NoXPos, nil, []*ir.CaseClause{clause})}
			case "multi-assignment":
				choose.Body[0] = ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC, ir.Nodes{output}, ir.Nodes{call(hop)})
			case "bodyless":
				choose.Body = nil
				parent.Body = append(parent.Body, assign(f, first.Nname))
			case "wrong-class":
				output.Class = ir.PPARAM
			case "other-function":
				output.Curfn = parent
			case "missing-name":
				choose.Type().Result(0).Nname = nil
			case "bad-return":
				choose.Body[1] = ir.NewReturnStmt(src.NoXPos, ir.Nodes{first.Nname, second.Nname})
			}
			got := packageFunctionTargets([]*ir.Func{parent, choose, hop, first, second})[parent][alias]
			names := make([]string, len(got))
			for i, fn := range got {
				names[i] = fn.Sym().Name
			}
			if !slices.Equal(names, test.want) {
				t.Fatalf("targets=%v, want %v", names, test.want)
			}
		})
	}
}
