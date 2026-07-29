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
	actionPanic
	actionGoexit
)

// LowerResult summarizes one package lowering pass.
type LowerResult struct {
	Lowered     int
	Skipped     int
	Diagnostics []string
}

type lowerCandidate struct {
	function      *Function
	transitions   map[*ir.CallExpr]SiteKind
	foreignCalls  map[*ir.CallExpr]ForeignCallClass
	directCalls   map[*ir.CallExpr]*ir.Func
	dependencies  map[*ir.Func]bool
	defers        []*lowerDefer
	dynamicDefers bool
	panics        map[*ir.UnaryExpr]bool
	parameters    map[*ir.Name]*ir.Name
	results       map[*ir.Name]*ir.Name
	resultValues  []*ir.Name
	resultPtrs    []*ir.Name
	factory       *ir.Func
}

type lowerDefer struct {
	statement       *ir.GoDeferStmt
	call            *ir.CallExpr
	armed           *ir.Name
	rewriteTerminal TerminalFlags
	terminalTarget  *ir.Func
	namedRecover    bool
	runTerminal     bool
	captures        []lowerDeferCapture
}

type lowerDeferCapture struct {
	source   *ir.Name
	snapshot *ir.Name
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
	cleanup    bool
	terminal   uint8
	panicValue ir.Node
}

// Lower rewrites supported coroutine primaries into explicit state machines.
// The lowering accepts closed calls, if statements, simple for loops, normal
// returns, statically resolved defers, and the supported operation sites.
// Unsupported functions remain unchanged.
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
		normalizeSingleResultCalls(function)
	}
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

	// A named terminal defer target must retain its ordinary entry. In
	// particular, recover in that target executes in the caller's active
	// defer scope, not in a nested coroutine root. Conservatively remove such
	// targets before resolving structured factory dependencies.
	var terminalTargets []*ir.Func
	for _, candidate := range candidates {
		for _, deferred := range candidate.defers {
			target := deferred.terminalTarget
			if target != nil && !deferred.runTerminal &&
				candidates[target] != nil &&
				!slices.Contains(terminalTargets, target) {
				terminalTargets = append(terminalTargets, target)
			}
		}
	}
	slices.SortFunc(terminalTargets, func(a, b *ir.Func) int {
		return strings.Compare(ir.PkgFuncName(a), ir.PkgFuncName(b))
	})
	for _, target := range terminalTargets {
		delete(candidates, target)
		result.Diagnostics = append(result.Diagnostics,
			fmt.Sprintf("%s: retained ordinary terminal defer entry",
				ir.PkgFuncName(target)))
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
	awaitContainers := make(map[*ir.CallExpr]ir.Node)
	goCalls := make(map[*ir.CallExpr]bool)
	var defers []*lowerDefer
	dynamicDefers := false
	recordAssignmentCall := func(node ir.Node) *ir.CallExpr {
		switch node := node.(type) {
		case *ir.AssignStmt:
			if call, ok := node.Y.(*ir.CallExpr); ok {
				statementCalls[call] = true
				awaitStatements[call] = node
				return call
			}
		case *ir.AssignListStmt:
			if len(node.Rhs) == 1 {
				if call, ok := node.Rhs[0].(*ir.CallExpr); ok {
					statementCalls[call] = true
					awaitStatements[call] = node
					return call
				}
			}
		}
		return nil
	}
	var collectStatements func(ir.Nodes, bool) error
	collectStatements = func(list ir.Nodes, inLoop bool) error {
		for _, stmt := range list {
			if init := stmt.Init(); len(init) != 0 {
				if err := collectStatements(init, inLoop); err != nil {
					return err
				}
			}
			// Calls returning multiple results inside an expression are
			// normalized into assignments attached to that expression's
			// Init list. Record the assignment that directly owns each call
			// so lowering can detach that Init list without treating the
			// surrounding statement as the result destination.
			ir.Visit(stmt, func(node ir.Node) {
				call := recordAssignmentCall(node)
				if call != nil && awaitContainers[call] == nil {
					awaitContainers[call] = stmt
				}
			})
			switch stmt := stmt.(type) {
			case *ir.CallExpr:
				statementCalls[stmt] = true
				awaitStatements[stmt] = stmt
				awaitContainers[stmt] = stmt
			case *ir.AssignStmt, *ir.AssignListStmt:
			case *ir.GoDeferStmt:
				call, ok := stmt.Call.(*ir.CallExpr)
				if !ok {
					return fmt.Errorf("non-call go or defer statement")
				}
				if stmt.Op() == ir.ODEFER {
					if inLoop {
						dynamicDefers = true
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
		function:      function,
		transitions:   make(map[*ir.CallExpr]SiteKind),
		foreignCalls:  make(map[*ir.CallExpr]ForeignCallClass),
		directCalls:   make(map[*ir.CallExpr]*ir.Func),
		dependencies:  make(map[*ir.Func]bool),
		defers:        defers,
		dynamicDefers: dynamicDefers,
		panics:        make(map[*ir.UnaryExpr]bool),
	}
	for _, site := range function.Sites {
		if site.Kind == SitePanic {
			panicExpr, ok := site.Node.(*ir.UnaryExpr)
			if !ok || panicExpr.Op() != ir.OPANIC {
				return nil, fmt.Errorf("invalid panic site %d", site.ID)
			}
			candidate.panics[panicExpr] = true
			continue
		}
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
		case SiteYield, SiteTimer, SiteFile, SitePoll, SiteGoexit:
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
			container := awaitContainers[call]
			if statement != container && !supportsAwaitInit(container) {
				_, err := callResultTargets(call, container,
					edge.Callee.Type().NumResults())
				if err == nil {
					err = fmt.Errorf("nested result expression")
				}
				return nil, fmt.Errorf("await site %d: %v", site.ID, err)
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
			if summary, known := plan.edgeSummary(edge); !known ||
				summary.Terminal&MayPanic != 0 {
				return nil, fmt.Errorf("spawn target may panic")
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
		closure, isClosure := deferred.call.Fun.(*ir.ClosureExpr)
		if edge.Callee.OClosure != nil {
			if !isClosure || closure.Func != edge.Callee {
				return nil, fmt.Errorf("defer closure does not match call plan")
			}
			if edge.Callee.Wrapper() && edge.Callee.WrappedFunc == nil {
				return nil, fmt.Errorf("defer wrapper has no target")
			}
			if !edge.Callee.Wrapper() && dynamicDefers {
				return nil, fmt.Errorf("repeated source defer literal")
			}
		}
		if summary.Terminal != 0 {
			terminal, ok := planDeferTerminal(plan, edge.Callee, summary)
			if !ok {
				return nil, fmt.Errorf("defer target has terminal control %s",
					summary.Terminal)
			}
			deferred.rewriteTerminal = terminal.rewrite
			deferred.terminalTarget = terminal.target
			deferred.runTerminal = terminal.target != nil &&
				summary.Terminal&MayGoexit != 0
			deferred.namedRecover = terminal.target != nil &&
				!deferred.runTerminal &&
				summary.Terminal&UsesRecover != 0
			if deferred.runTerminal {
				candidate.dependencies[terminal.target] = true
			}
		}
	}
	return candidate, nil
}

type lowerDeferTerminal struct {
	rewrite TerminalFlags
	target  *ir.Func
}

func planDeferTerminal(plan *Plan, fn *ir.Func,
	summary FuncSummary) (lowerDeferTerminal, bool) {
	function := plan.Functions[fn]
	if function == nil ||
		summary.Terminal != function.Terminal ||
		summary.Terminal&^(MayPanic|UsesRecover|MayGoexit) != 0 {
		return lowerDeferTerminal{}, false
	}

	// A source defer literal is private to this call site, so its direct
	// terminal operations can be rewritten in place.
	if fn.OClosure != nil && !fn.Wrapper() {
		if !hasDirectDeferTerminal(plan, fn, summary.Terminal) {
			return lowerDeferTerminal{}, false
		}
		return lowerDeferTerminal{rewrite: summary.Terminal}, true
	}

	target := fn
	if fn.Wrapper() {
		target = fn.WrappedFunc
		if target == nil || function.LocalTerminal != 0 {
			return lowerDeferTerminal{}, false
		}
		found := false
		for _, edge := range function.Edges {
			if edge.Kind == GoCall {
				continue
			}
			callSummary, known := plan.edgeSummary(edge)
			if !known {
				return lowerDeferTerminal{}, false
			}
			if callSummary.Terminal == 0 {
				continue
			}
			if found || edge.Callee != target ||
				callSummary.Terminal != summary.Terminal {
				return lowerDeferTerminal{}, false
			}
			found = true
		}
		if !found {
			return lowerDeferTerminal{}, false
		}
	}
	if summary.Terminal&MayGoexit != 0 {
		if !hasDeferGoexitTarget(plan, target, summary.Terminal) {
			return lowerDeferTerminal{}, false
		}
	} else if !hasDirectDeferTerminal(plan, target, summary.Terminal) {
		return lowerDeferTerminal{}, false
	}
	return lowerDeferTerminal{target: target}, true
}

func hasDeferGoexitTarget(plan *Plan, fn *ir.Func,
	want TerminalFlags) bool {
	function := plan.Functions[fn]
	if function == nil || function.Terminal != want ||
		function.LocalTerminal&MayGoexit == 0 {
		return false
	}
	for _, edge := range function.Edges {
		if edge.Kind == GoCall {
			return false
		}
		callSummary, known := plan.edgeSummary(edge)
		if !known {
			return false
		}
		if callSummary.Terminal == 0 || edge.Kind == DeferCall {
			continue
		}
		if edge.Recipe.Kind != SiteGoexit ||
			callSummary.Terminal != MayGoexit {
			return false
		}
	}
	return true
}

func hasDirectDeferTerminal(plan *Plan, fn *ir.Func,
	want TerminalFlags) bool {
	function := plan.Functions[fn]
	if function == nil || function.LocalTerminal != want ||
		function.Terminal != want {
		return false
	}
	for _, edge := range function.Edges {
		if edge.Kind == GoCall {
			if want&MayGoexit != 0 {
				return false
			}
			continue
		}
		callSummary, known := plan.edgeSummary(edge)
		if !known {
			return false
		}
		if callSummary.Terminal == 0 {
			continue
		}
		if edge.Recipe.Kind != SiteGoexit ||
			callSummary.Terminal != MayGoexit {
			return false
		}
	}
	return true
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
		candidate.function.Terminal != 0 || len(candidate.foreignCalls) == 0 {
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

// needsTerminalEntry reports whether a generated resume can observe a
// structured terminal outcome or panic while executing ordinary Go code.
// Unknown IR operations are treated conservatively.
func needsTerminalEntry(candidate *lowerCandidate) bool {
	if candidate.function.Terminal != 0 || len(candidate.defers) != 0 {
		return true
	}
	for _, transition := range candidate.transitions {
		if transition == SiteAwait || transition == SiteGoexit {
			return true
		}
	}

	safeComparison := func(expr *ir.BinaryExpr) bool {
		if ir.IsNil(expr.X) || ir.IsNil(expr.Y) {
			return true
		}
		typ := expr.X.Type()
		if typ == nil {
			return false
		}
		if expr.Op() != ir.OEQ && expr.Op() != ir.ONE {
			return typ.IsInteger() || typ.IsFloat() || typ.IsString()
		}
		return typ.IsScalar() || typ.IsString() || typ.IsPtr() ||
			typ.IsUnsafePtr() || typ.IsChan()
	}
	safeAssignOp := func(stmt *ir.AssignOpStmt) bool {
		if stmt.X.Type() != nil && stmt.X.Type().IsString() {
			return false
		}
		switch stmt.AsOp {
		case ir.OADD, ir.OSUB, ir.OOR, ir.OXOR, ir.OMUL, ir.OAND,
			ir.OANDNOT:
			return true
		}
		return false
	}
	mayPanic := func(node ir.Node) bool {
		switch node := node.(type) {
		case *ir.CallExpr:
			return candidate.transitions[node] == SiteInvalid &&
				candidate.foreignCalls[node] == NotForeign
		case *ir.AssignOpStmt:
			return !safeAssignOp(node)
		case *ir.BinaryExpr:
			switch node.Op() {
			case ir.OADD:
				return node.X.Type() != nil && node.X.Type().IsString()
			case ir.OSUB, ir.OOR, ir.OXOR, ir.OMUL, ir.OAND, ir.OANDNOT:
				return false
			}
			if node.Op().IsCmp() {
				return !safeComparison(node)
			}
			return true
		}

		switch node.Op() {
		case ir.ONAME, ir.OTYPE, ir.OLITERAL, ir.ONIL,
			ir.OADDR, ir.OANDAND, ir.OAS, ir.OAS2, ir.OAS2DOTTYPE,
			ir.OAS2FUNC, ir.OAS2MAPR, ir.OCAP, ir.OCLOSURE, ir.OCONV,
			ir.OCONVIFACE, ir.OCONVNOP, ir.ODCL, ir.ODOT, ir.ODOTMETH,
			ir.ODOTTYPE2, ir.OKEY, ir.OLEN, ir.OMETHEXPR, ir.ONOT,
			ir.OBITNOT, ir.OPAREN, ir.OPLUS, ir.ONEG, ir.OOROR,
			ir.OSTRUCTKEY, ir.OBLOCK, ir.OFOR, ir.OIF, ir.OGO,
			ir.ORETURN:
			return false
		}
		return true
	}
	for _, stmt := range candidate.function.Func.Body {
		if ir.Any(stmt, mayPanic) {
			return true
		}
	}
	return false
}

// rewriteDeferTerminal replaces terminal builtins in a direct deferred
// literal with task-owned runtime operations.
func rewriteDeferTerminal(fn *ir.Func, token *ir.Name,
	want TerminalFlags) error {
	var found TerminalFlags
	var edit func(ir.Node) ir.Node
	edit = func(node ir.Node) ir.Node {
		switch node := node.(type) {
		case *ir.CallExpr:
			if node.Op() == ir.ORECOVER {
				found |= UsesRecover
				return typecheck.Call(node.Pos(),
					typecheck.LookupRuntime("coroDeferRecover"),
					ir.Nodes{token}, false)
			}
			callee := ir.StaticCalleeName(node.Fun)
			if callee != nil {
				if recipe, ok := operationRecipe(callee.Func); ok &&
					recipe.Kind == SiteGoexit {
					found |= MayGoexit
					body := make(ir.Nodes, 0, len(node.Init())+2)
					for _, init := range ir.TakeInit(node) {
						body = append(body, edit(init))
					}
					body = append(body,
						typecheck.Call(node.Pos(),
							typecheck.LookupRuntime("coroDeferGoexit"),
							ir.Nodes{token}, false),
						ir.NewReturnStmt(node.Pos(), nil),
					)
					return ir.NewBlockStmt(node.Pos(), body)
				}
			}
		case *ir.UnaryExpr:
			if node.Op() == ir.OPANIC {
				found |= MayPanic
				body := make(ir.Nodes, 0, len(node.Init())+2)
				for _, init := range ir.TakeInit(node) {
					body = append(body, edit(init))
				}
				value := edit(node.X)
				record := typecheck.Call(node.Pos(),
					typecheck.LookupRuntime("coroDeferPanic"),
					ir.Nodes{token, value}, false)
				body = append(body, record, ir.NewReturnStmt(node.Pos(), nil))
				return ir.NewBlockStmt(node.Pos(), body)
			}
		}
		ir.EditChildren(node, edit)
		return node
	}
	for i, stmt := range fn.Body {
		fn.Body[i] = edit(stmt)
	}
	if found != want {
		return fmt.Errorf("%s: defer terminal body is %s, want %s",
			ir.PkgFuncName(fn), found, want)
	}
	return nil
}

// rewriteDeferFactoryCall routes a statically resolved named defer target
// through its coroutine factory. A generated go/defer wrapper already owns
// the target's captured arguments. Calls without arguments or results do not
// have such a wrapper, so lowering creates one.
func rewriteDeferFactoryCall(deferred *lowerDefer, resume *ir.Func,
	token *ir.Name, target, factory *ir.Func) (*ir.ClosureExpr, error) {
	if target == nil || factory == nil {
		return nil, fmt.Errorf("missing terminal defer factory")
	}

	closure, wrapped := deferred.call.Fun.(*ir.ClosureExpr)
	var call *ir.CallExpr
	if wrapped {
		if len(closure.Func.Body) != 1 {
			return nil, fmt.Errorf("%s: terminal defer wrapper has %d statements",
				ir.PkgFuncName(closure.Func), len(closure.Func.Body))
		}
		var ok bool
		call, ok = closure.Func.Body[0].(*ir.CallExpr)
		if !ok {
			return nil, fmt.Errorf("%s: terminal defer wrapper has no call",
				ir.PkgFuncName(closure.Func))
		}
		callee := ir.StaticCalleeName(call.Fun)
		if callee == nil || callee.Func != target {
			return nil, fmt.Errorf("%s: terminal defer wrapper calls %s",
				ir.PkgFuncName(closure.Func), symbolName(callee))
		}
	} else {
		callee := ir.StaticCalleeName(deferred.call.Fun)
		if callee == nil || callee.Func != target {
			return nil, fmt.Errorf("terminal defer has no static target")
		}
		if len(target.Type().RecvParams()) != 0 ||
			target.Type().NumResults() != 0 {
			return nil, fmt.Errorf("%s: terminal defer call is not normalized",
				ir.PkgFuncName(target))
		}
		wrapper := ir.NewClosureFunc(deferred.statement.Pos(),
			deferred.statement.Pos(), ir.ODEFER,
			types.NewSignature(nil, nil, nil), resume, typecheck.Target, 0)
		wrapper.DeclareParams(true)
		wrapper.SetWrapper(true)
		wrapper.WrappedFunc = target
		closure = wrapper.OClosure
		deferred.call.Fun = closure
	}

	wrapper := closure.Func
	token = ir.NewClosureVar(deferred.statement.Pos(), wrapper, token)
	var args ir.Nodes
	var body ir.Nodes
	if call != nil {
		args = slices.Clone(call.Args)
		body = append(body, ir.TakeInit(call)...)
	}
	inputs := target.Type().RecvParams()
	if len(args) != len(inputs) {
		return nil, fmt.Errorf("%s: terminal defer wrapper has %d arguments, want %d",
			ir.PkgFuncName(wrapper), len(args), len(inputs))
	}
	for _, result := range target.Type().Results() {
		temp := typecheck.TempAt(deferred.statement.Pos(), wrapper, result.Type)
		body = append(body, ir.NewDecl(deferred.statement.Pos(), ir.ODCL, temp))
		args = append(args, typecheck.NodAddrAt(deferred.statement.Pos(), temp))
	}
	resumeCall := typecheck.Call(deferred.statement.Pos(), factory.Nname,
		args, false)
	body = append(body, typecheck.Call(deferred.statement.Pos(),
		typecheck.LookupRuntime("coroDeferRun"),
		ir.Nodes{token, resumeCall}, false))
	wrapper.Body = body
	return closure, nil
}

// lowerRunToCompletion builds a single-invocation resume closure. Foreign
// calls are instrumented in place, so ordinary blocks, branches, and loops
// retain their source control flow.
func lowerRunToCompletion(candidate *lowerCandidate) error {
	fn := candidate.function.Func
	factory := candidate.factory
	resumeType := factory.Type().Result(0).Type
	pos := fn.Pos()
	terminalAware := needsTerminalEntry(candidate)

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
	if terminalAware {
		terminalAction := typecheck.TempAt(pos, resume,
			types.Types[types.TUINT8])
		resume.Body = append(resume.Body,
			ir.NewAssignStmt(pos, terminalAction,
				typecheck.Call(pos,
					typecheck.LookupRuntime("coroTerminalAction"),
					ir.Nodes{ctx}, false)))
		hasTerminal := ir.NewBinaryExpr(pos, ir.ONE, terminalAction,
			typedInt(pos, types.Types[types.TUINT8], int64(actionInvalid)))
		resume.Body = append(resume.Body, ir.NewIfStmt(pos, hasTerminal, ir.Nodes{
			ir.NewReturnStmt(pos, []ir.Node{
				terminalAction,
			}),
		}, nil))
	}
	resume.Body = append(resume.Body, body...)
	resume.Body = append(resume.Body, complete(pos)...)
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
	var deferStack *ir.Name
	var deferToken *ir.Name
	if candidate.dynamicDefers {
		// Use one stack for every site so fixed and repeated registrations
		// retain their global LIFO order.
		deferType := candidate.defers[0].call.Fun.Type()
		deferStack = typecheck.TempAt(pos, factory, types.NewSlice(deferType))
		addDeclaration(ir.NewDecl(pos, ir.ODCL, deferStack))
	}
	for _, deferred := range candidate.defers {
		if (deferred.rewriteTerminal != 0 || deferred.namedRecover ||
			deferred.runTerminal) &&
			deferToken == nil {
			deferToken = typecheck.TempAt(pos, factory,
				types.Types[types.TUNSAFEPTR])
			addDeclaration(ir.NewDecl(pos, ir.ODCL, deferToken))
		}
		if !candidate.dynamicDefers {
			deferred.armed = typecheck.TempAt(deferred.statement.Pos(), factory,
				types.Types[types.TBOOL])
			addDeclaration(ir.NewDecl(deferred.statement.Pos(), ir.ODCL,
				deferred.armed))
		}
		deferByStatement[deferred.statement] = deferred
	}

	pc := typecheck.TempAt(pos, factory, types.Types[types.TUINT32])
	addDeclaration(ir.NewDecl(pos, ir.ODCL, pc))
	terminalAware := needsTerminalEntry(candidate)

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
	var resumeDeferToken *ir.Name
	if deferToken != nil {
		resumeDeferToken = capture(deferToken)
	}

	const cleanupState = 0
	completeState := cleanupState
	states := []lowerState{{
		next:      -1,
		thenState: -1,
		elseState: -1,
		cleanup:   true,
	}}
	if terminalAware {
		// A recovered native panic enters cleanup directly. Keep normal
		// completion distinct so both paths establish the cleanup state
		// before invoking deferred calls.
		completeState = len(states)
		states = append(states, lowerState{
			next:      cleanupState,
			thenState: -1,
			elseState: -1,
			terminal:  actionComplete,
		})
	}
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
			if panicExpr, ok := node.(*ir.UnaryExpr); ok &&
				panicExpr.Op() == ir.OPANIC && candidate.panics[panicExpr] {
				required = true
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
		if stmt.Op() == ir.OPANIC {
			return true
		}
		switch stmt := stmt.(type) {
		case *ir.ReturnStmt:
			return true
		case *ir.BlockStmt, *ir.IfStmt, *ir.ForStmt:
			return requiresControlLowering(stmt)
		}
		if call := transitionCall(stmt, candidate.transitions); call != nil {
			switch candidate.transitions[call] {
			case SiteYield, SiteAwait, SiteTimer, SiteFile, SitePoll, SiteForeign,
				SiteGoexit:
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
		case *ir.UnaryExpr:
			if stmt.Op() != ir.OPANIC || !candidate.panics[stmt] {
				panic(fmt.Sprintf("unexpected coroutine unary boundary %s",
					stmt.Op()))
			}
			entry := addState(lowerState{
				next:       cleanupState,
				thenState:  -1,
				elseState:  -1,
				panicValue: stmt.X,
			})
			return lowerStatements(stmt.Init(), entry)

		case *ir.ReturnStmt:
			body := make(ir.Nodes, 0, len(stmt.Results))
			for i, value := range stmt.Results {
				result, _ := fn.Type().Result(i).Nname.(*ir.Name)
				body = append(body, ir.NewAssignStmt(stmt.Pos(), result, value))
			}
			entry := lowerStatements(body, completeState)
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
			case SiteYield, SiteAwait, SiteTimer, SiteFile, SitePoll, SiteForeign,
				SiteGoexit:
				stateNext := next
				if candidate.transitions[call] == SiteGoexit {
					stateNext = cleanupState
				}
				return addState(lowerState{
					transition: candidate.transitions[call],
					call:       call,
					statement:  stmt,
					next:       stateNext,
					thenState:  -1,
					elseState:  -1,
				})
			}
		}
		panic(fmt.Sprintf("unexpected coroutine lowering boundary %s", stmt.Op()))
	}
	entryState := lowerStatements(fn.Body, completeState)
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
	// A repeated site snapshots each capture before constructing its wrapper;
	// otherwise every registration would observe the last value written to
	// the shared frame slot.
	for _, deferred := range candidate.defers {
		closure, ok := deferred.call.Fun.(*ir.ClosureExpr)
		if deferred.runTerminal {
			var err error
			closure, err = rewriteDeferFactoryCall(deferred, resume,
				resumeDeferToken, deferred.terminalTarget,
				factories[deferred.terminalTarget])
			if err != nil {
				return err
			}
			ok = true
		}
		if !ok {
			continue
		}
		closure.Func.ClosureParent = resume
		if deferred.rewriteTerminal != 0 {
			token := ir.NewClosureVar(deferred.statement.Pos(),
				closure.Func, resumeDeferToken)
			if err := rewriteDeferTerminal(closure.Func, token,
				deferred.rewriteTerminal); err != nil {
				return err
			}
		}
		for _, variable := range closure.Func.ClosureVars {
			outer, ok := edit(variable.Outer).(*ir.Name)
			if !ok {
				return fmt.Errorf("%s: defer capture is not a variable",
					ir.PkgFuncName(fn))
			}
			if candidate.dynamicDefers {
				snapshot := typecheck.TempAt(variable.Pos(), resume,
					outer.Type())
				deferred.captures = append(deferred.captures,
					lowerDeferCapture{source: outer, snapshot: snapshot})
				variable.Outer = snapshot
				variable.Defn = snapshot
			} else {
				variable.Outer = outer
				variable.Defn = outer.Canonical()
			}
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
				if candidate.dynamicDefers {
					for _, deferredCapture := range deferred.captures {
						snapshot := deferredCapture.snapshot
						assignment := ir.NewAssignStmt(deferStmt.Pos(),
							snapshot, deferredCapture.source)
						assignment.Def = true
						snapshot.Defn = assignment
						body = append(body,
							ir.NewDecl(deferStmt.Pos(), ir.ODCL, snapshot),
							assignment)
					}
					stack := capture(deferStack)
					appendCall := typecheck.Call(deferStmt.Pos(),
						types.BuiltinPkg.Lookup("append").Def.(*ir.Name),
						ir.Nodes{stack, edit(deferred.call.Fun)}, false)
					body = append(body, ir.NewAssignStmt(deferStmt.Pos(),
						stack, appendCall))
				} else {
					body = append(body, ir.NewAssignStmt(deferStmt.Pos(),
						capture(deferred.armed),
						ir.NewBasicLit(deferStmt.Pos(),
							types.Types[types.TBOOL],
							constant.MakeBool(true))))
				}
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

		if state.panicValue != nil {
			if state.next < 0 {
				return fmt.Errorf("%s: state %d panic has no cleanup state",
					ir.PkgFuncName(fn), i)
			}
			body = append(body,
				typecheck.Call(pos, typecheck.LookupRuntime("coroPanic"),
					ir.Nodes{ctx, edit(state.panicValue)}, false),
				ir.NewAssignStmt(pos, resumePC,
					typedInt(pos, types.Types[types.TUINT32],
						int64(state.next))),
				ir.NewBranchStmt(pos, ir.OCONTINUE, nil),
			)
		} else if state.terminal != actionInvalid {
			if state.next < 0 {
				return fmt.Errorf("%s: state %d terminal action has no cleanup state",
					ir.PkgFuncName(fn), i)
			}
			body = append(body,
				ir.NewAssignStmt(pos, resumePC,
					typedInt(pos, types.Types[types.TUINT32],
						int64(state.next))),
				ir.NewBranchStmt(pos, ir.OCONTINUE, nil),
			)
		} else if state.cleanup {
			if candidate.dynamicDefers {
				stack := capture(deferStack)
				deferType := candidate.defers[0].call.Fun.Type()
				deferredCall := typecheck.TempAt(pos, resume, deferType)
				stackLen := func() ir.Node {
					return ir.NewUnaryExpr(pos, ir.OLEN, stack)
				}
				lastIndex := func() ir.Node {
					return ir.NewBinaryExpr(pos, ir.OSUB, stackLen(),
						ir.NewInt(pos, 1))
				}
				load := ir.NewAssignStmt(pos, deferredCall,
					ir.NewIndexExpr(pos, stack, lastIndex()))
				clear := ir.NewAssignStmt(pos,
					ir.NewIndexExpr(pos, stack, lastIndex()),
					typecheck.NodNil())
				pop := ir.NewAssignStmt(pos, stack,
					ir.NewSliceExpr(pos, ir.OSLICE, stack, nil,
						lastIndex(), nil))
				call := ir.Node(typecheck.Call(pos, deferredCall, nil, false))
				if slices.ContainsFunc(candidate.defers,
					func(deferred *lowerDefer) bool {
						return deferred.namedRecover
					}) {
					call = typecheck.Call(pos,
						typecheck.LookupRuntime("coroDeferCall"),
						ir.Nodes{resumeDeferToken, deferredCall}, false)
				}
				condition := ir.NewBinaryExpr(pos, ir.ONE, stackLen(),
					ir.NewInt(pos, 0))
				// Pop before calling so a later replacement-panic path cannot
				// invoke the same deferred call twice.
				body = append(body, ir.NewForStmt(pos, nil, condition, nil,
					ir.Nodes{load, clear, pop, call}, false))
			} else {
				for j := len(candidate.defers) - 1; j >= 0; j-- {
					deferred := candidate.defers[j]
					armed := capture(deferred.armed)
					clear := ir.NewAssignStmt(deferred.statement.Pos(), armed,
						ir.NewBasicLit(deferred.statement.Pos(),
							types.Types[types.TBOOL], constant.MakeBool(false)))
					fun := edit(deferred.call.Fun)
					call := ir.Node(typecheck.Call(deferred.call.Pos(),
						fun, nil, false))
					if deferred.namedRecover {
						call = typecheck.Call(deferred.call.Pos(),
							typecheck.LookupRuntime("coroDeferCall"),
							ir.Nodes{resumeDeferToken, fun}, false)
					}
					body = append(body, ir.NewIfStmt(deferred.statement.Pos(),
						armed, ir.Nodes{clear, call}, nil))
				}
			}
			if terminalAware {
				terminalAction := typecheck.TempAt(pos, resume,
					types.Types[types.TUINT8])
				body = append(body, ir.NewAssignStmt(pos, terminalAction,
					typecheck.Call(pos,
						typecheck.LookupRuntime("coroTerminalAction"),
						ir.Nodes{ctx}, false)))
				hasTerminal := ir.NewBinaryExpr(pos, ir.ONE, terminalAction,
					typedInt(pos, types.Types[types.TUINT8],
						int64(actionInvalid)))
				body = append(body, ir.NewIfStmt(pos, hasTerminal, ir.Nodes{
					ir.NewReturnStmt(pos, []ir.Node{
						terminalAction,
					}),
				}, nil))
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
			case SiteGoexit:
				body = append(body,
					typecheck.Call(state.call.Pos(),
						typecheck.LookupRuntime("coroGoexit"),
						ir.Nodes{ctx}, false),
					ir.NewBranchStmt(state.call.Pos(), ir.OCONTINUE, nil),
				)
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
	resume.Body = ir.Nodes{}
	if resumeDeferToken != nil {
		token := typecheck.Call(pos,
			typecheck.LookupRuntime("coroDeferToken"), ir.Nodes{ctx}, false)
		resume.Body = append(resume.Body,
			ir.NewAssignStmt(pos, resumeDeferToken, token))
	}
	if terminalAware {
		terminalAction := typecheck.Call(pos,
			typecheck.LookupRuntime("coroTerminalAction"), ir.Nodes{ctx}, false)
		hasTerminal := ir.NewBinaryExpr(pos, ir.ONE, terminalAction,
			typedInt(pos, types.Types[types.TUINT8], int64(actionInvalid)))
		resume.Body = append(resume.Body, ir.NewIfStmt(pos, hasTerminal, ir.Nodes{
			ir.NewAssignStmt(pos, resumePC,
				typedInt(pos, types.Types[types.TUINT32], cleanupState)),
		}, nil))
	}
	resume.Body = append(resume.Body,
		ir.NewForStmt(pos, nil, nil, nil, []ir.Node{
			dispatch,
			ir.NewReturnStmt(pos, []ir.Node{
				typedInt(pos, types.Types[types.TUINT8], int64(actionInvalid)),
			}),
		}, false))

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

// supportsAwaitInit reports whether an expression Init list may be detached
// ahead of stmt without reordering evaluation of an assignment destination.
func supportsAwaitInit(stmt ir.Node) bool {
	switch stmt := stmt.(type) {
	case *ir.CallExpr, *ir.ReturnStmt:
		return true
	case *ir.AssignStmt:
		return ir.IsBlank(stmt.X) || stmt.X.Op() == ir.ONAME
	case *ir.AssignListStmt:
		for _, target := range stmt.Lhs {
			if !ir.IsBlank(target) && target.Op() != ir.ONAME {
				return false
			}
		}
		return true
	}
	return false
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
