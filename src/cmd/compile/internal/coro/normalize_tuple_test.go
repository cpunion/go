// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"slices"
	"testing"
)

func TestNormalizeTupleCallInitializers(t *testing.T) {
	for _, argumentInit := range []bool{false, true} {
		t.Run(map[bool]string{false: "call", true: "argument"}[argumentInit], func(t *testing.T) {
			testNormalizeTupleInitializers(t, argumentInit)
		})
	}
}

func testNormalizeTupleInitializers(t *testing.T, argumentInit bool) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	for _, mode := range []string{"return", "plain-return", "tuple-return", "condition", "statement", "short-circuit", "observable-prefix", "loop", "inlined", "inlined-init", "plain-init"} {
		t.Run(mode, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/tupleinit/"+mode, "p")
			types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
			fields := func() []*types.Field {
				return []*types.Field{types.NewField(src.NoXPos, nil, types.Types[types.TINT]), types.NewField(src.NoXPos, nil, types.Types[types.TBOOL])}
			}
			one := func() []*types.Field { return fields()[:1] }
			results := one()
			if mode == "tuple-return" {
				results = fields()
			} else if mode == "condition" || mode == "short-circuit" || mode == "loop" {
				results = fields()[1:]
			}
			caller := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("caller"), types.NewSignature(nil, nil, results))
			caller.DeclareParams(true)
			typecheck.Target.Funcs = append(typecheck.Target.Funcs, caller)
			producer := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("producer"), types.NewSignature(nil, nil, fields()))
			consumer := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("consumer"), types.NewSignature(nil, fields(), results))
			prefix := newLowerTestCall(ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("prefix"), types.NewSignature(nil, nil, nil)))
			inner := newLowerTestCall(producer)
			inner.SetType(producer.Type().ResultsTuple())
			var values ir.Nodes
			assignment := ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC, nil, ir.Nodes{inner})
			for i, field := range fields() {
				value := caller.NewLocal(src.NoXPos, pkg.LookupNum("arg", i), field.Type)
				value.Defn = assignment
				values.Append(value)
				assignment.PtrInit().Append(ir.NewDecl(src.NoXPos, ir.ODCL, value))
			}
			assignment.Lhs = values
			outer := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, consumer.Nname, values)
			outer.SetType(results[0].Type)
			if mode == "tuple-return" {
				outer.SetType(consumer.Type().ResultsTuple())
			}
			outer.SetTypecheck(1)
			original := ir.Nodes{prefix, assignment}
			var owner ir.Node = outer
			if argumentInit {
				conversion := ir.NewConvExpr(src.NoXPos, ir.OCONVNOP, values[0].Type(), values[0])
				conversion.SetTypecheck(1)
				outer.Args = slices.Clone(values)
				outer.Args[0] = conversion
				owner = conversion
			}
			owner.(ir.InitNode).SetInit(original)
			arguments := slices.Clone(outer.Args)
			ret := ir.NewReturnStmt(src.NoXPos, ir.Nodes{outer})
			var root ir.Node = ret
			switch mode {
			case "condition":
				root = ir.NewIfStmt(src.NoXPos, outer, nil, nil)
			case "statement":
				root = outer
			case "short-circuit":
				ret.Results[0] = ir.NewLogicalExpr(src.NoXPos, ir.OANDAND, ir.NewBool(src.NoXPos, true), outer)
			case "observable-prefix":
				prior := newLowerTestCall(ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("prior"), types.NewSignature(nil, nil, one())))
				prior.SetType(types.Types[types.TINT])
				ret.Results[0] = ir.NewBinaryExpr(src.NoXPos, ir.OADD, prior, outer)
			case "loop":
				root = ir.NewForStmt(src.NoXPos, nil, outer, nil, nil, false)
			case "inlined", "inlined-init":
				n := ir.NewInlinedCallExpr(src.NoXPos, ir.Nodes{outer}, ir.Nodes{ir.NewInt(src.NoXPos, 0)})
				n.SetType(results[0].Type)
				if mode == "inlined-init" {
					n.SetInit(ir.TakeInit(owner))
					owner = n
				}
				ret.Results[0] = n
			}
			caller.Body = ir.Nodes{root}
			sites := []Site{{ID: 1, Kind: SiteAwait, Node: inner}}
			if mode != "plain-return" && mode != "plain-init" {
				sites = append(sites, Site{ID: 2, Kind: SiteAwait, Node: outer})
			}
			if mode == "plain-init" {
				other := newLowerTestCall(producer)
				other.SetType(producer.Type().ResultsTuple())
				caller.Body = append(caller.Body, ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC, ir.Nodes{ir.BlankNode, ir.BlankNode}, ir.Nodes{other}))
				sites = []Site{{ID: 1, Kind: SiteAwait, Node: other}}
			}
			function := &Function{Func: caller, Effect: MaySuspend, Primary: CoroPrimary, Sites: sites}
			normalizeSingleResultCalls(function)
			safe := mode == "return" || mode == "plain-return" || mode == "tuple-return" || mode == "condition" || mode == "statement"
			if !safe {
				if len(root.Init()) != 0 || !slices.Equal(owner.Init(), original) {
					t.Fatal("initializer crossed an unsupported expression boundary")
				}
				return
			}
			want := 3
			if mode == "plain-return" || mode == "statement" {
				want = 2
			}
			if len(root.Init()) != want || root.Init()[0] != prefix || root.Init()[1] != assignment || root != owner && len(owner.Init()) != 0 {
				t.Fatalf("ordered initializer prefix was not exposed: root=%v owner=%v", root.Init(), owner.Init())
			}
			for i, value := range values {
				if outer.Args[i] != arguments[i] || value.Name().Defn != assignment || assignment.Init()[i].Op() != ir.ODCL {
					t.Fatal("tuple argument cells or declarations changed")
				}
			}
			before := slices.Clone(root.Init())
			normalizeSingleResultCalls(function)
			if !slices.Equal(root.Init(), before) || root != owner && len(owner.Init()) != 0 {
				t.Fatal("initializer normalization is not idempotent")
			}
		})
	}
}

func TestLowerTupleCallInitializers(t *testing.T) {
	prepareLowerTest(t)
	oldPkg, oldTarget := types.LocalPkg, typecheck.Target
	defer func() { types.LocalPkg, typecheck.Target = oldPkg, oldTarget }()
	for _, suspendingConsumer := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "await"}[suspendingConsumer], func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/tupleinitlower/"+t.Name(), "p")
			types.LocalPkg, typecheck.Target = pkg, new(ir.Package)
			fields := func() []*types.Field {
				return []*types.Field{types.NewField(src.NoXPos, nil, types.Types[types.TINT]), types.NewField(src.NoXPos, nil, types.Types[types.TBOOL])}
			}
			newFunction := func(name string, params, results []*types.Field) *ir.Func {
				fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name), types.NewSignature(nil, params, results))
				fn.Nname.Defn = fn
				fn.DeclareParams(true)
				typecheck.Target.Funcs = append(typecheck.Target.Funcs, fn)
				return fn
			}
			producer := newFunction("producer", nil, fields())
			consumer := newFunction("consumer", fields(), fields()[:1])
			chooser := newFunction("chooser", nil, []*types.Field{types.NewField(src.NoXPos, nil, consumer.Type())})
			parent := newFunction("parent", nil, fields()[:1])
			yield := ir.NewFunc(src.NoXPos, src.NoXPos, types.NewPkg("runtime", "runtime").Lookup("Gosched"), types.NewSignature(nil, nil, nil))
			producer.Body = ir.Nodes{newLowerTestCall(yield), ir.NewReturnStmt(src.NoXPos, ir.Nodes{ir.NewInt(src.NoXPos, 7), ir.NewBool(src.NoXPos, true)})}
			chooser.Body = ir.Nodes{newLowerTestCall(yield), ir.NewReturnStmt(src.NoXPos, ir.Nodes{consumer.Nname})}
			consumer.Body = ir.Nodes{ir.NewReturnStmt(src.NoXPos, ir.Nodes{consumer.Type().Param(0).Nname.(*ir.Name)})}
			if suspendingConsumer {
				consumer.Body.Prepend(newLowerTestCall(yield))
			}
			inner := newLowerTestCall(producer)
			inner.SetType(producer.Type().ResultsTuple())
			assignment := ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC, nil, ir.Nodes{inner})
			for i, field := range fields() {
				n := parent.NewLocal(src.NoXPos, pkg.LookupNum("arg", i), field.Type)
				n.Defn = assignment
				assignment.Lhs.Append(n)
				assignment.PtrInit().Append(ir.NewDecl(src.NoXPos, ir.ODCL, n))
			}
			callee := parent.NewLocal(src.NoXPos, pkg.Lookup("callee"), consumer.Type())
			choose := newLowerTestCall(chooser)
			choose.SetType(consumer.Type())
			calleeAssignment := ir.NewAssignStmt(src.NoXPos, callee, choose)
			callee.Defn = calleeAssignment
			calleeAssignment.SetInit(ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, callee)})
			outer := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, callee, slices.Clone(assignment.Lhs))
			outer.SetType(types.Types[types.TINT])
			outer.SetTypecheck(1)
			outer.SetInit(ir.Nodes{calleeAssignment})
			conversion := ir.NewConvExpr(src.NoXPos, ir.OCONVNOP, outer.Args[0].Type(), outer.Args[0])
			conversion.SetTypecheck(1)
			conversion.SetInit(ir.Nodes{assignment})
			outer.Args[0] = conversion
			parent.Body = ir.Nodes{ir.NewReturnStmt(src.NoXPos, ir.Nodes{outer})}
			plan, err := Analyze([]*ir.Func{parent, producer, consumer, chooser}, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Both producer and callee initializers must be detached before
			// the consumer can be normalized. Preserve their source order.
			normalizeSingleResultCalls(plan.Functions[parent])
			prefix := parent.Body[0].Init()
			if len(prefix) < 2 || prefix[0] != calleeAssignment || prefix[1] != assignment {
				t.Fatalf("callee/argument prefix order = %v", prefix)
			}
			if outer.Fun != callee || inner.Fun != producer.Nname || choose.Fun != chooser.Nname {
				t.Fatal("normalization changed the source callee identities")
			}
			want := 3
			if suspendingConsumer {
				want = 4
			}
			result, err := Lower(plan, nil)
			if err != nil || result.Lowered != want || len(result.Diagnostics) != 0 {
				t.Fatalf("Lower=(%+v,%v), want %d", result, err, want)
			}
			// Lowering rewrites an ordinary call's local callee access to
			// frame storage. Await calls retain their source call nodes.
			if inner.Fun != producer.Nname || suspendingConsumer && outer.Fun != callee || choose.Fun != chooser.Nname || len(outer.Init()) != 0 || len(conversion.Init()) != 0 {
				t.Fatal("source call identity or initializer ownership changed")
			}
		})
	}
}
