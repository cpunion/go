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
	Lowered     int
	Skipped     int
	Diagnostics []string
}

type lowerCandidate struct {
	function     *Function
	transitions  map[*ir.CallExpr]SiteKind
	foreignCalls map[*ir.CallExpr]ForeignCallClass
	dependencies map[*ir.Func]bool
	parameters   map[*ir.Name]*ir.Name
	results      map[*ir.Name]*ir.Name
	resultValues []*ir.Name
	resultPtrs   []*ir.Name
	factory      *ir.Func
}

type lowerState struct {
	body       []ir.Node
	transition SiteKind
	call       *ir.CallExpr
	statement  ir.Node
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
			result.Diagnostics = append(result.Diagnostics,
				fmt.Sprintf("%s: %v", ir.PkgFuncName(function.Func), err))
			continue
		}
		candidates[function.Func] = candidate
	}

	// Remove callers whose structured children cannot use this ABI. Iterate to
	// a fixed point so one unsupported leaf removes every dependent caller.
	for changed := true; changed; {
		changed = false
		for _, function := range functions {
			fn := function.Func
			candidate := candidates[fn]
			if candidate == nil {
				continue
			}
			var unsupported []string
			for dependency := range candidate.dependencies {
				if candidates[dependency] == nil {
					unsupported = append(unsupported, ir.PkgFuncName(dependency))
				}
			}
			if len(unsupported) == 0 {
				continue
			}
			slices.Sort(unsupported)
			result.Diagnostics = append(result.Diagnostics,
				fmt.Sprintf("%s: unsupported coroutine dependency %s",
					ir.PkgFuncName(fn), unsupported[0]))
			delete(candidates, fn)
			changed = true
		}
	}

	for _, function := range functions {
		candidate := candidates[function.Func]
		if candidate == nil {
			continue
		}
		candidate.factory = newResumeFactory(candidate.function.Func)
		candidate.parameters = make(map[*ir.Name]*ir.Name)
		candidate.results = make(map[*ir.Name]*ir.Name)
		for i, field := range candidate.function.Func.Type().Params() {
			source, _ := field.Nname.(*ir.Name)
			target, _ := candidate.factory.Type().Param(i).Nname.(*ir.Name)
			if source != nil && target != nil {
				candidate.parameters[source.Canonical()] = target
			}
		}
		paramCount := candidate.function.Func.Type().NumParams()
		for i, field := range candidate.function.Func.Type().Results() {
			source, _ := field.Nname.(*ir.Name)
			value := typecheck.TempAt(candidate.function.Func.Pos(),
				candidate.factory, field.Type)
			pointer, _ := candidate.factory.Type().Param(paramCount + i).Nname.(*ir.Name)
			if source != nil {
				candidate.results[source.Canonical()] = value
			}
			candidate.resultValues = append(candidate.resultValues, value)
			candidate.resultPtrs = append(candidate.resultPtrs, pointer)
		}
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
	if sig.NumRecvs() != 0 {
		return nil, fmt.Errorf("receiver")
	}
	if sig.IsVariadic() {
		return nil, fmt.Errorf("variadic parameters")
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
		case *ir.AssignStmt:
			ir.Visit(stmt, func(node ir.Node) {
				if call, ok := node.(*ir.CallExpr); ok {
					topCalls[call] = true
				}
			})
		case *ir.AssignListStmt:
			ir.Visit(stmt, func(node ir.Node) {
				if call, ok := node.(*ir.CallExpr); ok {
					topCalls[call] = true
				}
			})
		case *ir.Decl, *ir.AssignOpStmt:
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
			if i != len(fn.Body)-1 ||
				len(stmt.Results) != 0 && len(stmt.Results) != sig.NumResults() {
				return nil, fmt.Errorf("non-terminal return")
			}
		default:
			switch stmt.Op() {
			case ir.OBLOCK, ir.OFOR, ir.OIF, ir.ORANGE, ir.OSWITCH:
				hasReturn := false
				ir.Visit(stmt, func(node ir.Node) {
					if node.Op() == ir.ORETURN {
						hasReturn = true
					}
				})
				if hasReturn {
					return nil, fmt.Errorf("nested return")
				}
			case ir.OBREAK, ir.OCONTINUE, ir.OFALL, ir.OGOTO,
				ir.OLABEL, ir.OSELECT:
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
		foreignCalls: make(map[*ir.CallExpr]ForeignCallClass),
		dependencies: make(map[*ir.Func]bool),
	}
	for _, site := range function.Sites {
		call, ok := site.Node.(*ir.CallExpr)
		if !ok {
			return nil, fmt.Errorf("non-call site %d", site.ID)
		}
		switch site.Kind {
		case SiteYield, SiteTimer, SiteFile, SitePoll:
			if !topCalls[call] {
				return nil, fmt.Errorf("nested %s site %d", site.Kind, site.ID)
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
		case SiteForeign:
			if site.Foreign != DirectNoBlock && site.Foreign != DirectMayBlock &&
				site.Foreign != AsyncOperation {
				return nil, fmt.Errorf("unsupported foreign site %s", site.Foreign)
			}
			if site.Foreign != DirectNoBlock && !topCalls[call] {
				return nil, fmt.Errorf("nested %s foreign site %d",
					site.Foreign, site.ID)
			}
			candidate.foreignCalls[call] = site.Foreign
			if site.Foreign != AsyncOperation {
				continue
			}
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
	params := make([]*types.Field, fn.Type().NumParams())
	for i, field := range fn.Type().Params() {
		params[i] = types.NewField(field.Pos, field.Sym, field.Type)
	}
	for i, field := range fn.Type().Results() {
		sym := fn.Sym().Pkg.LookupNum(".cororesult", i)
		params = append(params,
			types.NewField(field.Pos, sym, types.NewPtr(field.Type)))
	}
	typ := types.NewSignature(nil, params, []*types.Field{result})
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
	for _, result := range candidate.resultValues {
		declarations = append(declarations, ir.NewDecl(pos, ir.ODCL, result))
	}
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
		if call := transitionCall(stmt, candidate.transitions); call != nil {
			switch candidate.transitions[call] {
			case SiteYield, SiteAwait, SiteTimer, SiteFile, SitePoll, SiteForeign:
				state := &states[len(states)-1]
				state.transition = candidate.transitions[call]
				state.call = call
				state.statement = stmt
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
				for j, value := range stmt.Results {
					result, _ := fn.Type().Result(j).Nname.(*ir.Name)
					states[len(states)-1].body = append(
						states[len(states)-1].body,
						ir.NewAssignStmt(stmt.Pos(), result, value))
				}
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

	var edit func(ir.Node) ir.Node
	factoryCall := func(call *ir.CallExpr, statement ir.Node) (ir.Node, error) {
		edge := edgeForCall(function, call)
		child := candidates[edge.Callee]
		if child == nil {
			return nil, fmt.Errorf("%s: missing factory for %s",
				ir.PkgFuncName(fn), edge.CalleeName)
		}
		args := make(ir.Nodes, len(call.Args))
		for i, arg := range call.Args {
			args[i] = edit(arg)
		}
		targets, err := callResultTargets(statement,
			child.function.Func.Type().NumResults())
		if err != nil {
			return nil, fmt.Errorf("%s: %v", ir.PkgFuncName(fn), err)
		}
		for _, target := range targets {
			args = append(args, typecheck.NodAddr(edit(target)))
		}
		return typecheck.Call(call.Pos(), child.factory.Nname, args, false), nil
	}

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
			if parameter := candidate.parameters[name.Canonical()]; parameter != nil {
				return capture(parameter)
			}
			if result := candidate.results[name.Canonical()]; result != nil {
				return capture(result)
			}
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
	var readAssignments []*ir.AssignListStmt
	for i, state := range states {
		body := make([]ir.Node, 0, len(state.body)+4)
		for _, stmt := range state.body {
			if call, ok := stmt.(*ir.CallExpr); ok &&
				candidate.foreignCalls[call] == DirectMayBlock {
				body = append(body,
					typecheck.Call(call.Pos(),
						typecheck.LookupRuntime("coroEnterBlocking"),
						nil, false),
					edit(call),
					typecheck.Call(call.Pos(),
						typecheck.LookupRuntime("coroExitBlocking"),
						nil, false),
				)
				continue
			}
			direct := false
			ir.Visit(stmt, func(node ir.Node) {
				call, ok := node.(*ir.CallExpr)
				if ok && candidate.foreignCalls[call] == DirectNoBlock {
					direct = true
				}
			})
			if goStmt, ok := stmt.(*ir.GoDeferStmt); ok {
				if call, ok := goStmt.Call.(*ir.CallExpr); ok &&
					candidate.transitions[call] == SiteSpawn {
					child, err := factoryCall(call, nil)
					if err != nil {
						return err
					}
					body = append(body, typecheck.Call(goStmt.Pos(),
						typecheck.LookupRuntime("coroSpawn"),
						ir.Nodes{ctx, child}, false))
					continue
				}
			}
			edited := edit(stmt)
			if direct {
				body = append(body,
					typecheck.Call(stmt.Pos(),
						typecheck.LookupRuntime("coroEnterForeign"),
						nil, false),
					edited,
					typecheck.Call(stmt.Pos(),
						typecheck.LookupRuntime("coroExitForeign"),
						nil, false),
				)
			} else {
				body = append(body, edited)
			}
		}

		action := actionComplete
		if i+1 < len(states) {
			next := typedInt(pos, types.Types[types.TUINT32], int64(i+1))
			body = append(body, ir.NewAssignStmt(pos, resumePC, next))
			switch state.transition {
			case SiteYield:
				action = actionYield
			case SiteAwait:
				for _, init := range state.call.Init() {
					body = append(body, edit(init))
				}
				child, err := factoryCall(state.call, state.statement)
				if err != nil {
					return err
				}
				body = append(body, typecheck.Call(state.call.Pos(),
					typecheck.LookupRuntime("coroAwait"),
					ir.Nodes{ctx, child}, false))
				action = actionWait
			case SiteTimer:
				if len(state.call.Args) != 1 {
					return fmt.Errorf("%s: timer operation has %d arguments",
						ir.PkgFuncName(fn), len(state.call.Args))
				}
				for _, init := range state.call.Init() {
					body = append(body, edit(init))
				}
				duration := edit(state.call.Args[0])
				duration = typecheck.Conv(duration, types.Types[types.TINT64])
				body = append(body, typecheck.Call(state.call.Pos(),
					typecheck.LookupRuntime("coroSleep"),
					ir.Nodes{ctx, duration}, false))
				action = actionWait
			case SiteFile, SitePoll:
				if ordinaryReadOperation(state.call) {
					var init ir.Nodes
					oldCurFunc := ir.CurFunc
					ir.CurFunc = resume
					call := typecheck.MakeCallClosure(state.call.Pos(),
						edit(state.call), &init)
					if assignment, ok := state.statement.(*ir.AssignListStmt); ok {
						closure, ok := call.(*ir.ClosureExpr)
						if !ok {
							return fmt.Errorf("%s: read assignment has no call closure",
								ir.PkgFuncName(fn))
						}
						workerCaptures := make(map[*ir.Name]*ir.Name)
						for i, lhs := range assignment.Lhs {
							name, ok := lhs.(*ir.Name)
							if !ok {
								return fmt.Errorf("%s: read result %d is not a variable",
									ir.PkgFuncName(fn), i)
							}
							edited := edit(lhs)
							name, ok = edited.(*ir.Name)
							if !ok {
								return fmt.Errorf("%s: read result %d is not a variable",
									ir.PkgFuncName(fn), i)
							}
							if name.Class == ir.PEXTERN || ir.IsBlank(name) {
								assignment.Lhs[i] = name
								continue
							}
							if name.Curfn != resume {
								return fmt.Errorf("%s: read result %d has unsupported owner",
									ir.PkgFuncName(fn), i)
							}
							canonical := name.Canonical()
							captured := workerCaptures[canonical]
							if captured == nil {
								captured = ir.NewClosureVar(name.Pos(), closure.Func, name)
								workerCaptures[canonical] = captured
							}
							assignment.Lhs[i] = captured
						}
						assignment.Def = false
						assignment.Rhs = ir.Nodes{state.call}
						closure.Func.Body = []ir.Node{assignment}
						readAssignments = append(readAssignments, assignment)
					}
					ir.CurFunc = oldCurFunc
					for _, stmt := range init {
						body = append(body, stmt)
					}
					body = append(body, typecheck.Call(state.call.Pos(),
						typecheck.LookupRuntime("coroCallRead"),
						ir.Nodes{ctx, call}, false))
					action = actionWait
					break
				}
				if len(state.call.Args) != 4 {
					return fmt.Errorf("%s: %s operation has %d arguments",
						ir.PkgFuncName(fn), state.transition, len(state.call.Args))
				}
				for _, init := range state.call.Init() {
					body = append(body, edit(init))
				}
				args := make(ir.Nodes, 1, 5)
				args[0] = ctx
				for _, arg := range state.call.Args {
					args = append(args, edit(arg))
				}
				helper := "coroFileRead"
				if state.transition == SitePoll {
					helper = "coroSocketRead"
				}
				body = append(body, typecheck.Call(state.call.Pos(),
					typecheck.LookupRuntime(helper), args, false))
				action = actionWait
			case SiteForeign:
				if candidate.foreignCalls[state.call] != AsyncOperation {
					return fmt.Errorf("%s: state %d has non-async foreign transition",
						ir.PkgFuncName(fn), i)
				}
				if len(state.call.Args) != 5 {
					return fmt.Errorf("%s: async foreign operation has %d arguments",
						ir.PkgFuncName(fn), len(state.call.Args))
				}
				for _, init := range state.call.Init() {
					body = append(body, edit(init))
				}
				args := make(ir.Nodes, 1, 6)
				args[0] = ctx
				for _, arg := range state.call.Args {
					args = append(args, edit(arg))
				}
				body = append(body, typecheck.Call(state.call.Pos(),
					typecheck.LookupRuntime("coroAsyncDouble"), args, false))
				action = actionWait
			default:
				return fmt.Errorf("%s: state %d has no transition",
					ir.PkgFuncName(fn), i)
			}
		} else {
			for j, value := range candidate.resultValues {
				pointer := capture(candidate.resultPtrs[j])
				body = append(body, ir.NewAssignStmt(pos,
					ir.NewStarExpr(pos, pointer), capture(value)))
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
	args := make(ir.Nodes, fn.Type().NumParams())
	for i, field := range fn.Type().Params() {
		args[i], _ = field.Nname.(ir.Node)
	}
	for _, field := range fn.Type().Results() {
		result, _ := field.Nname.(ir.Node)
		args = append(args, typecheck.NodAddr(result))
	}
	newResume := typecheck.Call(pos, factory.Nname, args, false)
	run := typecheck.Call(pos, typecheck.LookupRuntime("coroRun"),
		ir.Nodes{newResume}, false)
	fn.Body = []ir.Node{run, ir.NewReturnStmt(pos, nil)}
	typecheck.Stmts(fn.Body)
	ir.CurFunc = oldCurFunc
	for _, assignment := range readAssignments {
		assignment.SetOp(ir.OAS2FUNC)
	}

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

func callResultTargets(stmt ir.Node, count int) (ir.Nodes, error) {
	if count == 0 {
		return nil, nil
	}
	switch stmt := stmt.(type) {
	case *ir.AssignStmt:
		if count == 1 {
			return ir.Nodes{stmt.X}, nil
		}
	case *ir.AssignListStmt:
		if len(stmt.Lhs) == count {
			for _, target := range stmt.Lhs {
				if ir.IsBlank(target) {
					return nil, fmt.Errorf("discarded coroutine result")
				}
			}
			return stmt.Lhs, nil
		}
	}
	return nil, fmt.Errorf("coroutine call has %d results without matching assignment",
		count)
}

func transitionCall(stmt ir.Node, transitions map[*ir.CallExpr]SiteKind) *ir.CallExpr {
	switch stmt := stmt.(type) {
	case *ir.CallExpr:
		if transitions[stmt] != SiteInvalid {
			return stmt
		}
	case *ir.AssignStmt:
		if call, ok := stmt.Y.(*ir.CallExpr); ok &&
			transitions[call] != SiteInvalid {
			return call
		}
	case *ir.AssignListStmt:
		if len(stmt.Rhs) == 1 {
			if call, ok := stmt.Rhs[0].(*ir.CallExpr); ok &&
				transitions[call] != SiteInvalid {
				return call
			}
		}
	}
	var found *ir.CallExpr
	ir.Visit(stmt, func(node ir.Node) {
		if found != nil {
			return
		}
		if call, ok := node.(*ir.CallExpr); ok &&
			transitions[call] != SiteInvalid {
			found = call
		}
	})
	return found
}

func ordinaryReadOperation(call *ir.CallExpr) bool {
	name := ir.StaticCalleeName(ir.StaticValue(call.Fun))
	switch symbolName(name) {
	case "os.(*File).Read", "net.(*TCPConn).Read", "net.(*conn).Read":
		return true
	}
	return false
}

func typedInt(pos src.XPos, typ *types.Type, value int64) ir.Node {
	return ir.NewBasicLit(pos, typ, constant.MakeInt64(value))
}
