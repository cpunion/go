// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"fmt"
	"slices"
)

type rangeHybridRegistration struct {
	statement *ir.GoDeferStmt
	parent    *ir.Func
	cleanup   *lowerDefer
	frozen    *ir.Name
	token     *ir.Name
	anchor    *ir.Name
	target    *ir.Func
}

func (candidate *lowerCandidate) hasLocalDefers() bool {
	return slices.ContainsFunc(candidate.defers, func(deferred *lowerDefer) bool {
		return deferred.rangeAnchor == nil
	})
}

func rangeBodyContains(body *rangeDeferBodyPlan, statement *ir.GoDeferStmt) bool {
	return body != nil && body.owner.err == nil && slices.ContainsFunc(body.cleanup.defers,
		func(deferred *lowerDefer) bool { return deferred.statement == statement })
}

// The callback only registers these defers. Their recover/Goexit effects
// execute later in the enclosing activation, so they must not prevent a
// registration-only callback from supplying its demanded typed factory.
// Every non-defer edge retains the existing range-body terminal restriction.
func rangeDeferRegistrationFactorySupported(plan *Plan, body *rangeDeferBodyPlan) bool {
	if body == nil || body.owner.err != nil || body.function.LocalTerminal&^MayPanic != 0 {
		return false
	}
	registration := *body.function
	registration.Exec = registration.LocalExec
	allExec := registration.LocalExec
	for _, edge := range body.function.Edges {
		if edge.Kind == GoCall {
			return false
		}
		summary, known := plan.edgeSummary(edge)
		if known {
			allExec |= summary.Exec
		}
		if edge.Kind == DeferCall && slices.ContainsFunc(body.cleanup.defers,
			func(deferred *lowerDefer) bool { return deferred.call == edge.Node }) {
			// Only an exactly validated enclosing-owner registration can
			// defer its execution constraints as well as terminal effects.
			// Direct calls and local execution requirements remain live.
			continue
		}
		if !known {
			return false
		}
		if summary.Terminal&^MayPanic != 0 {
			return false
		}
		registration.Exec |= summary.Exec
	}
	if allExec != body.function.Exec {
		return false
	}
	registration.Terminal &= MayPanic
	return rangeBodyFactorySupported(&registration)
}

// prepareRangeHybridBodies materializes ordinary source before any enclosing
// frame rewrites. Each native cleanup body is an independent copy of the
// validated working IR, bound to the ordinary callback's operand cells.
func prepareRangeHybridBodies(plan *Plan, owners map[*ir.Func]*rangeDeferOwnerPlan,
	candidates map[*ir.Func]*lowerCandidate, reader *BodyReader) {
	for _, owner := range owners {
		if owner.err != nil || candidates[owner.function] == nil {
			continue
		}
		for _, body := range owner.bodies {
			fn := body.function.Func
			var ordinary *ir.Func
			if build := plan.specializedBodies[fn]; build != nil {
				ordinary = build()
			} else if reader != nil && reader.Have(fn) {
				ordinary = reader.Read(fn)
			}
			if ordinary == nil {
				owner.err = fmt.Errorf("%s: ordinary range callback body unavailable", ir.PkgFuncName(fn))
				break
			}
			body.ordinary = ordinary
			if candidate := candidates[fn]; candidate != nil {
				candidate.ordinary = ordinary
			}
			var statements []*ir.GoDeferStmt
			ir.Visit(ordinary, func(node ir.Node) {
				if statement, ok := node.(*ir.GoDeferStmt); ok && statement.DeferAt != nil {
					statements = append(statements, statement)
				}
			})
			if len(statements) != len(body.cleanup.defers) {
				owner.err = fmt.Errorf("%s: ordinary range registration sites differ", ir.PkgFuncName(fn))
				break
			}
			for i, original := range statements {
				registration, err := prepareRangeHybridRegistration(plan, body.cleanup.defers[i], original, ordinary)
				if err != nil {
					owner.err = fmt.Errorf("%s: %w", ir.PkgFuncName(fn), err)
					break
				}
				owner.hybrids = append(owner.hybrids, registration)
			}
			if owner.err != nil {
				break
			}
		}
	}
}

func prepareRangeHybridRegistration(plan *Plan, source *lowerDefer, ordinary *ir.GoDeferStmt,
	parent *ir.Func) (*rangeHybridRegistration, error) {
	call, ok := ordinary.Call.(*ir.CallExpr)
	lifted := plan.operandCalls[source.statement]
	var selected *ir.Name
	if lifted != nil && lifted.created {
		if closure, ok := source.call.Fun.(*ir.ClosureExpr); ok && closure.Func == lifted.wrapper &&
			len(closure.Func.ClosureVars) == 1 {
			selected = closure.Func.ClosureVars[0]
		}
	}
	if !ok || ordinary.Op() != ir.ODEFER || call.Fun == nil || len(call.Args) != 0 ||
		(selected == nil && !types.Identical(source.call.Fun.Type(), call.Fun.Type()) ||
			selected != nil && !types.Identical(selected.Type(), call.Fun.Type())) || ordinary.DeferAt == nil ||
		!ordinary.DeferAt.Type().IsEmptyInterface() {
		return nil, fmt.Errorf("ordinary range defer has no matching normalized call")
	}
	pos := ordinary.Pos()
	frozen := typecheck.TempAt(pos, parent, call.Fun.Type())
	token := typecheck.TempAt(pos, parent, types.Types[types.TUNSAFEPTR])
	anchor := typecheck.TempAt(pos, parent, types.Types[types.TINTER])
	copy := *source
	copy.terminalTargets = slices.Clone(source.terminalTargets)
	copy.literalCaptures, copy.captures = nil, nil
	copy.rangeAnchor, copy.rangeToken = nil, nil
	copy.call = ir.Copy(source.call).(*ir.CallExpr)
	copy.call.SetInit(nil)
	copy.statement = ir.NewGoDeferStmt(pos, ir.ODEFER, copy.call)
	if closure, ok := source.call.Fun.(*ir.ClosureExpr); ok {
		if selected != nil {
			copy.call.Fun = copyRangeDeferBody(closure.Func, parent, map[*ir.Name]*ir.Name{selected: frozen})
			return &rangeHybridRegistration{statement: ordinary, parent: parent,
				cleanup: &copy, frozen: frozen, token: token, anchor: anchor}, nil
		}
		original, ok := call.Fun.(*ir.ClosureExpr)
		if !ok {
			return nil, fmt.Errorf("ordinary range defer lost its source function body")
		}
		bindings, err := bindRangeDeferCaptureCells(closure.Func, original.Func)
		if err != nil {
			return nil, err
		}
		copy.call.Fun = copyRangeDeferBody(closure.Func, parent, bindings)
	} else if copy.open || len(copy.terminalTargets) != 0 {
		copy.call.Fun = frozen
	} else if ir.StaticCalleeName(source.call.Fun) == nil {
		return nil, fmt.Errorf("ordinary range defer has no validated static or closed target")
	}
	return &rangeHybridRegistration{statement: ordinary, parent: parent,
		cleanup: &copy, frozen: frozen, token: token, anchor: anchor}, nil
}

// finishRangeHybridRegistration runs only after the enclosing owner and every
// required terminal factory have survived admission's fixed point.
func finishRangeHybridRegistration(registration *rangeHybridRegistration,
	factories map[*ir.Func]*ir.Func) error {
	deferred := registration.cleanup
	parent, token := registration.parent, registration.token
	if deferred.open {
		prepareResumableDeferWrapper(deferred, parent)
		if err := rewriteOpenDeferCall(deferred, token); err != nil {
			return err
		}
	} else if len(deferred.resumable) != 0 {
		prepareResumableDeferWrapper(deferred, parent)
		if err := rewriteResumableDeferCall(deferred, token, factories); err != nil {
			return err
		}
	} else if len(deferred.terminalTargets) != 0 {
		prepareDynamicDeferWrapper(deferred, parent)
		if err := rewriteDynamicDeferCall(deferred, token, factories); err != nil {
			return err
		}
	} else if deferred.directGoexit {
		if _, err := rewriteDeferGoexitCall(deferred, parent, token, deferred.terminalTarget); err != nil {
			return err
		}
	} else if deferred.runTerminal {
		if _, err := rewriteDeferFactoryCall(deferred, parent, token,
			deferred.terminalTarget, factories[deferred.terminalTarget]); err != nil {
			return err
		}
	}
	if closure, ok := deferred.call.Fun.(*ir.ClosureExpr); ok {
		registration.target = closure.Func
		if deferred.rewriteTerminal != 0 {
			captured := ir.NewClosureVar(deferred.statement.Pos(), closure.Func, token)
			if err := rewriteDeferTerminal(closure.Func, captured, deferred.rewriteTerminal); err != nil {
				return err
			}
		}
	} else {
		registration.target = ir.StaticCalleeName(deferred.call.Fun).Func
	}
	return nil
}

func installRangeHybridRegistrations(body *rangeDeferBodyPlan) {
	byStatement := make(map[*ir.GoDeferStmt]*rangeHybridRegistration)
	for _, registration := range body.owner.hybrids {
		if registration.parent == body.ordinary {
			byStatement[registration.statement] = registration
		}
	}
	var edit func(ir.Node) ir.Node
	edit = func(node ir.Node) ir.Node {
		statement, ok := node.(*ir.GoDeferStmt)
		if !ok || byStatement[statement] == nil {
			ir.EditChildren(node, edit)
			return node
		}
		r := byStatement[statement]
		pos := statement.Pos()
		call := statement.Call.(*ir.CallExpr)
		prefix := ir.TakeInit(statement)
		prefix = append(prefix, ir.TakeInit(call)...)
		freeze := ir.NewAssignStmt(pos, r.frozen, call.Fun)
		freeze.Def, r.frozen.Defn = true, freeze
		call.Fun = r.frozen
		prefix = append(prefix, ir.NewDecl(pos, ir.ODCL, r.frozen), freeze,
			ir.NewDecl(pos, ir.ODCL, r.anchor), ir.NewAssignStmt(pos, r.anchor, statement.DeferAt))
		owner := ir.NewAssignStmt(pos, r.token, typecheck.Call(pos,
			typecheck.LookupRuntime("coroRangeDeferOwner"), ir.Nodes{r.anchor}, false))
		owner.Def, r.token.Defn = true, owner
		native := ir.Nodes{ir.NewDecl(pos, ir.ODCL, r.token), owner}
		native = append(native, r.cleanup.statement.Init()...)
		value := r.cleanup.call.Fun
		if _, closure := value.(*ir.ClosureExpr); !closure {
			value = r.frozen
		}
		native = append(native, typecheck.Call(pos, typecheck.LookupRuntime("coroRangeDeferPush"),
			ir.Nodes{r.anchor, value}, false))
		condition := typecheck.Call(pos, typecheck.LookupRuntime("coroRangeDeferIsNative"), ir.Nodes{r.anchor}, false)
		prefix = append(prefix, ir.NewIfStmt(pos, condition, native, ir.Nodes{statement}))
		return ir.NewBlockStmt(pos, prefix)
	}
	for i, node := range body.ordinary.Body {
		body.ordinary.Body[i] = edit(node)
	}
}

func appendRangeDeferRecord(pos src.XPos, stack ir.Node, function, anchor ir.Node) ir.Node {
	element := stack.Type().Elem()
	value := ir.NewCompLitExpr(pos, ir.OCOMPLIT, element, []ir.Node{
		ir.NewStructKeyExpr(pos, element.Field(0), function),
		ir.NewStructKeyExpr(pos, element.Field(1), anchor),
	})
	appendCall := typecheck.Call(pos, types.BuiltinPkg.Lookup("append").Def.(*ir.Name), ir.Nodes{stack, value}, false)
	return ir.NewAssignStmt(pos, stack, appendCall)
}

func rangeBodyEntryAnchors(candidate *lowerCandidate) []*ir.Name {
	var result []*ir.Name
	for _, deferred := range candidate.defers {
		if anchor, ok := deferred.rangeAnchor.(*ir.Name); ok && !slices.Contains(result, anchor) {
			result = append(result, anchor)
		}
	}
	return result
}
