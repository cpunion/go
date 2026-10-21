// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"fmt"
)

// waitGroupGoSignature recognizes the actual sync method and its complete
// receiver/callback signature. A similarly named user method is not a recipe.
func waitGroupGoSignature(fn *ir.Func) bool {
	if fn == nil || fn.Nname == nil || symbolName(fn.Nname) != "sync.(*WaitGroup).Go" {
		return false
	}
	t := fn.Type()
	if t == nil || t.Kind() != types.TFUNC || t.Recv() == nil ||
		t.NumParams() != 1 || t.NumResults() != 0 || t.IsVariadic() {
		return false
	}
	recv := t.Recv().Type
	if !recv.IsPtr() || recv.Elem().Kind() != types.TSTRUCT {
		return false
	}
	sym := recv.Elem().Sym()
	callback := t.Param(0).Type
	return sym != nil && sym.Pkg.Path == "sync" && sym.Name == "WaitGroup" &&
		callback.Kind() == types.TFUNC && callback.Recv() == nil &&
		callback.NumParams() == 0 && callback.NumResults() == 0 && !callback.IsVariadic() &&
		waitGroupMethod(recv, "Add") != nil && waitGroupMethod(recv, "Done") != nil
}

// Imported exported method symbols belong to the importing package. Use the
// actual method-set symbol rather than constructing sync.Add or sync.Done.
func waitGroupMethod(recv *types.Type, name string) *types.Sym {
	for _, method := range recv.Elem().Methods() {
		if method.Sym == nil || method.Type == nil || method.Sym.Name != name {
			continue
		}
		t := method.Type
		params := 0
		if name == "Add" {
			params = 1
		}
		if t.Kind() == types.TFUNC && t.Recv() != nil &&
			types.Identical(t.Recv().Type, recv) && !t.IsVariadic() &&
			t.NumResults() == 0 && t.NumParams() == params &&
			(params == 0 || types.Identical(t.Param(0).Type, types.Types[types.TINT])) {
			return method.Sym
		}
	}
	return nil
}

// AnalyzeWithCallbacks specializes only proved closed WaitGroup.Go call sites.
// The original serialized caller body still contains the ordinary method call.
// Private helpers get independent ordinary bodies and use existing factory and
// terminal/defer ABIs. No public callback parameter acquires a closed-set proof.
// Call this only after inlining, when native, uninstrumented lowering is enabled.
func AnalyzeWithCallbacks(funcs []*ir.Func, directives [][]string) (*Plan, error) {
	targets := packageFunctionTargets(funcs)
	specialized := make(map[*ir.Func]map[*ir.Name][]*ir.Func)
	bodies := make(map[*ir.Func]func() *ir.Func)
	demands := make(map[*ir.Func]bool)
	index := 0
	for _, fn := range funcs {
		if fn.OClosure != nil || !resumeFactorySupported(fn) {
			continue
		}
		ir.Visit(fn, func(node ir.Node) {
			call, ok := node.(*ir.CallExpr)
			if !ok || call.Op() != ir.OCALLFUNC || len(call.Args) != 2 || call.GoDefer {
				return
			}
			// Only replace an already static method expression. Resolving a
			// caller-local alias here would leave that local in the helper's
			// ordinary body; unwrapping an inlined callee could also discard
			// its initialization effects before the receiver and callback.
			if ir.StaticValue(call.Fun) != call.Fun || len(call.Fun.Init()) != 0 {
				return
			}
			name := ir.StaticCalleeName(ir.StaticValue(call.Fun))
			if name == nil || !waitGroupGoSignature(name.Func) {
				return
			}
			arg := call.Args[1]
			for arg.Op() == ir.OCONV || arg.Op() == ir.OCONVNOP {
				arg = arg.(*ir.ConvExpr).X
			}
			var closed []*ir.Func
			if name := ir.StaticCalleeName(arg); name != nil &&
				resumeFactorySupported(name.Func) && name.Type().NumRecvs() == 0 {
				closed = []*ir.Func{name.Func}
			} else if local, ok := arg.(*ir.Name); ok {
				closed = targets[fn][local]
			}
			if len(closed) == 0 {
				return
			}
			index++
			pos := call.Pos()
			recv := name.Type().Recv().Type
			callback := name.Type().Param(0).Type
			base := fmt.Sprintf("%s.wggo%d", fn.Sym().Name, index)
			newHelper := func(suffix string) *ir.Func {
				typ := types.NewSignature(nil, []*types.Field{
					types.NewField(pos, typecheck.Lookup("wg"), recv),
					types.NewField(pos, typecheck.Lookup("callback"), callback),
				}, nil)
				helper := ir.NewFunc(pos, pos, types.LocalPkg.Lookup(base+suffix), typ)
				helper.SetDupok(fn.Dupok())
				helper.Nname.Defn = helper
				helper.DeclareParams(true)
				return helper
			}
			newCleanup := func(suffix string) *ir.Func {
				typ := types.NewSignature(nil, []*types.Field{
					types.NewField(pos, typecheck.Lookup("wg"), recv),
				}, nil)
				helper := ir.NewFunc(pos, pos, types.LocalPkg.Lookup(base+".done"+suffix), typ)
				helper.SetDupok(fn.Dupok())
				helper.Nname.Defn = helper
				helper.DeclareParams(true)
				buildWaitGroupCleanup(helper)
				return helper
			}
			cleanup := newCleanup("")
			child := newHelper(".child")
			buildWaitGroupChild(child, cleanup)
			creator := newHelper("")
			ir.WithFunc(creator, func() {
				wg, f := creator.Dcl[0], creator.Dcl[1]
				add := typecheck.NewMethodExpr(pos, recv, waitGroupMethod(recv, "Add"))
				creator.Body = ir.Nodes{
					typecheck.Call(pos, add, ir.Nodes{wg, ir.NewInt(pos, 1)}, false),
					ir.NewGoDeferStmt(pos, ir.OGO,
						typecheck.Call(pos, child.Nname, ir.Nodes{wg, f}, false)),
				}
				typecheck.Stmts(creator.Body)
			})
			typecheck.Target.Funcs = append(typecheck.Target.Funcs, creator, child, cleanup)
			specialized[child] = map[*ir.Name][]*ir.Func{child.Dcl[1]: closed}
			for _, target := range closed {
				demands[target] = true
			}
			// Regenerate rather than shallow-copying mutable IR or capture cells.
			bodies[child] = func() *ir.Func {
				body := newHelper(".child.go")
				buildWaitGroupChild(body, cleanup)
				return body
			}
			bodies[cleanup] = func() *ir.Func { return newCleanup(".go") }
			original := call.Fun
			bodies[creator] = func() *ir.Func {
				body := newHelper(".go")
				ir.WithFunc(body, func() {
					body.Body = ir.Nodes{typecheck.Call(pos, ir.DeepCopy(pos, original),
						ir.Nodes{body.Dcl[0], body.Dcl[1]}, false)}
					typecheck.Stmts(body.Body)
				})
				return body
			}
			call.Fun = creator.Nname
		})
	}
	plan, err := analyze(typecheck.Target.Funcs, directives, specialized)
	if err != nil {
		return nil, err
	}
	plan.specializedBodies, plan.callbackFactories = bodies, demands
	return plan, nil
}

// buildWaitGroupChild preserves sync.WaitGroup.Go's recovery policy exactly:
// normal return and Goexit decrement the count; a non-nil recovered panic is
// rethrown without Done. In legacy panicnil mode recover may return nil, in
// which case the source method also completes the task and calls Done.
func buildWaitGroupChild(fn, cleanup *ir.Func) {
	pos := fn.Pos()
	ir.WithFunc(fn, func() {
		fn.Body = ir.Nodes{
			ir.NewGoDeferStmt(pos, ir.ODEFER, typecheck.Call(pos, cleanup.Nname, ir.Nodes{fn.Dcl[0]}, false)),
			typecheck.Call(pos, fn.Dcl[1], nil, false),
		}
		typecheck.Stmts(fn.Body)
	})
}

func buildWaitGroupCleanup(fn *ir.Func) {
	pos := fn.Pos()
	ir.WithFunc(fn, func() {
		wg := fn.Dcl[0]
		x := typecheck.TempAt(pos, fn, types.Types[types.TINTER])
		recoverCall := ir.NewCallExpr(pos, ir.ORECOVER, nil, nil)
		recoverCall.SetType(types.Types[types.TINTER])
		recoverCall.SetTypecheck(1)
		check := ir.NewIfStmt(pos, ir.NewBinaryExpr(pos, ir.ONE, x,
			ir.NewNilExpr(pos, x.Type())), ir.Nodes{ir.NewUnaryExpr(pos, ir.OPANIC, x)}, nil)
		check.SetInit(ir.Nodes{ir.NewDecl(pos, ir.ODCL, x), ir.NewAssignStmt(pos, x, recoverCall)})
		done := typecheck.NewMethodExpr(pos, wg.Type(), waitGroupMethod(wg.Type(), "Done"))
		fn.Body = ir.Nodes{check, typecheck.Call(pos, done, ir.Nodes{wg}, false)}
		typecheck.Stmts(fn.Body)
	})
}
