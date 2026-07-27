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
	directCalls  map[*ir.CallExpr]*ir.Func
	dependencies map[*ir.Func]bool
	defers       []*lowerDefer
	parameters   map[*ir.Name]*ir.Name
	results      map[*ir.Name]*ir.Name
	resultValues []*ir.Name
	resultPtrs   []*ir.Name
	factory      *ir.Func
}

type lowerDefer struct {
	statement *ir.GoDeferStmt
	call      *ir.CallExpr
	armed     *ir.Name
}

type lowerState struct {
	body       []ir.Node
	transition SiteKind
	call       *ir.CallExpr
	statement  ir.Node
	next       int
	condition  ir.Node
	thenState  int
	elseState  int
	complete   bool
}

// Lower rewrites supported coroutine primaries into explicit state machines.
// The lowering accepts closed calls, if statements, simple for loops, normal
// returns, fixed direct defers, and the supported operation sites. Unsupported
// functions remain unchanged.
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
		candidate, err := newLowerCandidate(plan, function)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics,
				fmt.Sprintf("%s: %v", ir.PkgFuncName(function.Func), err))
			continue
		}
		candidates[function.Func] = candidate
	}

	factories := make(map[*ir.Func]*ir.Func)

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
				if !hasResumeFactory(plan, candidates, factories,
					dependency) {
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
		factories[function.Func] = candidate.factory
		candidate.parameters = make(map[*ir.Name]*ir.Name)
		candidate.results = make(map[*ir.Name]*ir.Name)
		inputs := candidate.function.Func.Type().RecvParams()
		for i, field := range inputs {
			source, _ := field.Nname.(*ir.Name)
			target, _ := candidate.factory.Type().Param(i).Nname.(*ir.Name)
			if source != nil && target != nil {
				candidate.parameters[source.Canonical()] = target
			}
		}
		paramCount := len(inputs)
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
		var err error
		if canLowerRunToCompletion(candidate) {
			err = lowerRunToCompletion(candidate)
		} else {
			err = lowerFunction(candidate, factories)
		}
		if err != nil {
			return result, err
		}
		if resumeFactorySupported(function.Func) {
			function.Factory = FactoryABI1
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

func newLowerCandidate(plan *Plan, function *Function) (*lowerCandidate, error) {
	fn := function.Func
	if fn == nil || fn.OClosure != nil {
		return nil, fmt.Errorf("not a top-level function")
	}
	sig := fn.Type()
	if sig.HasShape() {
		return nil, fmt.Errorf("generic shape")
	}
	if len(function.Sites) == 0 {
		return nil, fmt.Errorf("no coroutine sites")
	}

	statementCalls := make(map[*ir.CallExpr]bool)
	awaitStatements := make(map[*ir.CallExpr]ir.Node)
	goCalls := make(map[*ir.CallExpr]bool)
	var defers []*lowerDefer
	var collectStatements func(ir.Nodes, bool) error
	collectStatements = func(list ir.Nodes, inLoop bool) error {
		for _, stmt := range list {
			if init := stmt.Init(); len(init) != 0 {
				if err := collectStatements(init, inLoop); err != nil {
					return err
				}
			}
			switch stmt := stmt.(type) {
			case *ir.CallExpr:
				statementCalls[stmt] = true
				awaitStatements[stmt] = stmt
			case *ir.AssignStmt:
				if call, ok := stmt.Y.(*ir.CallExpr); ok {
					statementCalls[call] = true
					awaitStatements[call] = stmt
				}
			case *ir.AssignListStmt:
				if len(stmt.Rhs) == 1 {
					if call, ok := stmt.Rhs[0].(*ir.CallExpr); ok {
						statementCalls[call] = true
						awaitStatements[call] = stmt
					}
				}
				// Multi-result method calls may have already been normalized
				// into an assignment form whose call is below the RHS root.
				// Keep the containing assignment for either typed worker
				// adaptation or result-projection validation.
				ir.Visit(stmt, func(node ir.Node) {
					call, ok := node.(*ir.CallExpr)
					if !ok {
						return
					}
					awaitStatements[call] = stmt
					if ordinaryReadOperation(call) {
						statementCalls[call] = true
					}
				})
			case *ir.GoDeferStmt:
				call, ok := stmt.Call.(*ir.CallExpr)
				if !ok {
					return fmt.Errorf("non-call go or defer statement")
				}
				if stmt.Op() == ir.ODEFER {
					if inLoop {
						return fmt.Errorf("defer in loop")
					}
					if call.Op() != ir.OCALLFUNC || call.Fun == nil ||
						call.Fun.Type() == nil || len(call.Args) != 0 ||
						call.Fun.Type().NumParams() != 0 ||
						call.Fun.Type().NumResults() != 0 {
						return fmt.Errorf("non-normalized defer call")
					}
					defers = append(defers, &lowerDefer{
						statement: stmt,
						call:      call,
					})
					continue
				}
				goCalls[call] = true
			case *ir.BlockStmt:
				if err := collectStatements(stmt.List, inLoop); err != nil {
					return err
				}
			case *ir.IfStmt:
				if err := collectStatements(stmt.Body, inLoop); err != nil {
					return err
				}
				if err := collectStatements(stmt.Else, inLoop); err != nil {
					return err
				}
			case *ir.ForStmt:
				if stmt.Label != nil {
					return fmt.Errorf("labeled for loop")
				}
				if stmt.Post != nil {
					if err := collectStatements(ir.Nodes{stmt.Post}, true); err != nil {
						return err
					}
				}
				if err := collectStatements(stmt.Body, true); err != nil {
					return err
				}
			case *ir.ReturnStmt:
				if len(stmt.Results) != 0 && len(stmt.Results) != sig.NumResults() {
					return fmt.Errorf("return has %d results, want %d",
						len(stmt.Results), sig.NumResults())
				}
			default:
				hasDefer := false
				ir.Visit(stmt, func(node ir.Node) {
					if deferStmt, ok := node.(*ir.GoDeferStmt); ok &&
						deferStmt.Op() == ir.ODEFER {
						hasDefer = true
					}
				})
				if hasDefer {
					return fmt.Errorf("defer in unsupported control flow %v",
						stmt.Op())
				}
				switch stmt.Op() {
				case ir.OBREAK, ir.OCONTINUE, ir.OFALL, ir.OGOTO,
					ir.OLABEL, ir.OSELECT:
					return fmt.Errorf("control flow %v", stmt.Op())
				}
			}
		}
		return nil
	}
	if err := collectStatements(fn.Body, false); err != nil {
		return nil, err
	}

	edges := make(map[*ir.CallExpr]Edge)
	for _, edge := range function.Edges {
		edges[edge.Node] = edge
		if edge.Kind != DirectCall || edge.Recipe.Kind != SiteInvalid {
			continue
		}
		summary, known := plan.edgeSummary(edge)
		hasAwait := slices.ContainsFunc(function.Sites, func(site Site) bool {
			return site.Kind == SiteAwait && site.Node == edge.Node
		})
		if known && summary.Primary() == CoroPrimary && !hasAwait {
			return nil, fmt.Errorf(
				"direct call to %s requires coroutine factory entry",
				edge.CalleeName)
		}
	}
	candidate := &lowerCandidate{
		function:     function,
		transitions:  make(map[*ir.CallExpr]SiteKind),
		foreignCalls: make(map[*ir.CallExpr]ForeignCallClass),
		directCalls:  make(map[*ir.CallExpr]*ir.Func),
		dependencies: make(map[*ir.Func]bool),
		defers:       defers,
	}
	for _, site := range function.Sites {
		call, ok := site.Node.(*ir.CallExpr)
		if !ok {
			return nil, fmt.Errorf("non-call site %d", site.ID)
		}
		if slices.ContainsFunc(defers, func(deferred *lowerDefer) bool {
			return deferred.call == call
		}) {
			return nil, fmt.Errorf("suspending defer")
		}
		switch site.Kind {
		case SiteYield, SiteTimer, SiteFile, SitePoll:
			if !statementCalls[call] {
				return nil, fmt.Errorf("nested %s site %d", site.Kind, site.ID)
			}
		case SiteAwait:
			statement := awaitStatements[call]
			if statement == nil {
				return nil, fmt.Errorf("nested await site %d", site.ID)
			}
			edge := edges[call]
			if edge.Callee == nil {
				return nil, fmt.Errorf("dynamic await site %d", site.ID)
			}
			targets, err := callResultTargets(call, statement,
				edge.Callee.Type().NumResults())
			if err != nil {
				return nil, fmt.Errorf("await site %d: %v", site.ID, err)
			}
			for i, target := range targets {
				if target == nil {
					continue
				}
				if _, ok := target.(*ir.Name); !ok {
					return nil, fmt.Errorf(
						"await site %d result %d is not a variable",
						site.ID, i)
				}
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
			if edge.Callee.Type().NumResults() != 0 {
				return nil, fmt.Errorf("spawn target returns values")
			}
			candidate.dependencies[edge.Callee] = true
		case SiteForeign:
			if site.Foreign != DirectNoBlock && site.Foreign != DirectMayBlock &&
				site.Foreign != AsyncOperation {
				return nil, fmt.Errorf("unsupported foreign site %s", site.Foreign)
			}
			if !statementCalls[call] {
				return nil, fmt.Errorf("nested %s foreign site %d",
					site.Foreign, site.ID)
			}
			candidate.foreignCalls[call] = site.Foreign
			edge := edges[call]
			if edge.Recipe.Direct != "" {
				if edge.Direct == nil {
					return nil, fmt.Errorf("missing direct entry %s",
						edge.Recipe.Direct)
				}
				candidate.directCalls[call] = edge.Direct
			}
			if site.Foreign != AsyncOperation {
				continue
			}
		default:
			return nil, fmt.Errorf("unsupported site %s", site.Kind)
		}
		candidate.transitions[call] = site.Kind
	}
	for _, deferred := range defers {
		edge, ok := edges[deferred.call]
		if !ok || edge.Kind != DeferCall || edge.Callee == nil {
			return nil, fmt.Errorf("defer has no static call plan")
		}
		if edge.Unknown {
			return nil, fmt.Errorf("defer target has unknown effects")
		}
		summary, _ := plan.edgeSummary(edge)
		if summary.Effect != NoSuspend {
			return nil, fmt.Errorf("suspending defer")
		}
		if summary.Exec != 0 {
			return nil, fmt.Errorf("defer target has execution constraints %s",
				summary.Exec)
		}
		if edge.Callee.OClosure != nil &&
			(!edge.Callee.Wrapper() || edge.Callee.WrappedFunc == nil) {
			return nil, fmt.Errorf("defer target is not a fixed direct call")
		}
	}
	return candidate, nil
}

func stacklessResumeType() *types.Type {
	ctx := types.NewField(src.NoXPos, types.LocalPkg.Lookup(".coroctx"),
		types.Types[types.TUNSAFEPTR])
	action := types.NewField(src.NoXPos, nil, types.Types[types.TUINT8])
	return types.NewSignature(nil, []*types.Field{ctx}, []*types.Field{action})
}

func resumeFactorySupported(fn *ir.Func) bool {
	if fn == nil || fn.OClosure != nil || fn.Type() == nil {
		return false
	}
	return !fn.Type().HasShape()
}

func resumeFactoryType(fn *ir.Func) *types.Type {
	pos := fn.Pos()
	resumeType := stacklessResumeType()
	result := types.NewField(pos, nil, resumeType)
	inputs := fn.Type().RecvParams()
	params := make([]*types.Field, len(inputs))
	for i, field := range inputs {
		// Type checking has normalized a variadic argument to its slice.
		// Deliberately leave IsDDD unset on the factory parameter.
		params[i] = types.NewField(field.Pos, field.Sym, field.Type)
	}
	for i, field := range fn.Type().Results() {
		sym := fn.Sym().Pkg.LookupNum(".cororesult", i)
		params = append(params,
			types.NewField(field.Pos, sym, types.NewPtr(field.Type)))
	}
	return types.NewSignature(nil, params, []*types.Field{result})
}

func resumeFactorySymbol(fn *ir.Func) *types.Sym {
	return fn.Sym().Pkg.Lookup(fn.Sym().Name + ".coro")
}

func newResumeFactory(fn *ir.Func) *ir.Func {
	pos := fn.Pos()
	typ := resumeFactoryType(fn)
	sym := resumeFactorySymbol(fn)
	factory := ir.NewFunc(pos, pos, sym, typ)
	factory.SetDupok(fn.Dupok())
	factory.Nname.Defn = factory
	factory.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, factory)
	return factory
}

func importedResumeFactory(fn *ir.Func) (*ir.Func, bool) {
	summary, ok := Summary(fn)
	if !ok || summary.Factory != FactoryABI1 || !resumeFactorySupported(fn) {
		return nil, false
	}
	pos := fn.Pos()
	return ir.NewFunc(pos, pos, resumeFactorySymbol(fn),
		resumeFactoryType(fn)), true
}

func hasResumeFactory(plan *Plan, candidates map[*ir.Func]*lowerCandidate,
	factories map[*ir.Func]*ir.Func, fn *ir.Func) bool {
	if _, local := plan.Functions[fn]; local {
		return candidates[fn] != nil
	}
	if factories[fn] != nil {
		return true
	}
	factory, ok := importedResumeFactory(fn)
	if ok {
		factories[fn] = factory
	}
	return ok
}

// canLowerRunToCompletion reports whether candidate has to enter a native ABI
// but never yields logical control. Such functions can retain their structured
// control flow instead of paying for a state-machine dispatch around every
// loop iteration.
func canLowerRunToCompletion(candidate *lowerCandidate) bool {
	if candidate.function.Effect != NoSuspend || len(candidate.defers) != 0 ||
		len(candidate.foreignCalls) == 0 {
		return false
	}
	for _, site := range candidate.function.Sites {
		if site.Kind != SiteForeign ||
			(site.Foreign != DirectNoBlock && site.Foreign != DirectMayBlock) {
			return false
		}
	}

	// A for post statement cannot be expanded into the enter/call/exit
	// sequence without changing continue semantics. Keep using the general
	// lowering until the IR has a dedicated representation for that case.
	supported := true
	ir.Visit(candidate.function.Func, func(node ir.Node) {
		if !supported {
			return
		}
		loop, ok := node.(*ir.ForStmt)
		if !ok || loop.Post == nil {
			return
		}
		ir.Visit(loop.Post, func(node ir.Node) {
			if call, ok := node.(*ir.CallExpr); ok &&
				candidate.foreignCalls[call] != NotForeign {
				supported = false
			}
		})
	})
	return supported
}

// lowerRunToCompletion builds a single-invocation resume closure. Foreign
// calls are instrumented in place, so ordinary blocks, branches, and loops
// retain their source control flow.
func lowerRunToCompletion(candidate *lowerCandidate) error {
	fn := candidate.function.Func
	factory := candidate.factory
	resumeType := factory.Type().Result(0).Type
	pos := fn.Pos()

	var declarations []ir.Node
	declared := make(map[*ir.Name]bool)
	addDeclaration := func(decl *ir.Decl) {
		name := decl.X.Canonical()
		if !declared[name] {
			declared[name] = true
			declarations = append(declarations, decl)
		}
	}

	// Source locals become factory locals. Returning the closure then places
	// the captured slots in a typed, GC-scanned heap object.
	fnDcl := make([]*ir.Name, 0, len(fn.Dcl))
	for _, name := range fn.Dcl {
		switch name.Class {
		case ir.PAUTO, ir.PAUTOHEAP:
			name.Curfn = factory
			factory.Dcl = append(factory.Dcl, name)
			addDeclaration(ir.NewDecl(name.Pos(), ir.ODCL, name))
		default:
			fnDcl = append(fnDcl, name)
		}
	}
	fn.Dcl = fnDcl

	for _, result := range candidate.resultValues {
		addDeclaration(ir.NewDecl(pos, ir.ODCL, result))
	}

	resume := ir.NewClosureFunc(pos, pos, ir.OCLOSURE, resumeType, factory,
		typecheck.Target, 0)
	// A native executor stack is fixed and cannot satisfy morestack. Calls
	// made by the resume function retain their own stack checks, so exhausting
	// the executor budget fails in the runtime instead of copying the stack.
	resume.Pragma |= ir.Nosplit
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
		case *ir.CallExpr:
			if direct := candidate.directCalls[node]; direct != nil {
				node.Fun = direct.Nname
			}
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

	complete := func(at src.XPos) ir.Nodes {
		body := make(ir.Nodes, 0, len(candidate.resultValues)+1)
		for i, value := range candidate.resultValues {
			pointer := capture(candidate.resultPtrs[i])
			body = append(body, ir.NewAssignStmt(at,
				ir.NewStarExpr(at, pointer), capture(value)))
		}
		return append(body, ir.NewReturnStmt(at, []ir.Node{
			typedInt(at, types.Types[types.TUINT8], int64(actionComplete)),
		}))
	}

	var rewriteList func(ir.Nodes) (ir.Nodes, error)
	rewriteList = func(list ir.Nodes) (ir.Nodes, error) {
		body := make(ir.Nodes, 0, len(list))
		for _, stmt := range list {
			switch stmt := stmt.(type) {
			case *ir.Decl:
				addDeclaration(stmt)
				continue

			case *ir.ReturnStmt:
				init, err := rewriteList(ir.TakeInit(stmt))
				if err != nil {
					return nil, err
				}
				body = append(body, init...)
				if len(stmt.Results) != 0 {
					lhs := make(ir.Nodes, len(candidate.resultValues))
					rhs := make(ir.Nodes, len(stmt.Results))
					for i, result := range candidate.resultValues {
						lhs[i] = capture(result)
						rhs[i] = edit(stmt.Results[i])
					}
					if len(lhs) == 1 {
						body = append(body, ir.NewAssignStmt(stmt.Pos(),
							lhs[0], rhs[0]))
					} else {
						body = append(body, ir.NewAssignListStmt(stmt.Pos(),
							ir.OAS2, lhs, rhs))
					}
				}
				body = append(body, complete(stmt.Pos())...)
				continue

			case *ir.BlockStmt:
				init, err := rewriteList(ir.TakeInit(stmt))
				if err != nil {
					return nil, err
				}
				stmt.SetInit(init)
				stmt.List, err = rewriteList(stmt.List)
				if err != nil {
					return nil, err
				}
				stmt.SetTypecheck(0)
				body = append(body, stmt)
				continue

			case *ir.IfStmt:
				init, err := rewriteList(ir.TakeInit(stmt))
				if err != nil {
					return nil, err
				}
				stmt.SetInit(init)
				stmt.Cond = edit(stmt.Cond)
				stmt.Body, err = rewriteList(stmt.Body)
				if err != nil {
					return nil, err
				}
				stmt.Else, err = rewriteList(stmt.Else)
				if err != nil {
					return nil, err
				}
				stmt.SetTypecheck(0)
				body = append(body, stmt)
				continue

			case *ir.ForStmt:
				init, err := rewriteList(ir.TakeInit(stmt))
				if err != nil {
					return nil, err
				}
				stmt.SetInit(init)
				if stmt.Cond != nil {
					stmt.Cond = edit(stmt.Cond)
				}
				if stmt.Post != nil {
					stmt.Post = edit(stmt.Post)
				}
				stmt.Body, err = rewriteList(stmt.Body)
				if err != nil {
					return nil, err
				}
				stmt.SetTypecheck(0)
				body = append(body, stmt)
				continue
			}

			init, err := rewriteList(ir.TakeInit(stmt))
			if err != nil {
				return nil, err
			}
			body = append(body, init...)

			foreign := NotForeign
			var foreignCall *ir.CallExpr
			multipleForeignCalls := false
			ir.Visit(stmt, func(node ir.Node) {
				call, ok := node.(*ir.CallExpr)
				if !ok || candidate.foreignCalls[call] == NotForeign {
					return
				}
				if foreignCall != nil {
					multipleForeignCalls = true
					return
				}
				foreignCall = call
				foreign = candidate.foreignCalls[call]
			})
			if multipleForeignCalls {
				return nil, fmt.Errorf("%s: multiple foreign calls in one statement",
					ir.PkgFuncName(fn))
			}
			if foreignCall == nil {
				body = append(body, edit(stmt))
				continue
			}

			callInit, err := rewriteList(ir.TakeInit(foreignCall))
			if err != nil {
				return nil, err
			}
			body = append(body, callInit...)
			for i, arg := range foreignCall.Args {
				arg = edit(arg)
				if arg.Type() == nil {
					return nil, fmt.Errorf("%s: foreign argument %d has no type",
						ir.PkgFuncName(fn), i)
				}
				temp := typecheck.TempAt(arg.Pos(), resume, arg.Type())
				body = append(body, ir.NewAssignStmt(arg.Pos(), temp, arg))
				foreignCall.Args[i] = temp
			}

			edited := edit(stmt)
			switch foreign {
			case DirectNoBlock:
				body = append(body,
					typecheck.Call(stmt.Pos(),
						typecheck.LookupRuntime("coroEnterForeign"), nil, false),
					edited,
					typecheck.Call(stmt.Pos(),
						typecheck.LookupRuntime("coroExitForeign"), nil, false),
				)
			case DirectMayBlock:
				body = append(body,
					typecheck.Call(stmt.Pos(),
						typecheck.LookupRuntime("coroEnterBlocking"),
						ir.Nodes{ctx}, false),
					edited,
					typecheck.Call(stmt.Pos(),
						typecheck.LookupRuntime("coroExitBlocking"), nil, false),
				)
			default:
				return nil, fmt.Errorf("%s: unsupported foreign call %s",
					ir.PkgFuncName(fn), foreign)
			}
		}
		return body, nil
	}

	body, err := rewriteList(fn.Body)
	if err != nil {
		return err
	}
	resume.Body = append(body, complete(pos)...)
	finishLowering(candidate, resume, declarations, nil)
	return nil
}

func lowerFunction(candidate *lowerCandidate, factories map[*ir.Func]*ir.Func) error {
	function := candidate.function
	fn := function.Func
	factory := candidate.factory
	resumeType := factory.Type().Result(0).Type
	pos := fn.Pos()

	var declarations []ir.Node
	declared := make(map[*ir.Name]bool)
	addDeclaration := func(decl *ir.Decl) {
		name := decl.X.Canonical()
		if !declared[name] {
			declared[name] = true
			declarations = append(declarations, decl)
		}
	}

	// Source locals become factory locals. Returning the closure then places
	// the captured slots in a typed, GC-scanned heap object.
	fnDcl := make([]*ir.Name, 0, len(fn.Dcl))
	for _, name := range fn.Dcl {
		switch name.Class {
		case ir.PAUTO, ir.PAUTOHEAP:
			name.Curfn = factory
			factory.Dcl = append(factory.Dcl, name)
			addDeclaration(ir.NewDecl(name.Pos(), ir.ODCL, name))
		default:
			fnDcl = append(fnDcl, name)
		}
	}
	fn.Dcl = fnDcl

	for _, result := range candidate.resultValues {
		addDeclaration(ir.NewDecl(pos, ir.ODCL, result))
	}
	deferByStatement := make(map[*ir.GoDeferStmt]*lowerDefer,
		len(candidate.defers))
	for _, deferred := range candidate.defers {
		deferred.armed = typecheck.TempAt(deferred.statement.Pos(), factory,
			types.Types[types.TBOOL])
		addDeclaration(ir.NewDecl(deferred.statement.Pos(), ir.ODCL,
			deferred.armed))
		deferByStatement[deferred.statement] = deferred
	}

	pc := typecheck.TempAt(pos, factory, types.Types[types.TUINT32])
	addDeclaration(ir.NewDecl(pos, ir.ODCL, pc))

	resume := ir.NewClosureFunc(pos, pos, ir.OCLOSURE, resumeType, factory,
		typecheck.Target, 0)
	// A native executor stack is fixed and cannot satisfy morestack. Calls
	// made by the resume function retain their own stack checks, so exhausting
	// the executor budget fails in the runtime instead of copying the stack.
	resume.Pragma |= ir.Nosplit
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

	states := []lowerState{{
		next:      -1,
		thenState: -1,
		elseState: -1,
		complete:  true,
	}}
	addState := func(state lowerState) int {
		stateIndex := len(states)
		states = append(states, state)
		return stateIndex
	}
	newBodyState := func(body ir.Nodes, next int) int {
		filtered := make(ir.Nodes, 0, len(body))
		for _, stmt := range body {
			if decl, ok := stmt.(*ir.Decl); ok {
				addDeclaration(decl)
				continue
			}
			filtered = append(filtered, stmt)
		}
		if len(filtered) == 0 {
			return next
		}
		return addState(lowerState{
			body:      filtered,
			next:      next,
			thenState: -1,
			elseState: -1,
		})
	}
	requiresControlLowering := func(node ir.Node) bool {
		required := false
		ir.Visit(node, func(node ir.Node) {
			if required {
				return
			}
			if stmt, ok := node.(*ir.GoDeferStmt); ok &&
				stmt.Op() == ir.ODEFER {
				required = true
				return
			}
			call, ok := node.(*ir.CallExpr)
			if !ok {
				return
			}
			if candidate.transitions[call] != SiteInvalid ||
				candidate.foreignCalls[call] != NotForeign {
				required = true
			}
		})
		return required
	}
	isBoundary := func(stmt ir.Node) bool {
		switch stmt := stmt.(type) {
		case *ir.ReturnStmt:
			return true
		case *ir.BlockStmt, *ir.IfStmt, *ir.ForStmt:
			return requiresControlLowering(stmt)
		}
		if call := transitionCall(stmt, candidate.transitions); call != nil {
			switch candidate.transitions[call] {
			case SiteYield, SiteAwait, SiteTimer, SiteFile, SitePoll, SiteForeign:
				return true
			}
		}
		return false
	}

	var lowerStatements func(ir.Nodes, int) int
	var lowerStatement func(ir.Node, int) int
	lowerStatements = func(list ir.Nodes, next int) int {
		end := len(list)
		for i := len(list) - 1; i >= 0; i-- {
			if !isBoundary(list[i]) {
				continue
			}
			next = newBodyState(list[i+1:end], next)
			next = lowerStatement(list[i], next)
			end = i
		}
		return newBodyState(list[:end], next)
	}
	lowerStatement = func(stmt ir.Node, next int) int {
		switch stmt := stmt.(type) {
		case *ir.ReturnStmt:
			body := make(ir.Nodes, 0, len(stmt.Results))
			for i, value := range stmt.Results {
				result, _ := fn.Type().Result(i).Nname.(*ir.Name)
				body = append(body, ir.NewAssignStmt(stmt.Pos(), result, value))
			}
			entry := newBodyState(body, 0)
			return lowerStatements(stmt.Init(), entry)

		case *ir.BlockStmt:
			entry := lowerStatements(stmt.List, next)
			return lowerStatements(stmt.Init(), entry)

		case *ir.IfStmt:
			thenState := lowerStatements(stmt.Body, next)
			elseState := lowerStatements(stmt.Else, next)
			branch := addState(lowerState{
				condition: stmt.Cond,
				next:      -1,
				thenState: thenState,
				elseState: elseState,
			})
			return lowerStatements(stmt.Init(), branch)

		case *ir.ForStmt:
			conditionState := addState(lowerState{
				condition: stmt.Cond,
				next:      -1,
				thenState: -1,
				elseState: next,
			})
			postState := conditionState
			if stmt.Post != nil {
				postState = lowerStatements(ir.Nodes{stmt.Post}, conditionState)
			}
			bodyState := lowerStatements(stmt.Body, postState)
			states[conditionState].thenState = bodyState
			return lowerStatements(stmt.Init(), conditionState)
		}
		if init := ir.TakeInit(stmt); len(init) != 0 {
			continuation := next
			if transitionCall(stmt, candidate.transitions) != nil {
				continuation = lowerStatement(stmt, next)
			} else {
				continuation = newBodyState(ir.Nodes{stmt}, next)
			}
			return lowerStatements(init, continuation)
		}
		if call := transitionCall(stmt, candidate.transitions); call != nil {
			if init := takeCallInit(stmt, call); len(init) != 0 {
				continuation := newBodyState(ir.Nodes{stmt}, next)
				return lowerStatements(init, continuation)
			}
			switch candidate.transitions[call] {
			case SiteYield, SiteAwait, SiteTimer, SiteFile, SitePoll, SiteForeign:
				return addState(lowerState{
					transition: candidate.transitions[call],
					call:       call,
					statement:  stmt,
					next:       next,
					thenState:  -1,
					elseState:  -1,
				})
			}
		}
		panic(fmt.Sprintf("unexpected coroutine lowering boundary %s", stmt.Op()))
	}
	entryState := lowerStatements(fn.Body, 0)
	declarations = append(declarations, ir.NewAssignStmt(pos, pc,
		typedInt(pos, types.Types[types.TUINT32], int64(entryState))))

	var edit func(ir.Node) ir.Node
	factoryCall := func(call *ir.CallExpr, statement ir.Node) (ir.Node, error) {
		edge := edgeForCall(function, call)
		child := factories[edge.Callee]
		if child == nil {
			return nil, fmt.Errorf("%s: missing factory for %s",
				ir.PkgFuncName(fn), edge.CalleeName)
		}
		args := make(ir.Nodes, len(call.Args))
		for i, arg := range call.Args {
			args[i] = edit(arg)
		}
		targets, err := callResultTargets(call, statement,
			edge.Callee.Type().NumResults())
		if err != nil {
			return nil, fmt.Errorf("%s: %v", ir.PkgFuncName(fn), err)
		}
		for i, target := range targets {
			if target == nil {
				temp := typecheck.TempAt(call.Pos(), factory,
					edge.Callee.Type().Result(i).Type)
				addDeclaration(ir.NewDecl(call.Pos(), ir.ODCL, temp))
				target = temp
			}
			args = append(args, typecheck.NodAddr(edit(target)))
		}
		return typecheck.Call(call.Pos(), child.Nname, args, false), nil
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
		case *ir.CallExpr:
			if direct := candidate.directCalls[node]; direct != nil {
				node.Fun = direct.Nname
			}
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

	prepareForeignCall := func(stmt ir.Node, call *ir.CallExpr) (ir.Nodes, error) {
		var before ir.Nodes
		for _, init := range ir.TakeInit(stmt) {
			before = append(before, edit(init))
		}
		for _, init := range ir.TakeInit(call) {
			before = append(before, edit(init))
		}
		for i, arg := range call.Args {
			arg = edit(arg)
			if arg.Type() == nil {
				return nil, fmt.Errorf("%s: foreign argument %d has no type",
					ir.PkgFuncName(fn), i)
			}
			temp := typecheck.TempAt(arg.Pos(), resume, arg.Type())
			before = append(before, ir.NewAssignStmt(arg.Pos(), temp, arg))
			call.Args[i] = temp
		}
		return before, nil
	}

	// Go/defer normalization has already moved each deferred call into a
	// parameterless wrapper and placed its argument evaluation in the defer
	// statement's init list. Reparent those wrappers to the generated resume
	// function and route their captures through the typed coroutine frame.
	for _, deferred := range candidate.defers {
		closure, ok := deferred.call.Fun.(*ir.ClosureExpr)
		if !ok {
			continue
		}
		closure.Func.ClosureParent = resume
		for _, variable := range closure.Func.ClosureVars {
			outer, ok := edit(variable.Outer).(*ir.Name)
			if !ok {
				return fmt.Errorf("%s: defer capture is not a variable",
					ir.PkgFuncName(fn))
			}
			variable.Outer = outer
		}
	}

	cases := make([]*ir.CaseClause, len(states))
	var readAssignments []*ir.AssignListStmt
	for i, state := range states {
		body := make([]ir.Node, 0, len(state.body)+4)
		for _, stmt := range state.body {
			if deferStmt, ok := stmt.(*ir.GoDeferStmt); ok &&
				deferStmt.Op() == ir.ODEFER {
				deferred := deferByStatement[deferStmt]
				if deferred == nil {
					return fmt.Errorf("%s: missing defer plan",
						ir.PkgFuncName(fn))
				}
				for _, init := range deferStmt.Init() {
					body = append(body, edit(init))
				}
				body = append(body, ir.NewAssignStmt(deferStmt.Pos(),
					capture(deferred.armed),
					ir.NewBasicLit(deferStmt.Pos(), types.Types[types.TBOOL],
						constant.MakeBool(true))))
				continue
			}
			foreign := NotForeign
			var foreignCall *ir.CallExpr
			multipleForeignCalls := false
			ir.Visit(stmt, func(node ir.Node) {
				call, ok := node.(*ir.CallExpr)
				if !ok || candidate.foreignCalls[call] == NotForeign {
					return
				}
				if foreignCall != nil {
					multipleForeignCalls = true
					return
				}
				foreignCall = call
				foreign = candidate.foreignCalls[call]
			})
			if multipleForeignCalls {
				return fmt.Errorf("%s: multiple foreign calls in one statement",
					ir.PkgFuncName(fn))
			}
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
			if foreignCall != nil {
				before, err := prepareForeignCall(stmt, foreignCall)
				if err != nil {
					return err
				}
				body = append(body, before...)
			}
			edited := edit(stmt)
			switch foreign {
			case NotForeign:
				body = append(body, edited)
			case DirectNoBlock:
				body = append(body,
					typecheck.Call(stmt.Pos(),
						typecheck.LookupRuntime("coroEnterForeign"),
						nil, false),
					edited,
					typecheck.Call(stmt.Pos(),
						typecheck.LookupRuntime("coroExitForeign"),
						nil, false),
				)
			case DirectMayBlock:
				body = append(body,
					typecheck.Call(stmt.Pos(),
						typecheck.LookupRuntime("coroEnterBlocking"),
						ir.Nodes{ctx}, false),
					edited,
					typecheck.Call(stmt.Pos(),
						typecheck.LookupRuntime("coroExitBlocking"),
						nil, false),
				)
			default:
				return fmt.Errorf("%s: unsupported foreign call %s",
					ir.PkgFuncName(fn), foreign)
			}
		}

		if state.complete {
			for j := len(candidate.defers) - 1; j >= 0; j-- {
				deferred := candidate.defers[j]
				armed := capture(deferred.armed)
				clear := ir.NewAssignStmt(deferred.statement.Pos(), armed,
					ir.NewBasicLit(deferred.statement.Pos(),
						types.Types[types.TBOOL], constant.MakeBool(false)))
				call := typecheck.Call(deferred.call.Pos(),
					edit(deferred.call.Fun), nil, false)
				body = append(body, ir.NewIfStmt(deferred.statement.Pos(),
					armed, ir.Nodes{clear, call}, nil))
			}
			for j, value := range candidate.resultValues {
				pointer := capture(candidate.resultPtrs[j])
				body = append(body, ir.NewAssignStmt(pos,
					ir.NewStarExpr(pos, pointer), capture(value)))
			}
			body = append(body, ir.NewReturnStmt(pos, []ir.Node{
				typedInt(pos, types.Types[types.TUINT8], int64(actionComplete)),
			}))
		} else if state.transition != SiteInvalid {
			if state.next < 0 {
				return fmt.Errorf("%s: state %d transition has no continuation",
					ir.PkgFuncName(fn), i)
			}
			next := typedInt(pos, types.Types[types.TUINT32], int64(state.next))
			body = append(body, ir.NewAssignStmt(pos, resumePC, next))
			action := actionInvalid
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
			body = append(body, ir.NewReturnStmt(pos, []ir.Node{
				typedInt(pos, types.Types[types.TUINT8], int64(action)),
			}))
		} else if state.thenState >= 0 {
			thenPC := ir.NewAssignStmt(pos, resumePC,
				typedInt(pos, types.Types[types.TUINT32], int64(state.thenState)))
			if state.condition == nil {
				body = append(body, thenPC)
			} else {
				if state.elseState < 0 {
					return fmt.Errorf("%s: state %d branch has no false continuation",
						ir.PkgFuncName(fn), i)
				}
				elsePC := ir.NewAssignStmt(pos, resumePC,
					typedInt(pos, types.Types[types.TUINT32], int64(state.elseState)))
				body = append(body, ir.NewIfStmt(pos, edit(state.condition),
					ir.Nodes{thenPC}, ir.Nodes{elsePC}))
			}
			body = append(body, ir.NewBranchStmt(pos, ir.OCONTINUE, nil))
		} else if state.next >= 0 {
			body = append(body,
				ir.NewAssignStmt(pos, resumePC,
					typedInt(pos, types.Types[types.TUINT32], int64(state.next))),
				ir.NewBranchStmt(pos, ir.OCONTINUE, nil),
			)
		} else {
			return fmt.Errorf("%s: state %d has no terminator",
				ir.PkgFuncName(fn), i)
		}
		label := typedInt(pos, types.Types[types.TUINT32], int64(i))
		cases[i] = ir.NewCaseStmt(pos, []ir.Node{label}, body)
	}
	dispatch := ir.NewSwitchStmt(pos, resumePC, cases)
	resume.Body = []ir.Node{ir.NewForStmt(pos, nil, nil, nil, []ir.Node{
		dispatch,
		ir.NewReturnStmt(pos, []ir.Node{
			typedInt(pos, types.Types[types.TUINT8], int64(actionInvalid)),
		}),
	}, false)}

	finishLowering(candidate, resume, declarations, readAssignments)
	return nil
}

func finishLowering(candidate *lowerCandidate, resume *ir.Func,
	declarations ir.Nodes, readAssignments []*ir.AssignListStmt) {
	fn := candidate.function.Func
	factory := candidate.factory
	pos := fn.Pos()

	oldCurFunc := ir.CurFunc
	ir.CurFunc = resume
	typecheck.Stmts(resume.Body)

	ir.CurFunc = factory
	factory.Body = append(declarations,
		ir.NewReturnStmt(pos, []ir.Node{resume.OClosure}))
	typecheck.Stmts(factory.Body)

	ir.CurFunc = fn
	inputs := fn.Type().RecvParams()
	args := make(ir.Nodes, len(inputs))
	for i, field := range inputs {
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
}

func edgeForCall(function *Function, call *ir.CallExpr) Edge {
	for _, edge := range function.Edges {
		if edge.Node == call {
			return edge
		}
	}
	return Edge{}
}

func callResultTargets(call *ir.CallExpr, stmt ir.Node,
	count int) (ir.Nodes, error) {
	if count == 0 {
		if stmt == nil || stmt == call {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"coroutine call has no results but is nested in another statement")
	}
	targets := make(ir.Nodes, count)
	switch stmt := stmt.(type) {
	case *ir.CallExpr:
		if stmt == call {
			return targets, nil
		}
	case *ir.AssignStmt:
		if stmt.Y == call && count == 1 {
			if !ir.IsBlank(stmt.X) {
				targets[0] = stmt.X
			}
			return targets, nil
		}
	case *ir.AssignListStmt:
		if len(stmt.Rhs) == 1 && stmt.Rhs[0] == call &&
			len(stmt.Lhs) == count {
			for i, target := range stmt.Lhs {
				if !ir.IsBlank(target) {
					targets[i] = target
				}
			}
			return targets, nil
		}
	}
	if assignment, ok := normalizedResultAssignment(call, stmt, count); ok {
		targets := slices.Clone(assignment.Lhs)
		for i, target := range targets {
			if ir.IsBlank(target) {
				targets[i] = nil
			}
		}
		return targets, nil
	}
	return nil, fmt.Errorf("coroutine call has %d results without matching assignment",
		count)
}

func normalizedResultAssignment(call *ir.CallExpr, stmt ir.Node,
	count int) (*ir.AssignListStmt, bool) {
	outer, ok := stmt.(*ir.AssignListStmt)
	if !ok || len(outer.Lhs) != count || len(outer.Rhs) != count {
		return nil, false
	}
	for _, target := range outer.Lhs {
		if ir.IsBlank(target) {
			continue
		}
		if _, ok := target.(*ir.Name); !ok {
			return nil, false
		}
	}

	var inner *ir.AssignListStmt
	ir.Visit(outer, func(node ir.Node) {
		if inner != nil {
			return
		}
		assignment, ok := node.(*ir.AssignListStmt)
		if !ok || len(assignment.Rhs) != 1 ||
			assignment.Rhs[0] != call || len(assignment.Lhs) != count {
			return
		}
		inner = assignment
	})
	if inner == nil {
		return nil, false
	}
	if callInitNode(outer, call) == nil {
		return nil, false
	}
	for i, result := range outer.Rhs {
		if !isResultProjection(result, inner.Lhs[i]) {
			return nil, false
		}
	}
	return inner, true
}

// isResultProjection reports whether node only converts a temporary result.
// Other expressions need general evaluation-order lowering around suspension.
func isResultProjection(node, result ir.Node) bool {
	for {
		conversion, ok := node.(*ir.ConvExpr)
		if !ok {
			return node == result
		}
		switch conversion.Op() {
		case ir.OCONV, ir.OCONVNOP:
			node = conversion.X
		default:
			return false
		}
	}
}

func callInitNode(node ir.Node, call *ir.CallExpr) ir.Node {
	if init := node.Init(); len(init) != 0 {
		found := false
		ir.VisitList(init, func(node ir.Node) {
			if node == call {
				found = true
			}
		})
		if found {
			return node
		}
	}

	var result ir.Node
	ir.DoChildren(node, func(child ir.Node) bool {
		result = callInitNode(child, call)
		return result != nil
	})
	return result
}

// takeCallInit detaches the containing initialization list that evaluates
// call. Its surrounding expression then reads the result temporaries after
// resume.
func takeCallInit(node ir.Node, call *ir.CallExpr) ir.Nodes {
	if init := callInitNode(node, call); init != nil {
		return ir.TakeInit(init)
	}
	return nil
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
