// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"fmt"
	"strings"
)

// specializeParameters creates sealed, per-target-set copies of local source
// functions. An ordinary caller must never acquire a call to one of these
// copies: its source body is restored if lowering rejects it.
func specializeParameters(funcs []*ir.Func, reader *BodyReader,
	parameters map[*ir.Func]map[*ir.Name][]*ir.Func,
	bodies map[*ir.Func]func() *ir.Func, demands map[*ir.Func]bool) map[*ir.Func]bool {
	rewritten := make(map[*ir.Func]bool)
	if reader == nil || reader.Clone == nil {
		return rewritten
	}
	type key struct {
		source *ir.Func
		proof  string
	}
	copies := make(map[key]*ir.Func)
	generated := make(map[*ir.Func]bool)
	work := append([]*ir.Func(nil), funcs...)
	creations, replacements := 0, 0
	var solved map[*ir.Func]map[*ir.Name][]*ir.Func
	for changed := true; changed && replacements < 256; {
		changed = false
		for index := 0; index < len(work) && replacements < 256; index++ {
			caller := work[index]
			if !resumeFactorySupported(caller) ||
				!reader.Have(caller) && parameters[caller] == nil {
				continue
			}
			if solved == nil {
				solved = packageFunctionTargetsWithParameters(typecheck.Target.Funcs, parameters)
			}
			targets := solved[caller]
			spawns := make(map[*ir.CallExpr]bool)
			ir.Visit(caller, func(node ir.Node) {
				if stmt, ok := node.(*ir.GoDeferStmt); ok && stmt.Op() == ir.OGO {
					spawns[stmt.Call.(*ir.CallExpr)] = true
				}
			})
			ir.Visit(caller, func(node ir.Node) {
				call, ok := node.(*ir.CallExpr)
				if !ok || call.Op() != ir.OCALLFUNC || replacements >= 256 {
					return
				}
				original := call
				var wrapper *ir.Func
				if call.GoDefer {
					if !spawns[call] {
						return
					}
					call = unwrapGoCall(call)
					if call != original {
						wrapper = original.Fun.(*ir.ClosureExpr).Func
					}
				}
				if ir.StaticValue(call.Fun) != call.Fun || len(call.Fun.Init()) != 0 {
					return
				}
				name := ir.StaticCalleeName(call.Fun)
				if name == nil || name.Func == nil || generated[name.Func] {
					return
				}
				source := name.Func
				if source.Sym().Pkg != types.LocalPkg || !resumeFactorySupported(source) ||
					source.Dupok() || source.Wrapper() || source.ABIWrapper() ||
					source.Type().NumRecvs() != 0 || source.Pragma & ^ir.Noinline != 0 ||
					!reader.Have(source) || len(call.Args) != source.Type().NumParams() ||
					!readOnlyCallbackParameters(source) {
					return
				}
				sets := make(map[int][]*ir.Func)
				var proof strings.Builder
				for i, field := range source.Type().Params() {
					if field.Type.Kind() != types.TFUNC {
						continue
					}
					arg := call.Args[i]
					for arg.Op() == ir.OCONV || arg.Op() == ir.OCONVNOP {
						arg = arg.(*ir.ConvExpr).X
					}
					var closed []*ir.Func
					if target := ir.StaticCalleeName(arg); target != nil &&
						resumeFactorySupported(target.Func) && target.Type().NumRecvs() == 0 {
						closed = []*ir.Func{target.Func}
					} else if local, ok := arg.(*ir.Name); ok {
						closed = targets[local]
					}
					if len(closed) == 0 || len(closed) > 16 {
						return
					}
					sets[i] = closed
					fmt.Fprintf(&proof, "%d:", i)
					for _, target := range closed {
						fmt.Fprintf(&proof, "%p;", target)
					}
				}
				if len(sets) == 0 {
					return
				}
				k := key{source, proof.String()}
				helper, known := copies[k]
				if !known {
					if creations >= 64 {
						return
					}
					if !smallParameterBody(source) {
						return
					}
					creations++
					sym := types.LocalPkg.Lookup(fmt.Sprintf("%s.param%d", source.Sym().Name, creations))
					helper = reader.Clone(source, sym)
					if helper == nil {
						copies[k] = nil
						return
					}
					typecheck.Target.Funcs = append(typecheck.Target.Funcs, helper)
					generated[helper] = true
					if !readOnlyCallbackParameters(helper) {
						// Keep the independent copy ordinary and unreachable. Its
						// nested closures still have a valid compiled parent.
						copies[k] = nil
						return
					}
					copies[k] = helper
					parameters[helper] = make(map[*ir.Name][]*ir.Func)
					for i, closed := range sets {
						param := helper.Type().Param(i).Nname.(*ir.Name)
						parameters[helper][param] = closed
						for _, target := range closed {
							demands[target] = true
						}
					}
					bodies[helper] = func() *ir.Func {
						return reader.Clone(source, types.LocalPkg.Lookup(sym.Name+".go"))
					}
					work = append(work, helper)
				}
				if helper == nil {
					return
				}
				if wrapper == nil {
					call.Fun = helper.Nname
				} else {
					// Read the proof from the creator's already-frozen operands,
					// but rewrite the real wrapper, not unwrapGoCall's IR copy.
					// Keep its captures and argument evaluation in their original
					// positions. Ordinary callers restore their source bodies.
					wrapper.Body[0].(*ir.CallExpr).Fun = helper.Nname
					wrapper.WrappedFunc = helper
				}
				if reader.Have(caller) {
					rewritten[caller] = true
				}
				replacements++
				changed = true
				solved = nil
			})
		}
	}
	return rewritten
}

// Count enclosed bodies as well: cloning one small outer function must not
// multiply an arbitrarily large closure body by the specialization limit.
func smallParameterBody(fn *ir.Func) bool {
	nodes := 0
	seen := make(map[*ir.Func]bool)
	var visit func(*ir.Func)
	visit = func(fn *ir.Func) {
		if seen[fn] || nodes > 1024 {
			return
		}
		seen[fn] = true
		ir.Visit(fn, func(node ir.Node) {
			nodes++
			if closure, ok := node.(*ir.ClosureExpr); ok {
				visit(closure.Func)
			}
		})
	}
	visit(fn)
	return nodes <= 1024
}

func readOnlyCallbackParameters(fn *ir.Func) bool {
	parameters := make(map[*ir.Name]bool)
	for _, field := range fn.Type().Params() {
		if field.Type.Kind() == types.TFUNC {
			name, ok := field.Nname.(*ir.Name)
			if !ok || name.Addrtaken() {
				return false
			}
			parameters[name] = true
		}
	}
	if len(parameters) == 0 {
		return false
	}
	valid := true
	write := func(node ir.Node) {
		if name, ok := node.(*ir.Name); ok && parameters[name.Canonical()] {
			valid = false
		}
	}
	ir.Visit(fn, func(node ir.Node) {
		switch node := node.(type) {
		case *ir.AssignStmt:
			write(node.X)
		case *ir.AssignListStmt:
			for _, lhs := range node.Lhs {
				write(lhs)
			}
		case *ir.RangeStmt:
			write(node.Key)
			write(node.Value)
		case *ir.AddrExpr:
			write(node.X)
		case *ir.ClosureExpr:
			for _, name := range node.Func.ClosureVars {
				write(name.Canonical())
			}
		}
	})
	return valid
}
