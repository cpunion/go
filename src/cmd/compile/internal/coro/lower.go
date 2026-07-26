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
	"go/constant"
	"slices"
	"strings"
)

const (
	actionInvalid uint8 = iota
	actionYield
	actionWait
	actionComplete
)

// LowerResult summarizes one package lowering pass.
type LowerResult struct {
	Lowered int
	Skipped int
}

type lowerCandidate struct {
	function     *Function
	transitions  map[*ir.CallExpr]SiteKind
	dependencies map[*ir.Func]bool
	factory      *ir.Func
}

type lowerState struct {
	body       []ir.Node
	transition SiteKind
	call       *ir.CallExpr
}

// Lower rewrites supported coroutine primaries into explicit state machines.
// The initial lowering accepts linear, parameterless functions containing
// yield, structured call, and spawn sites. Unsupported functions remain
// unchanged.
func Lower(plan *Plan) (LowerResult, error) {
	var result LowerResult
	if err := plan.Verify(); err != nil {
		return result, err
	}

	candidates := make(map[*ir.Func]*lowerCandidate)
	functions := make([]*Function, 0, len(plan.Functions))
	for _, function := range plan.Functions {
		functions = append(functions, function)
	}
	slices.SortFunc(functions, func(a, b *Function) int {
		return strings.Compare(ir.PkgFuncName(a.Func), ir.PkgFuncName(b.Func))
	})
	for _, function := range functions {
		if function.Primary != CoroPrimary {
			continue
		}
		candidate, err := newLowerCandidate(function)
		if err != nil {
			continue
		}
		candidates[function.Func] = candidate
	}

	// Remove callers whose structured children cannot use this ABI. Iterate to
	// a fixed point so one unsupported leaf removes every dependent caller.
	for changed := true; changed; {
		changed = false
		for fn, candidate := range candidates {
			for dependency := range candidate.dependencies {
				if candidates[dependency] == nil {
					delete(candidates, fn)
					changed = true
					break
				}
			}
		}
	}

	for _, function := range functions {
		candidate := candidates[function.Func]
		if candidate == nil {
			continue
		}
		candidate.factory = newResumeFactory(candidate.function.Func)
	}
	for _, function := range functions {
		candidate := candidates[function.Func]
		if candidate == nil {
			continue
		}
		if err := lowerFunction(candidate, candidates); err != nil {
			return result, err
		}
		result.Lowered++
	}
	for _, function := range functions {
		if function.Primary == CoroPrimary {
			result.Skipped++
		}
	}
	result.Skipped -= result.Lowered
	return result, nil
}

func newLowerCandidate(function *Function) (*lowerCandidate, error) {
	fn := function.Func
	if fn == nil || fn.OClosure != nil {
		return nil, fmt.Errorf("not a top-level function")
	}
	if function.Recursive {
		return nil, fmt.Errorf("recursive function")
	}
	sig := fn.Type()
	if sig.NumRecvs()+sig.NumParams()+sig.NumResults() != 0 {
		return nil, fmt.Errorf("parameters or results")
	}
	if len(function.Sites) == 0 {
		return nil, fmt.Errorf("no coroutine sites")
	}

	topCalls := make(map[*ir.CallExpr]bool)
	goCalls := make(map[*ir.CallExpr]bool)
	for i, stmt := range fn.Body {
		switch stmt := stmt.(type) {
		case *ir.CallExpr:
			topCalls[stmt] = true
		case *ir.Decl, *ir.AssignStmt, *ir.AssignListStmt,
			*ir.AssignOpStmt:
		case *ir.GoDeferStmt:
			if stmt.Op() != ir.OGO {
				return nil, fmt.Errorf("defer is not supported")
			}
			call, ok := stmt.Call.(*ir.CallExpr)
			if !ok {
				return nil, fmt.Errorf("non-call go statement")
			}
			goCalls[call] = true
		case *ir.ReturnStmt:
			if i != len(fn.Body)-1 || len(stmt.Results) != 0 {
				return nil, fmt.Errorf("non-terminal return")
			}
		default:
			switch stmt.Op() {
			case ir.OBLOCK, ir.OBREAK, ir.OCONTINUE, ir.OFALL,
				ir.OFOR, ir.OGOTO, ir.OIF, ir.OLABEL, ir.ORANGE,
				ir.OSELECT, ir.OSWITCH:
				return nil, fmt.Errorf("control flow %s", stmt.Op())
			}
		}
	}

	edges := make(map[*ir.CallExpr]Edge)
	for _, edge := range function.Edges {
		edges[edge.Node] = edge
	}
	candidate := &lowerCandidate{
		function:     function,
		transitions:  make(map[*ir.CallExpr]SiteKind),
		dependencies: make(map[*ir.Func]bool),
	}
	for _, site := range function.Sites {
		call, ok := site.Node.(*ir.CallExpr)
		if !ok {
			return nil, fmt.Errorf("non-call site %d", site.ID)
		}
		switch site.Kind {
		case SiteYield:
			if !topCalls[call] {
				return nil, fmt.Errorf("nested yield site %d", site.ID)
			}
		case SiteAwait:
			if !topCalls[call] {
				return nil, fmt.Errorf("nested await site %d", site.ID)
			}
			edge := edges[call]
			if edge.Callee == nil {
				return nil, fmt.Errorf("dynamic await site %d", site.ID)
			}
			candidate.dependencies[edge.Callee] = true
		case SiteSpawn:
			if !goCalls[call] {
				return nil, fmt.Errorf("nested spawn site %d", site.ID)
			}
			edge := edges[call]
			if edge.Callee == nil {
				return nil, fmt.Errorf("dynamic spawn site %d", site.ID)
			}
			candidate.dependencies[edge.Callee] = true
		default:
			return nil, fmt.Errorf("unsupported site %s", site.Kind)
		}
		candidate.transitions[call] = site.Kind
	}
	return candidate, nil
}

func stacklessResumeType() *types.Type {
	ctx := types.NewField(src.NoXPos, types.LocalPkg.Lookup(".coroctx"),
		types.Types[types.TUNSAFEPTR])
	action := types.NewField(src.NoXPos, nil, types.Types[types.TUINT8])
	return types.NewSignature(nil, []*types.Field{ctx}, []*types.Field{action})
}

func newResumeFactory(fn *ir.Func) *ir.Func {
	pos := fn.Pos()
	resumeType := stacklessResumeType()
	result := types.NewField(pos, nil, resumeType)
	typ := types.NewSignature(nil, nil, []*types.Field{result})
	sym := fn.Sym().Pkg.Lookup(fn.Sym().Name + ".coro")
	factory := ir.NewFunc(pos, pos, sym, typ)
	factory.SetDupok(fn.Dupok())
	factory.Nname.Defn = factory
	factory.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, factory)
	return factory
}

func lowerFunction(candidate *lowerCandidate, candidates map[*ir.Func]*lowerCandidate) error {
	function := candidate.function
	fn := function.Func
	factory := candidate.factory
	resumeType := factory.Type().Result(0).Type
	pos := fn.Pos()

	// Source locals become factory locals. Returning the closure then places
	// the captured slots in a typed, GC-scanned heap object.
	fnDcl := make([]*ir.Name, 0, len(fn.Dcl))
	for _, name := range fn.Dcl {
		switch name.Class {
		case ir.PAUTO, ir.PAUTOHEAP:
			name.Curfn = factory
			factory.Dcl = append(factory.Dcl, name)
		default:
			fnDcl = append(fnDcl, name)
		}
	}
	fn.Dcl = fnDcl

	var declarations []ir.Node
	declared := make(map[*ir.Name]bool)
	addDeclaration := func(decl *ir.Decl) {
		name := decl.X.Canonical()
		if !declared[name] {
			declared[name] = true
			declarations = append(declarations, decl)
		}
	}

	states := []lowerState{{}}
	for i, stmt := range fn.Body {
		if call, ok := stmt.(*ir.CallExpr); ok {
			switch candidate.transitions[call] {
			case SiteYield, SiteAwait:
				state := &states[len(states)-1]
				state.transition = candidate.transitions[call]
				state.call = call
				states = append(states, lowerState{})
				continue
			}
		}
		switch stmt := stmt.(type) {
		case *ir.Decl:
			addDeclaration(stmt)
			continue
		case *ir.ReturnStmt:
			if i == len(fn.Body)-1 {
				continue
			}
		}
		states[len(states)-1].body = append(states[len(states)-1].body, stmt)
	}

	pc := typecheck.TempAt(pos, factory, types.Types[types.TUINT32])
	declarations = append(declarations, ir.NewDecl(pos, ir.ODCL, pc))

	resume := ir.NewClosureFunc(pos, pos, ir.OCLOSURE, resumeType, factory,
		typecheck.Target, 0)
	resume.DeclareParams(true)
	ctx := resume.Dcl[0]

	captured := make(map[*ir.Name]*ir.Name)
	capture := func(name *ir.Name) *ir.Name {
		name = name.Canonical()
		if closure := captured[name]; closure != nil {
			return closure
		}
		// Initializers execute after the closure has been created. Force
		// capture by reference, rather than loading an uninitialized slot.
		name.Defn = nil
		closure := ir.NewClosureVar(name.Pos(), resume, name)
		captured[name] = closure
		return closure
	}
	resumePC := capture(pc)

	factoryCall := func(call *ir.CallExpr) (ir.Node, error) {
		edge := edgeForCall(function, call)
		child := candidates[edge.Callee]
		if child == nil {
			return nil, fmt.Errorf("%s: missing factory for %s",
				ir.PkgFuncName(fn), edge.CalleeName)
		}
		return typecheck.Call(call.Pos(), child.factory.Nname, nil, false), nil
	}

	var edit func(ir.Node) ir.Node
	edit = func(node ir.Node) ir.Node {
		switch node := node.(type) {
		case *ir.Decl:
			addDeclaration(node)
			return ir.NewBlockStmt(node.Pos(), nil)
		case *ir.AssignStmt:
			node.Def = false
		case *ir.AssignListStmt:
			node.Def = false
		}
		if name, ok := node.(*ir.Name); ok {
			switch name.Class {
			case ir.PAUTO, ir.PAUTOHEAP, ir.PPARAM, ir.PPARAMOUT:
				if name.Curfn == factory || name.Canonical().Curfn == factory {
					return capture(name)
				}
			}
			return node
		}
		ir.EditChildren(node, edit)
		return node
	}

	cases := make([]*ir.CaseClause, len(states))
	for i, state := range states {
		body := make([]ir.Node, 0, len(state.body)+4)
		for _, stmt := range state.body {
			if goStmt, ok := stmt.(*ir.GoDeferStmt); ok {
				if call, ok := goStmt.Call.(*ir.CallExpr); ok &&
					candidate.transitions[call] == SiteSpawn {
					child, err := factoryCall(call)
					if err != nil {
						return err
					}
					body = append(body, typecheck.Call(goStmt.Pos(),
						typecheck.LookupRuntime("coroSpawn"),
						ir.Nodes{ctx, child}, false))
					continue
				}
			}
			body = append(body, edit(stmt))
		}

		action := actionComplete
		if i+1 < len(states) {
			next := typedInt(pos, types.Types[types.TUINT32], int64(i+1))
			body = append(body, ir.NewAssignStmt(pos, resumePC, next))
			switch state.transition {
			case SiteYield:
				action = actionYield
			case SiteAwait:
				child, err := factoryCall(state.call)
				if err != nil {
					return err
				}
				body = append(body, typecheck.Call(state.call.Pos(),
					typecheck.LookupRuntime("coroAwait"),
					ir.Nodes{ctx, child}, false))
				action = actionWait
			default:
				return fmt.Errorf("%s: state %d has no transition",
					ir.PkgFuncName(fn), i)
			}
		}
		body = append(body, ir.NewReturnStmt(pos, []ir.Node{
			typedInt(pos, types.Types[types.TUINT8], int64(action)),
		}))
		label := typedInt(pos, types.Types[types.TUINT32], int64(i))
		cases[i] = ir.NewCaseStmt(pos, []ir.Node{label}, body)
	}
	resume.Body = []ir.Node{
		ir.NewSwitchStmt(pos, resumePC, cases),
		ir.NewReturnStmt(pos, []ir.Node{
			typedInt(pos, types.Types[types.TUINT8], int64(actionInvalid)),
		}),
	}

	oldCurFunc := ir.CurFunc
	ir.CurFunc = resume
	typecheck.Stmts(resume.Body)

	ir.CurFunc = factory
	factory.Body = append(declarations,
		ir.NewReturnStmt(pos, []ir.Node{resume.OClosure}))
	typecheck.Stmts(factory.Body)

	ir.CurFunc = fn
	newResume := typecheck.Call(pos, factory.Nname, nil, false)
	run := typecheck.Call(pos, typecheck.LookupRuntime("coroRun"),
		ir.Nodes{newResume}, false)
	fn.Body = []ir.Node{run, ir.NewReturnStmt(pos, nil)}
	typecheck.Stmts(fn.Body)
	ir.CurFunc = oldCurFunc

	// The source inline body no longer describes the physical primary.
	fn.Inl = nil
	return nil
}

func edgeForCall(function *Function, call *ir.CallExpr) Edge {
	for _, edge := range function.Edges {
		if edge.Node == call {
			return edge
		}
	}
	return Edge{}
}

func typedInt(pos src.XPos, typ *types.Type, value int64) ir.Node {
	return ir.NewBasicLit(pos, typ, constant.MakeInt64(value))
}
