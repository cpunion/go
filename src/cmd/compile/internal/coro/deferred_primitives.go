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

// Deferred primitives need a cleanup activation rather than an operation at
// registration. Keep the frontend's frozen operands in place, and use private
// helpers with independent ordinary bodies for operations without a factory.
func specializeDeferredPrimitives(funcs []*ir.Func, reader *BodyReader,
	bodies map[*ir.Func]func() *ir.Func) map[*ir.Func]bool {
	rewritten := make(map[*ir.Func]bool)
	if reader == nil || reader.Have == nil || reader.Read == nil || reader.Clone == nil {
		return rewritten
	}
	index := 0
	imported := make(map[*ir.Func]*ir.Func)
	syncOwners := make(map[*ir.Func]bool)
	var localize func(*ir.Func) *ir.Func
	localize = func(target *ir.Func) *ir.Func {
		if helper := imported[target]; helper != nil {
			return helper
		}
		if target == nil || target.Nname == nil || target.Sym() == nil || target.Sym().Pkg == nil ||
			target.Type() == nil || target.Type().HasShape() ||
			target.Needctxt() || target.Wrapper() || len(target.ClosureVars) != 0 ||
			len(imported) >= 32 || !reader.Have(target) {
			return nil
		}
		path := target.Sym().Pkg.Path
		if path != "sync" && path != "internal/sync" {
			return nil
		}
		if _, intrinsic := operationRecipe(target); intrinsic {
			return nil
		}
		index++
		sym := types.LocalPkg.Lookup(fmt.Sprintf(".coro.defer.sync%d", index))
		helper := reader.Clone(target, sym)
		if helper == nil || len(helper.Body) == 0 {
			return nil
		}
		if !types.Identical(helper.Type(), target.Type()) ||
			!smallParameterBody(helper) {
			return nil
		}
		// Only concrete source bodies can use this private invocation ABI.
		// Shaped implementations carry an implicit dictionary that an ordinary
		// signature copy cannot reproduce. Keep their existing entry intact.
		// Reading a body creates its nested closures, but the independent
		// root is only an IR container until its caller registers it.
		typecheck.Target.Funcs = append(typecheck.Target.Funcs, helper)
		// Static calls carry the receiver as their first argument. A private
		// helper is a function rather than a method expression, so make that
		// input an explicit parameter for type checking and argument walking.
		if helper.Type().Recv() != nil {
			helper.Nname.SetType(types.NewSignature(nil, helper.Type().RecvParams(), helper.Type().Results()))
		}
		imported[target] = helper
		bodies[helper] = func() *ir.Func {
			return reader.Clone(target, types.LocalPkg.Lookup(sym.Name+".go"))
		}
		// Expose the same bounded sync implementation through the private
		// activation until its existing runtime operation recipes are reached.
		// Other packages, unknown calls and execution constraints keep their
		// existing admission and machine-stack checks.
		ir.Visit(helper, func(node ir.Node) {
			call, ok := node.(*ir.CallExpr)
			if !ok || call.Op() != ir.OCALLFUNC || call.GoDefer ||
				call.Fun == nil || len(call.Fun.Init()) != 0 {
				return
			}
			name := ir.StaticCalleeName(call.Fun)
			if name == nil || ir.StaticValue(call.Fun) != call.Fun {
				return
			}
			if child := localize(name.Func); child != nil {
				call.Fun = child.Nname
			}
		})
		return helper
	}
	for _, owner := range funcs {
		if owner.Wrapper() || !reader.Have(owner) {
			continue
		}
		ir.Visit(owner, func(node ir.Node) {
			deferred, ok := node.(*ir.GoDeferStmt)
			if !ok || deferred.Op() != ir.ODEFER {
				return
			}
			call, ok := deferred.Call.(*ir.CallExpr)
			if !ok || call.Op() != ir.OCALLFUNC || call.Fun == nil || len(call.Fun.Init()) != 0 {
				return
			}
			if closure, ok := call.Fun.(*ir.ClosureExpr); ok {
				wrapper := closure.Func
				if !wrapper.Wrapper() {
					return
				}
				if deferredSyncMethod(wrapper.WrappedFunc) {
					syncOwners[owner] = true
					// Inlining can replace the wrapper's original method call
					// with several statements. Its sole frozen receiver still
					// proves the complete invocation; do not evaluate it again.
					recv := wrapper.WrappedFunc.Type().Recv().Type
					if len(wrapper.ClosureVars) == 1 && types.Identical(wrapper.ClosureVars[0].Type(), recv) {
						if helper := localize(wrapper.WrappedFunc); helper != nil {
							ir.WithFunc(wrapper, func() {
								wrapper.Body = ir.Nodes{typecheck.Call(call.Pos(), helper.Nname, ir.Nodes{wrapper.ClosureVars[0]}, false)}
								typecheck.Stmts(wrapper.Body)
							})
							wrapper.WrappedFunc = helper
							rewritten[owner] = true
							return
						}
					}
				}
				if len(wrapper.Body) != 1 {
					return
				}
				if wrapper.WrappedFunc == nil {
					builtin := wrapper.Body[0]
					switch builtin.Op() {
					case ir.OCLOSE, ir.OPANIC:
						// A compiler-created builtin wrapper is private to this
						// registration. Its frozen capture cells already encode
						// the complete operand evaluation order.
					case ir.ORECOVER:
						// "defer recover()" cannot recover its owner's panic.
						// It must remain nil when the private wrapper becomes a
						// source cleanup literal with explicit recover support.
						if builtin.Type() == nil {
							return
						}
						wrapper.Body = append(ir.TakeInit(builtin), ir.NewAssignStmt(builtin.Pos(),
							ir.BlankNode, ir.NewNilExpr(builtin.Pos(), builtin.Type())))
						ir.WithFunc(wrapper, func() { typecheck.Stmts(wrapper.Body) })
					default:
						return
					}
					wrapper.SetWrapper(false)
					rewritten[owner] = true
					return
				}
				inner, ok := wrapper.Body[0].(*ir.CallExpr)
				if !ok || inner.Op() != ir.OCALLFUNC || inner.Fun == nil || len(inner.Fun.Init()) != 0 {
					return
				}
				name := ir.StaticCalleeName(inner.Fun)
				if name == nil || ir.StaticValue(inner.Fun) != inner.Fun || name.Func != wrapper.WrappedFunc ||
					!deferredSyncMethod(name.Func) {
					return
				}
				syncOwners[owner] = true
				if helper := localize(name.Func); helper != nil {
					inner.Fun, wrapper.WrappedFunc = helper.Nname, helper
					rewritten[owner] = true
				}
				return
			}
			name := ir.StaticCalleeName(call.Fun)
			if name == nil || call.Fun != name || name.Func == nil || len(call.Args) != 0 {
				return
			}
			target := name.Func
			recipe, known := operationRecipe(target)
			typ := target.Type()
			if !known || recipe.Kind != SiteYield && recipe.Kind != SiteGC ||
				typ == nil || typ.Kind() != types.TFUNC || typ.Recv() != nil ||
				typ.NumParams() != 0 || typ.NumResults() != 0 || typ.IsVariadic() {
				return
			}
			index++
			sym := types.LocalPkg.Lookup(fmt.Sprintf("%s.coro.deferop%d", owner.Sym().Name, index))
			build := func(suffix string) *ir.Func {
				pos := call.Pos()
				helper := ir.NewFunc(pos, pos, types.LocalPkg.Lookup(sym.Name+suffix), types.NewSignature(nil, nil, nil))
				helper.SetDupok(owner.Dupok())
				helper.Nname.Defn = helper
				helper.DeclareParams(true)
				ir.WithFunc(helper, func() {
					helper.Body = ir.Nodes{typecheck.Call(pos, target.Nname, nil, false)}
					typecheck.Stmts(helper.Body)
				})
				return helper
			}
			helper := build("")
			typecheck.Target.Funcs = append(typecheck.Target.Funcs, helper)
			bodies[helper] = func() *ir.Func { return build(".go") }
			call.Fun = helper.Nname
			rewritten[owner] = true
		})
	}
	// A cleanup receiver can also be used by Lock/RLock in the same owner.
	// Materialize those ordinary source calls through the same closed sync
	// implementation; otherwise a valid cleanup would inherit an unavailable
	// imported suspension dependency from the acquisition before it.
	for owner := range syncOwners {
		ir.Visit(owner, func(node ir.Node) {
			call, ok := node.(*ir.CallExpr)
			if !ok || call.Op() != ir.OCALLFUNC || call.GoDefer || call.Fun == nil ||
				len(call.Fun.Init()) != 0 || ir.StaticValue(call.Fun) != call.Fun {
				return
			}
			name := ir.StaticCalleeName(call.Fun)
			if name == nil {
				return
			}
			if helper := localize(name.Func); helper != nil {
				call.Fun = helper.Nname
				rewritten[owner] = true
			}
		})
	}
	return rewritten
}

func deferredSyncMethod(fn *ir.Func) bool {
	if fn == nil || fn.Nname == nil {
		return false
	}
	switch symbolName(fn.Nname) {
	case "sync.(*Mutex).Unlock", "sync.(*RWMutex).Unlock", "sync.(*RWMutex).RUnlock", "sync.(*WaitGroup).Done":
	default:
		return false
	}
	typ := fn.Type()
	if typ == nil || typ.Kind() != types.TFUNC || typ.Recv() == nil ||
		typ.NumParams() != 0 || typ.NumResults() != 0 || typ.IsVariadic() {
		return false
	}
	recv := typ.Recv().Type
	if !recv.IsPtr() || recv.Elem().Kind() != types.TSTRUCT || recv.Elem().Sym() == nil {
		return false
	}
	sym := recv.Elem().Sym()
	return sym.Pkg != nil && sym.Pkg.Path == "sync" && (sym.Name == "Mutex" || sym.Name == "RWMutex" || sym.Name == "WaitGroup")
}
