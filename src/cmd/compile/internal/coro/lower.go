// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/reflectdata"
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
	actionSwitch
)

// Keep this in sync with runtime.stacklessCoroFrameChunkSize. The array length
// is part of compiler-generated typed storage rather than the factory ABI.
const explicitFrameChunkSize = 4

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
	directCalls   map[*ir.CallExpr]lowerDirectCall
	dependencies  map[*ir.Func]bool
	deferDeps     map[*ir.Func]bool
	defers        []*lowerDefer
	dynamicDefers bool
	channels      map[ir.Node]*lowerChannel
	selects       map[*ir.SelectStmt]*lowerSelect
	rangeVars     []*ir.Name
	panics        map[*ir.UnaryExpr]bool
	parameters    map[*ir.Name]*ir.Name
	results       map[*ir.Name]*ir.Name
	resultValues  []*ir.Name
	resultPtrs    []*ir.Name
	factory       *ir.Func
	factoryABI    FactoryABI
	fusedAwaits   map[*ir.CallExpr]bool
	fusedRequests map[*ir.CallExpr]bool
	fusedFrame    bool
	fusedResume   bool
	fusedSelf     bool
	selfAwait     bool
	selfSpawn     bool
}

type lowerDirectCall struct {
	function *ir.Func
	errno    bool
}

type lowerFactoryCall struct {
	setup  ir.Nodes
	frame  ir.Node
	resume ir.Node
}

type lowerDefer struct {
	statement       *ir.GoDeferStmt
	call            *ir.CallExpr
	armed           *ir.Name
	sourceLiteral   bool
	literalCaptures []*ir.Name
	rewriteTerminal TerminalFlags
	terminalTarget  *ir.Func
	namedRecover    bool
	runTerminal     bool
	directGoexit    bool
	captures        []lowerDeferCapture
}

type lowerDeferCapture struct {
	source   ir.Node
	snapshot *ir.Name
}

type lowerChannel struct {
	node       ir.Node
	statement  ir.Node
	container  ir.Node
	channel    ir.Node
	sendValue  ir.Node
	recvValue  ir.Node
	recvOK     ir.Node
	recvOKTemp *ir.Name
	rangeStmt  *ir.RangeStmt
	rangeChan  *ir.Name
}

type lowerSelect struct {
	statement   *ir.SelectStmt
	cases       []*lowerSelectCase
	defaultCase *ir.CommClause
	descriptors *ir.Name
	chosen      *ir.Name
	received    *ir.Name
}

type lowerSelectCase struct {
	clause       *ir.CommClause
	operation    ir.Node
	channel      ir.Node
	sendValue    ir.Node
	recvValue    ir.Node
	recvOK       ir.Node
	send         bool
	index        int
	channelValue *ir.Name
	elementValue *ir.Name
}

type lowerRead struct {
	call      *ir.CallExpr
	statement ir.Node
	receiver  *ir.Name
	buffer    *ir.Name
	n         *ir.Name
	err       *ir.Name
	dummyErr  *ir.Name
	direct    *ir.Name
	status    *ir.Name
	wait      *ir.Name
	start     ir.Node
	finish    ir.Node
}

type lowerState struct {
	body       []ir.Node
	transition SiteKind
	call       *ir.CallExpr
	channel    *lowerChannel
	selection  *lowerSelect
	read       *lowerRead
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

	// Remove callers whose structured children cannot use the required ABI.
	// Iterate to a fixed point so one unsupported leaf removes every
	// dependent caller, including callers that require the stricter defer
	// factory contract.
	var deferFactories map[*ir.Func]bool
	for {
		changed := false
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
		deferFactories = deferFactoryCandidates(plan, candidates)
		for _, function := range functions {
			fn := function.Func
			candidate := candidates[fn]
			if candidate == nil {
				continue
			}
			var unsupported []string
			for dependency := range candidate.deferDeps {
				if !hasDeferFactory(plan, deferFactories, dependency) {
					unsupported = append(unsupported,
						ir.PkgFuncName(dependency))
				}
			}
			if len(unsupported) == 0 {
				continue
			}
			slices.Sort(unsupported)
			result.Diagnostics = append(result.Diagnostics,
				fmt.Sprintf("%s: unsupported coroutine defer dependency %s",
					ir.PkgFuncName(fn), unsupported[0]))
			delete(candidates, fn)
			changed = true
		}
		if !changed {
			break
		}
	}
	for _, function := range functions {
		function.Defer = NoDeferABI
		if plainDeferEntrySupported(plan, function) ||
			deferFactories[function.Func] {
			function.Defer = DeferABI1
		}
	}
	for _, function := range functions {
		candidate := candidates[function.Func]
		if candidate == nil {
			continue
		}
		candidate.factoryABI = FactoryABI1
		if explicitFrameFactorySupported(candidate) {
			candidate.factoryABI = FactoryABI3
		}
		candidate.factory = newResumeFactory(candidate.function.Func,
			candidate.factoryABI)
		factories[function.Func] = candidate.factory
		candidate.parameters = make(map[*ir.Name]*ir.Name)
		candidate.results = make(map[*ir.Name]*ir.Name)
		inputs := candidate.function.Func.Type().RecvParams()
		paramOffset := 0
		if candidate.factoryABI == FactoryABI3 {
			paramOffset = 1
		}
		for i, field := range inputs {
			source, _ := field.Nname.(*ir.Name)
			target, _ := candidate.factory.Type().Param(paramOffset + i).Nname.(*ir.Name)
			if source != nil && target != nil {
				candidate.parameters[source.Canonical()] = target
			}
		}
		paramCount := len(inputs)
		for i, field := range candidate.function.Func.Type().Results() {
			source, _ := field.Nname.(*ir.Name)
			value := typecheck.TempAt(candidate.function.Func.Pos(),
				candidate.factory, field.Type)
			pointer, _ := candidate.factory.Type().Param(paramOffset + paramCount + i).Nname.(*ir.Name)
			if source != nil {
				candidate.results[source.Canonical()] = value
			}
			candidate.resultValues = append(candidate.resultValues, value)
			candidate.resultPtrs = append(candidate.resultPtrs, pointer)
		}
	}
	markFusedFrameCandidates(candidates)
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
			function.Factory = candidate.factoryABI
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
	channelStatements := make(map[ir.Node]ir.Node)
	channelContainers := make(map[ir.Node]ir.Node)
	selects := make(map[*ir.SelectStmt]*lowerSelect)
	selectOperations := make(map[ir.Node]*ir.SelectStmt)
	goCalls := make(map[*ir.CallExpr]bool)
	var defers []*lowerDefer
	dynamicDefers := false
	var rangeVars []*ir.Name
	recordedRangeVars := make(map[*ir.Name]bool)
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
			containers := ir.Nodes{stmt}
			switch stmt := stmt.(type) {
			case *ir.BlockStmt:
				containers = nil
			case *ir.IfStmt:
				containers = ir.Nodes{stmt.Cond}
			case *ir.ForStmt:
				containers = ir.Nodes{stmt.Cond}
			case *ir.RangeStmt:
				containers = ir.Nodes{stmt.X}
				if stmt.X != nil && stmt.X.Type() != nil &&
					stmt.X.Type().IsChan() {
					channelStatements[stmt] = stmt
					channelContainers[stmt] = stmt
				}
			case *ir.SelectStmt:
				containers = nil
				for _, clause := range stmt.Cases {
					if clause != nil && clause.Comm != nil {
						containers = append(containers, clause.Comm)
					}
				}
			}
			for _, root := range containers {
				ir.Visit(root, func(node ir.Node) {
					call := recordAssignmentCall(node)
					if call != nil && awaitContainers[call] == nil {
						awaitContainers[call] = stmt
					}
					switch node := node.(type) {
					case *ir.SendStmt:
						channelStatements[node] = node
					case *ir.AssignStmt:
						if recv, ok := node.Y.(*ir.UnaryExpr); ok &&
							recv.Op() == ir.ORECV {
							channelStatements[recv] = node
						}
					case *ir.AssignListStmt:
						if len(node.Rhs) == 1 {
							if recv, ok := node.Rhs[0].(*ir.UnaryExpr); ok &&
								recv.Op() == ir.ORECV {
								channelStatements[recv] = node
							}
						}
					}
				})
				ir.Visit(root, func(node ir.Node) {
					switch node.Op() {
					case ir.OSEND, ir.ORECV, ir.OSELECT, ir.ORANGE:
						if channelStatements[node] == nil {
							channelStatements[node] = stmt
						}
						if channelContainers[node] == nil {
							channelContainers[node] = stmt
						}
					}
				})
			}
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
			case *ir.RangeStmt:
				if stmt.Label != nil {
					return fmt.Errorf("labeled range loop")
				}
				if stmt.X == nil || stmt.X.Type() == nil ||
					!stmt.X.Type().IsChan() {
					return fmt.Errorf("unsupported range loop")
				}
				if variable := transformedRangeVariable(stmt); variable != nil &&
					!recordedRangeVars[variable.Canonical()] {
					recordedRangeVars[variable.Canonical()] = true
					rangeVars = append(rangeVars, variable)
				}
				if err := collectStatements(stmt.Body, true); err != nil {
					return err
				}
			case *ir.SelectStmt:
				selection, err := newLowerSelect(stmt)
				if err != nil {
					return err
				}
				selects[stmt] = selection
				for _, selected := range selection.cases {
					selectOperations[selected.operation] = stmt
					selectOperations[selected.clause.Comm] = stmt
				}
				for _, clause := range stmt.Cases {
					if err := collectStatements(clause.Init(), inLoop); err != nil {
						return err
					}
					if err := collectStatements(clause.Body, inLoop); err != nil {
						return err
					}
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
					ir.OLABEL:
					return fmt.Errorf("control flow %v", stmt.Op())
				}
			}
		}
		return nil
	}
	if err := collectStatements(fn.Body, false); err != nil {
		return nil, err
	}
	if len(rangeVars) != 0 {
		rangeVariableSet := make(map[*ir.Name]bool, len(rangeVars))
		for _, variable := range rangeVars {
			rangeVariableSet[variable.Canonical()] = true
		}
		rangeClosureCapture := false
		ir.Visit(fn, func(node ir.Node) {
			if rangeClosureCapture {
				return
			}
			closure, ok := node.(*ir.ClosureExpr)
			if !ok {
				return
			}
			for _, variable := range closure.Func.ClosureVars {
				if rangeVariableSet[variable.Canonical()] {
					rangeClosureCapture = true
				}
			}
		})
		if rangeClosureCapture {
			return nil, fmt.Errorf("range variable captured by closure")
		}
	}

	edges := make(map[*ir.CallExpr]Edge)
	for _, edge := range function.Edges {
		edges[edge.Node] = edge
		if edge.Kind != DirectCall || edge.Recipe.Kind != SiteInvalid {
			continue
		}
		_, known := plan.edgeSummary(edge)
		hasAwait := slices.ContainsFunc(function.Sites, func(site Site) bool {
			return site.Kind == SiteAwait && site.Node == edge.Node
		})
		if known && plan.edgeNeedsCoroEntry(edge) && !hasAwait {
			return nil, fmt.Errorf(
				"direct call to %s requires coroutine factory entry",
				edge.CalleeName)
		}
	}
	candidate := &lowerCandidate{
		function:      function,
		transitions:   make(map[*ir.CallExpr]SiteKind),
		foreignCalls:  make(map[*ir.CallExpr]ForeignCallClass),
		directCalls:   make(map[*ir.CallExpr]lowerDirectCall),
		dependencies:  make(map[*ir.Func]bool),
		deferDeps:     make(map[*ir.Func]bool),
		defers:        defers,
		dynamicDefers: dynamicDefers,
		channels:      make(map[ir.Node]*lowerChannel),
		selects:       selects,
		rangeVars:     rangeVars,
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
		if site.Kind == SiteChannel {
			if stmt, ok := site.Node.(*ir.SelectStmt); ok {
				if candidate.selects[stmt] == nil {
					return nil, fmt.Errorf("unplanned select site %d", site.ID)
				}
				continue
			}
			if selectOperations[site.Node] != nil {
				continue
			}
			channel, err := newLowerChannel(site.Node,
				channelStatements[site.Node], channelContainers[site.Node])
			if err != nil {
				return nil, fmt.Errorf("channel site %d: %v", site.ID, err)
			}
			if candidate.channels[channel.statement] != nil {
				return nil, fmt.Errorf(
					"multiple channel operations in one statement")
			}
			candidate.channels[channel.statement] = channel
			continue
		}
		call, ok := site.Node.(*ir.CallExpr)
		if !ok {
			return nil, fmt.Errorf("non-call site %d", site.ID)
		}
		deferredSite := slices.ContainsFunc(defers,
			func(deferred *lowerDefer) bool {
				return deferred.call == call
			})
		if deferredSite && site.Kind == SiteGoexit {
			// The deferred call is registered here rather than executed.
			// planDeferTerminal validates and rewrites the operation below.
			continue
		}
		if deferredSite {
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
			if edge.Callee == fn {
				candidate.selfAwait = true
			}
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
			if edge.Callee == fn {
				candidate.selfSpawn = true
			}
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
				direct := lowerDirectCall{
					function: edge.Direct,
					errno:    edge.Recipe.Errno,
				}
				if direct.errno {
					if err := validateCgoErrnoCall(call,
						awaitStatements[call], direct); err != nil {
						return nil, err
					}
				}
				candidate.directCalls[call] = direct
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
				deferred.sourceLiteral = true
				deferred.literalCaptures = slices.Clone(
					edge.Callee.ClosureVars)
				captured := make(map[*ir.Name]bool,
					len(deferred.literalCaptures))
				for _, variable := range deferred.literalCaptures {
					source := variable.Canonical()
					if source == nil || source.Type() == nil {
						return nil, fmt.Errorf(
							"repeated source defer capture has no type")
					}
					captured[source] = true
				}
				nested := false
				ir.Visit(edge.Callee, func(node ir.Node) {
					closure, ok := node.(*ir.ClosureExpr)
					if !ok || closure.Func == edge.Callee {
						return
					}
					for _, variable := range closure.Func.ClosureVars {
						if captured[variable.Canonical()] {
							nested = true
						}
					}
				})
				if nested {
					return nil, fmt.Errorf(
						"nested repeated source defer capture")
				}
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
			deferred.runTerminal = terminal.factory
			deferred.directGoexit = terminal.directGoexit
			deferred.namedRecover = terminal.target != nil &&
				!deferred.runTerminal && !deferred.directGoexit &&
				summary.Terminal&UsesRecover != 0
			if deferred.runTerminal {
				candidate.dependencies[terminal.target] = true
				candidate.deferDeps[terminal.target] = true
			}
		}
	}
	return candidate, nil
}

// transformedRangeVariable recognizes the assignment inserted by
// loopvar.ForCapture for a per-iteration range variable.
func transformedRangeVariable(stmt *ir.RangeStmt) *ir.Name {
	if stmt == nil || len(stmt.Body) == 0 {
		return nil
	}
	assignment, ok := stmt.Body[0].(*ir.AssignStmt)
	if !ok || !assignment.Def || assignment.Y != stmt.Key {
		return nil
	}
	variable, ok := assignment.X.(*ir.Name)
	if !ok {
		return nil
	}
	for _, init := range assignment.Init() {
		declaration, ok := init.(*ir.Decl)
		if ok && declaration.X.Canonical() == variable.Canonical() {
			return variable
		}
	}
	return nil
}

func newLowerSelect(stmt *ir.SelectStmt) (*lowerSelect, error) {
	if stmt == nil {
		return nil, fmt.Errorf("nil select")
	}
	if stmt.Label != nil {
		return nil, fmt.Errorf("labeled select")
	}
	if len(stmt.Compiled) != 0 || stmt.Walked() {
		return nil, fmt.Errorf("already lowered select")
	}
	if len(stmt.Cases) > 1<<16 {
		return nil, fmt.Errorf("select has %d cases", len(stmt.Cases))
	}

	selection := &lowerSelect{statement: stmt}
	var sends, receives []*lowerSelectCase
	checkTarget := func(target ir.Node, result int) error {
		if target == nil || ir.IsBlank(target) {
			return nil
		}
		if _, ok := target.(*ir.Name); !ok {
			return fmt.Errorf("receive result %d is not a variable", result)
		}
		if target.Type() == nil {
			return fmt.Errorf("receive result %d has no type", result)
		}
		return nil
	}
	for _, clause := range stmt.Cases {
		if clause == nil {
			return nil, fmt.Errorf("nil select case")
		}
		if clause.Comm == nil {
			if selection.defaultCase != nil {
				return nil, fmt.Errorf("multiple select defaults")
			}
			selection.defaultCase = clause
			continue
		}

		selected := &lowerSelectCase{clause: clause}
		switch operation := clause.Comm.(type) {
		case *ir.SendStmt:
			selected.operation = operation
			selected.channel = operation.Chan
			selected.sendValue = operation.Value
			selected.send = true
			sends = append(sends, selected)

		case *ir.UnaryExpr:
			if operation.Op() != ir.ORECV {
				return nil, fmt.Errorf("unsupported select operation %v",
					operation.Op())
			}
			selected.operation = operation
			selected.channel = operation.X
			receives = append(receives, selected)

		case *ir.AssignStmt:
			receive, ok := operation.Y.(*ir.UnaryExpr)
			if !ok || receive.Op() != ir.ORECV {
				return nil, fmt.Errorf("unsupported select assignment")
			}
			if err := checkTarget(operation.X, 0); err != nil {
				return nil, err
			}
			selected.operation = receive
			selected.channel = receive.X
			selected.recvValue = operation.X
			receives = append(receives, selected)

		case *ir.AssignListStmt:
			if (operation.Op() != ir.OSELRECV2 &&
				operation.Op() != ir.OAS2RECV &&
				operation.Op() != ir.OAS2) ||
				len(operation.Lhs) != 2 || len(operation.Rhs) != 1 {
				return nil, fmt.Errorf("unsupported select receive assignment")
			}
			receive, ok := operation.Rhs[0].(*ir.UnaryExpr)
			if !ok || receive.Op() != ir.ORECV {
				return nil, fmt.Errorf("select receive has no receive operation")
			}
			for i, target := range operation.Lhs {
				if err := checkTarget(target, i); err != nil {
					return nil, err
				}
			}
			selected.operation = receive
			selected.channel = receive.X
			selected.recvValue = operation.Lhs[0]
			selected.recvOK = operation.Lhs[1]
			receives = append(receives, selected)

		default:
			return nil, fmt.Errorf("unsupported select operation %v",
				clause.Comm.Op())
		}
		if selected.channel == nil || selected.channel.Type() == nil ||
			!selected.channel.Type().IsChan() {
			return nil, fmt.Errorf("select operation has no channel type")
		}
	}
	selection.cases = append(sends, receives...)
	for i, selected := range selection.cases {
		selected.index = i
	}
	return selection, nil
}

// stacklessSelectCaseType matches runtime.scase. The runtime helper accepts a
// byte pointer, just like selectgo, so this compiler-private type only freezes
// the two-pointer layout used by the generated frame.
func stacklessSelectCaseType(pos src.XPos) *types.Type {
	return types.NewStruct([]*types.Field{
		types.NewField(pos, typecheck.Lookup("c"),
			types.Types[types.TUNSAFEPTR]),
		types.NewField(pos, typecheck.Lookup("elem"),
			types.Types[types.TUNSAFEPTR]),
	})
}

func newLowerChannel(node, statement, container ir.Node) (*lowerChannel, error) {
	if node == nil || statement == nil || container == nil {
		return nil, fmt.Errorf("operation has no owning statement")
	}
	channel := &lowerChannel{
		node: node, statement: statement, container: container,
	}
	switch node := node.(type) {
	case *ir.SendStmt:
		if statement != node || container != node {
			return nil, fmt.Errorf("nested send")
		}
		channel.channel = node.Chan
		channel.sendValue = node.Value

	case *ir.UnaryExpr:
		if node.Op() != ir.ORECV {
			return nil, fmt.Errorf("invalid receive operation %v", node.Op())
		}
		channel.channel = node.X
		switch statement := statement.(type) {
		case *ir.UnaryExpr:
			if statement != node {
				return nil, fmt.Errorf("nested receive")
			}
		case *ir.AssignStmt:
			if statement.Y != node {
				return nil, fmt.Errorf("nested receive assignment")
			}
			channel.recvValue = statement.X
		case *ir.AssignListStmt:
			if (statement.Op() != ir.OAS2RECV &&
				statement.Op() != ir.OAS2) ||
				len(statement.Lhs) != 2 || len(statement.Rhs) != 1 ||
				statement.Rhs[0] != node {
				return nil, fmt.Errorf("unsupported receive assignment")
			}
			channel.recvValue = statement.Lhs[0]
			channel.recvOK = statement.Lhs[1]
		default:
			return nil, fmt.Errorf("nested receive in %v", statement.Op())
		}
		for i, target := range []ir.Node{channel.recvValue, channel.recvOK} {
			if target == nil || ir.IsBlank(target) {
				continue
			}
			if _, ok := target.(*ir.Name); !ok {
				return nil, fmt.Errorf(
					"receive result %d is not a variable", i)
			}
			if target.Type() == nil {
				return nil, fmt.Errorf(
					"receive result %d has no type", i)
			}
		}
		if statement != container &&
			!normalizedReceiveAssignment(node, statement, container) {
			return nil, fmt.Errorf("nested receive assignment")
		}

	case *ir.RangeStmt:
		if statement != node || container != node {
			return nil, fmt.Errorf("nested channel range")
		}
		if node.Value != nil {
			return nil, fmt.Errorf("channel range has value variable")
		}
		if node.DistinctVars {
			return nil, fmt.Errorf("unprocessed range variables")
		}
		if node.Key != nil && !ir.IsBlank(node.Key) {
			if _, ok := node.Key.(*ir.Name); !ok {
				return nil, fmt.Errorf("range result is not a variable")
			}
			if node.Key.Type() == nil {
				return nil, fmt.Errorf("range result has no type")
			}
		}
		channel.channel = node.X
		channel.rangeStmt = node

	default:
		return nil, fmt.Errorf("unsupported operation %v", node.Op())
	}
	if channel.channel == nil || channel.channel.Type() == nil ||
		!channel.channel.Type().IsChan() {
		return nil, fmt.Errorf("operation has no channel type")
	}
	return channel, nil
}

type lowerDeferTerminal struct {
	rewrite      TerminalFlags
	target       *ir.Func
	factory      bool
	directGoexit bool
}

func planDeferTerminal(plan *Plan, fn *ir.Func,
	summary FuncSummary) (lowerDeferTerminal, bool) {
	if fn == nil || summary.Terminal == 0 ||
		summary.Terminal&^(MayPanic|UsesRecover|MayGoexit) != 0 {
		return lowerDeferTerminal{}, false
	}
	function := plan.Functions[fn]
	if function != nil && summary.Terminal != function.Terminal {
		return lowerDeferTerminal{}, false
	}

	// A source defer literal is private to this call site, so its direct
	// terminal operations can be rewritten in place.
	if function != nil && fn.OClosure != nil && !fn.Wrapper() {
		if !hasDirectDeferTerminal(plan, fn, summary.Terminal) {
			return lowerDeferTerminal{}, false
		}
		return lowerDeferTerminal{rewrite: summary.Terminal}, true
	}

	target := fn
	if function != nil && fn.Wrapper() {
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

	if recipe, ok := plan.operationRecipe(target); ok {
		if recipe.Kind != SiteGoexit ||
			recipe.Terminal != summary.Terminal {
			return lowerDeferTerminal{}, false
		}
		return lowerDeferTerminal{
			target: target, directGoexit: true,
		}, true
	}

	targetSummary, known := plan.funcSummary(target)
	if !known || targetSummary.Terminal != summary.Terminal ||
		targetSummary.Effect != NoSuspend || targetSummary.Exec != 0 {
		return lowerDeferTerminal{}, false
	}
	if targetSummary.Primary() == PlainPrimary {
		if plan.Functions[target] != nil {
			if !hasDirectDeferTerminal(plan, target,
				targetSummary.Terminal) {
				return lowerDeferTerminal{}, false
			}
		} else if targetSummary.Defer != DeferABI1 {
			return lowerDeferTerminal{}, false
		}
		return lowerDeferTerminal{target: target}, true
	}

	if plan.Functions[target] != nil {
		if !canPlanDeferFactoryTarget(plan, target,
			targetSummary.Terminal) {
			return lowerDeferTerminal{}, false
		}
	} else if targetSummary.Defer != DeferABI1 ||
		targetSummary.Factory != FactoryABI1 ||
		!resumeFactorySupported(target) {
		return lowerDeferTerminal{}, false
	}
	return lowerDeferTerminal{target: target, factory: true}, true
}

func canPlanDeferFactoryTarget(plan *Plan, fn *ir.Func,
	want TerminalFlags) bool {
	function := plan.Functions[fn]
	if function == nil || function.Terminal != want ||
		function.Effect != NoSuspend || function.Exec != 0 ||
		(FuncSummary{Terminal: want}).Primary() != CoroPrimary ||
		!resumeFactorySupported(fn) {
		return false
	}
	found := function.LocalTerminal
	for _, edge := range function.Edges {
		if edge.Kind == GoCall {
			return false
		}
		summary, known := plan.edgeSummary(edge)
		if !known {
			return false
		}
		if edge.Recipe.Kind != SiteInvalid &&
			(edge.Recipe.Kind != SiteGoexit ||
				edge.Recipe.Terminal != MayGoexit ||
				function.LocalTerminal&MayGoexit == 0) {
			return false
		}
		if edge.Kind == DirectCall &&
			summary.Terminal&UsesRecover != 0 {
			return false
		}
		found |= summary.Terminal
	}
	return found == want
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

func resumeFactoryType(fn *ir.Func, abi FactoryABI) *types.Type {
	pos := fn.Pos()
	resumeType := stacklessResumeType()
	results := []*types.Field{types.NewField(pos, nil, resumeType)}
	if abi == FactoryABI2 || abi == FactoryABI3 {
		results = append([]*types.Field{
			types.NewField(pos, nil, types.Types[types.TUNSAFEPTR]),
		}, results...)
	}
	inputs := fn.Type().RecvParams()
	paramOffset := 0
	if abi == FactoryABI3 {
		paramOffset = 1
	}
	params := make([]*types.Field, paramOffset+len(inputs))
	if abi == FactoryABI3 {
		params[0] = types.NewField(pos,
			fn.Sym().Pkg.Lookup(".coroctx"), types.Types[types.TUNSAFEPTR])
	}
	for i, field := range inputs {
		// Type checking has normalized a variadic argument to its slice.
		// Deliberately leave IsDDD unset on the factory parameter.
		params[paramOffset+i] = types.NewField(field.Pos, field.Sym, field.Type)
	}
	for i, field := range fn.Type().Results() {
		sym := fn.Sym().Pkg.LookupNum(".cororesult", i)
		params = append(params,
			types.NewField(field.Pos, sym, types.NewPtr(field.Type)))
	}
	return types.NewSignature(nil, params, results)
}

func resumeFactorySymbol(fn *ir.Func) *types.Sym {
	return fn.Sym().Pkg.Lookup(fn.Sym().Name + ".coro")
}

func newResumeFactory(fn *ir.Func, abi FactoryABI) *ir.Func {
	pos := fn.Pos()
	typ := resumeFactoryType(fn, abi)
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
	if !ok || (summary.Factory != FactoryABI1 &&
		summary.Factory != FactoryABI2 &&
		summary.Factory != FactoryABI3) || !resumeFactorySupported(fn) {
		return nil, false
	}
	pos := fn.Pos()
	return ir.NewFunc(pos, pos, resumeFactorySymbol(fn),
		resumeFactoryType(fn, summary.Factory)), true
}

func resumeFactoryABI(factory, fn *ir.Func) FactoryABI {
	if factory == nil || fn == nil || factory.Type() == nil || fn.Type() == nil {
		return NoFactory
	}
	switch factory.Type().NumResults() {
	case 1:
		return FactoryABI1
	case 2:
		params := len(fn.Type().RecvParams()) + fn.Type().NumResults()
		switch factory.Type().NumParams() {
		case params:
			return FactoryABI2
		case params + 1:
			return FactoryABI3
		}
	}
	return NoFactory
}

// explicitFrameFactorySupported limits explicit-frame lowering to
// state-machine functions whose storage can move into one generated frame
// without rewriting nested closure or defer capture ownership.
func explicitFrameFactorySupported(candidate *lowerCandidate) bool {
	if candidate == nil || canLowerRunToCompletion(candidate) ||
		candidate.function.Defer != NoDeferABI || len(candidate.defers) != 0 ||
		len(candidate.rangeVars) != 0 || candidate.function.Terminal != 0 {
		return false
	}
	for _, site := range candidate.function.Sites {
		switch site.Kind {
		// These transitions keep their suspended state in the resume frame.
		case SiteYield, SiteAwait, SiteChannel, SiteTimer, SiteFile, SitePoll:
		default:
			return false
		}
	}
	supported := true
	ir.Visit(candidate.function.Func, func(node ir.Node) {
		if _, ok := node.(*ir.ClosureExpr); ok {
			supported = false
		}
	})
	return supported
}

// markFusedFrameCandidates identifies local structured awaits whose explicit
// frames can share one logical task. Cross-package factories retain the
// ordinary task path until their export ABI describes the extended frame
// header required when the resume entry changes.
func markFusedFrameCandidates(candidates map[*ir.Func]*lowerCandidate) {
	for _, candidate := range candidates {
		if candidate.factoryABI != FactoryABI3 {
			continue
		}
		for call, transition := range candidate.transitions {
			if transition != SiteAwait {
				continue
			}
			edge := edgeForCall(candidate.function, call)
			child := candidates[edge.Callee]
			if child == nil || child.factoryABI != FactoryABI3 {
				continue
			}
			self := edge.Callee == candidate.function.Func
			if self && candidate.selfSpawn {
				continue
			}
			if candidate.fusedAwaits == nil {
				candidate.fusedAwaits = make(map[*ir.CallExpr]bool)
			}
			candidate.fusedAwaits[call] = true
			child.fusedFrame = true
			if self {
				child.fusedSelf = true
			} else {
				if candidate.fusedRequests == nil {
					candidate.fusedRequests = make(map[*ir.CallExpr]bool)
				}
				candidate.fusedRequests[call] = true
				child.fusedResume = true
			}
		}
	}
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

func deferFactoryCandidates(plan *Plan,
	candidates map[*ir.Func]*lowerCandidate) map[*ir.Func]bool {
	available := make(map[*ir.Func]bool)
	for fn, candidate := range candidates {
		if deferFactoryCandidate(plan, candidate) {
			available[fn] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for fn := range available {
			candidate := candidates[fn]
			supported := true
			for dependency := range candidate.dependencies {
				summary, known := plan.funcSummary(dependency)
				if !known {
					supported = false
					break
				}
				if summary.Primary() != CoroPrimary {
					continue
				}
				if plan.Functions[dependency] != nil {
					supported = available[dependency]
				} else {
					supported = summary.Defer == DeferABI1 &&
						summary.Factory == FactoryABI1
				}
				if !supported {
					break
				}
			}
			if supported {
				continue
			}
			delete(available, fn)
			changed = true
		}
	}
	return available
}

func deferFactoryCandidate(plan *Plan, candidate *lowerCandidate) bool {
	if candidate == nil || candidate.function.Effect != NoSuspend ||
		candidate.function.Exec != 0 ||
		candidate.function.Terminal == 0 ||
		!resumeFactorySupported(candidate.function.Func) {
		return false
	}
	for _, edge := range candidate.function.Edges {
		if edge.Kind == GoCall {
			return false
		}
		if edge.Kind != DirectCall {
			continue
		}
		summary, known := plan.edgeSummary(edge)
		if !known || summary.Terminal&UsesRecover != 0 {
			return false
		}
	}
	return true
}

func hasDeferFactory(plan *Plan, available map[*ir.Func]bool,
	fn *ir.Func) bool {
	if plan.Functions[fn] != nil {
		return available[fn]
	}
	summary, ok := Summary(fn)
	return ok && summary.Defer == DeferABI1 &&
		summary.Factory == FactoryABI1 && resumeFactorySupported(fn)
}

func plainDeferEntrySupported(plan *Plan, function *Function) bool {
	fn := function.Func
	return function.Primary == PlainPrimary &&
		function.Effect == NoSuspend && function.Exec == 0 &&
		function.Terminal != 0 && fn != nil && !fn.Wrapper() &&
		resumeFactorySupported(fn) &&
		hasDirectDeferTerminal(plan, fn, function.Terminal)
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
	for _, channel := range candidate.channels {
		if channel.sendValue != nil {
			return true
		}
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

func rewriteDeferGoexitCall(deferred *lowerDefer, resume *ir.Func,
	token *ir.Name, target *ir.Func) (*ir.ClosureExpr, error) {
	if target == nil {
		return nil, fmt.Errorf("missing direct Goexit target")
	}

	closure, wrapped := deferred.call.Fun.(*ir.ClosureExpr)
	var call *ir.CallExpr
	if wrapped {
		if len(closure.Func.Body) != 1 {
			return nil, fmt.Errorf("%s: direct Goexit wrapper has %d statements",
				ir.PkgFuncName(closure.Func), len(closure.Func.Body))
		}
		var ok bool
		call, ok = closure.Func.Body[0].(*ir.CallExpr)
		if !ok {
			return nil, fmt.Errorf("%s: direct Goexit wrapper has no call",
				ir.PkgFuncName(closure.Func))
		}
		callee := ir.StaticCalleeName(call.Fun)
		if callee == nil || callee.Func != target {
			return nil, fmt.Errorf("%s: direct Goexit wrapper calls %s",
				ir.PkgFuncName(closure.Func), symbolName(callee))
		}
	} else {
		callee := ir.StaticCalleeName(deferred.call.Fun)
		if callee == nil || callee.Func != target {
			return nil, fmt.Errorf("direct Goexit defer has no static target")
		}
		if len(target.Type().RecvParams()) != 0 ||
			target.Type().NumResults() != 0 {
			return nil, fmt.Errorf("%s: direct Goexit call is not normalized",
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
	var body ir.Nodes
	if call != nil {
		if len(call.Args) != 0 {
			return nil, fmt.Errorf("%s: direct Goexit wrapper has arguments",
				ir.PkgFuncName(wrapper))
		}
		body = append(body, ir.TakeInit(call)...)
	}
	body = append(body, typecheck.Call(deferred.statement.Pos(),
		typecheck.LookupRuntime("coroDeferGoexit"),
		ir.Nodes{token}, false))
	wrapper.Body = body
	return closure, nil
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
		if terminalAware {
			// Generated terminal control can bypass every source return.
			// Initialize the captured result slot so SSA does not require a
			// value assigned only on normal-return paths.
			declarations = append(declarations, ir.NewAssignStmt(pos, result,
				ir.NewZero(pos, result.Type())))
		}
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
			if direct := candidate.directCalls[node]; direct.function != nil {
				node.Fun = direct.function.Nname
				if direct.errno {
					setCgoDirectCallType(node, direct.function.Type())
				}
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

			post, err := rewriteCgoErrnoResult(stmt, foreignCall,
				candidate.directCalls[foreignCall], resume, edit)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", ir.PkgFuncName(fn), err)
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
				body = append(body, post...)
			case DirectMayBlock:
				body = append(body, blockingForeignEnter(stmt.Pos(), ctx)...)
				body = append(body, edited)
				body = append(body, blockingForeignExit(stmt.Pos())...)
				body = append(body, post...)
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
	finishLowering(candidate, resume, declarations)
	return nil
}

func reparentClosureCaptures(fn, resume *ir.Func,
	deferClosures map[*ir.Func]bool, edit func(ir.Node) ir.Node) error {
	// Source function literals now execute inside resume. Reparent them and
	// route their captures through resume's copy of each factory local.
	// Defer wrappers need specialized snapshot and terminal rewriting, so
	// leave those closures alone here.
	var captureErr error
	ir.Visit(fn, func(node ir.Node) {
		if captureErr != nil {
			return
		}
		closure, ok := node.(*ir.ClosureExpr)
		if !ok || closure.Func.ClosureParent != fn ||
			deferClosures[closure.Func] {
			return
		}
		closure.Func.ClosureParent = resume
		for _, variable := range closure.Func.ClosureVars {
			outer, ok := edit(variable.Outer).(*ir.Name)
			if !ok {
				captureErr = fmt.Errorf(
					"%s: closure capture is not a variable",
					ir.PkgFuncName(fn))
				return
			}
			variable.Outer = outer
			variable.Defn = outer.Canonical()
		}
		// Reparenting can change the canonical definition of a variable
		// captured again by a nested literal. Refresh that descendant chain
		// before escape analysis assigns locations.
		ir.Visit(closure.Func, func(node ir.Node) {
			nested, ok := node.(*ir.ClosureExpr)
			if !ok {
				return
			}
			for _, variable := range nested.Func.ClosureVars {
				variable.Defn = variable.Outer.Canonical()
			}
		})
	})
	return captureErr
}

func lowerFunction(candidate *lowerCandidate, factories map[*ir.Func]*ir.Func) error {
	function := candidate.function
	fn := function.Func
	factory := candidate.factory
	resumeType := stacklessResumeType()
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

	captureCells := make(map[*ir.Name]*ir.Name)
	captureStorage := make(map[*ir.Name]*ir.Name)
	var captureCellInitializers ir.Nodes
	addCaptureCell := func(variable *ir.Name) {
		source := variable.Canonical()
		if captureCells[source] != nil {
			return
		}
		storage := source
		parameter := candidate.parameters[source]
		result := candidate.results[source]
		if parameter != nil {
			storage = parameter
		} else if result != nil {
			storage = result
		}
		cell := typecheck.TempAt(source.Pos(), factory,
			types.NewPtr(source.Type()))
		captureCells[source] = cell
		captureStorage[storage.Canonical()] = cell
		addDeclaration(ir.NewDecl(source.Pos(), ir.ODCL, cell))

		if parameter == nil && result == nil {
			captureCellInitializers = append(captureCellInitializers,
				ir.NewAssignStmt(source.Pos(), cell,
					ir.NewZero(source.Pos(), cell.Type())))
			return
		}
		allocation := typedNew(source.Pos(), source.Type())
		captureCellInitializers = append(captureCellInitializers,
			ir.NewAssignStmt(source.Pos(), cell, allocation))
		if parameter != nil {
			captureCellInitializers = append(captureCellInitializers,
				ir.NewAssignStmt(source.Pos(),
					typedDeref(source.Pos(), cell, source.Type()),
					parameter))
		}
	}
	for _, variable := range candidate.rangeVars {
		addCaptureCell(variable)
	}
	for _, deferred := range candidate.defers {
		if !deferred.sourceLiteral {
			continue
		}
		for _, variable := range deferred.literalCaptures {
			addCaptureCell(variable)
		}
	}
	captureCell := func(name *ir.Name) *ir.Name {
		if cell := captureCells[name.Canonical()]; cell != nil {
			return cell
		}
		return captureStorage[name.Canonical()]
	}
	// Source locals become factory locals. ABI 1 captures them in an ordinary
	// closure; the explicit-frame ABIs copy their initial values into one
	// typed frame.
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

	terminalAware := needsTerminalEntry(candidate)
	for _, result := range candidate.resultValues {
		addDeclaration(ir.NewDecl(pos, ir.ODCL, result))
		if terminalAware {
			// Generated terminal control can bypass every source return.
			// Initialize the captured result slot so SSA does not require a
			// value assigned only on normal-return paths.
			declarations = append(declarations, ir.NewAssignStmt(pos, result,
				ir.NewZero(pos, result.Type())))
		}
	}
	declarations = append(declarations, captureCellInitializers...)
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
			deferred.runTerminal || deferred.directGoexit) &&
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
		// Explicit frames also replace references in the defining assignment,
		// so it must no longer remain the outer variable's definition.
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
	captureCellAllocation := func(name *ir.Name) ir.Node {
		cell := captureCell(name)
		if cell == nil {
			return nil
		}
		allocation := typedNew(name.Pos(), name.Type())
		assign := ir.NewAssignStmt(name.Pos(), capture(cell), allocation)
		assign.SetTypecheck(1)
		return assign
	}
	newBodyState := func(body ir.Nodes, next int) int {
		filtered := make(ir.Nodes, 0, len(body)+1)
		allocated := make(map[*ir.Name]bool)
		for _, stmt := range body {
			if init := stmt.Init(); len(init) != 0 {
				for i, initStmt := range init {
					decl, ok := initStmt.(*ir.Decl)
					if !ok || captureCell(decl.X) == nil {
						continue
					}
					name := decl.X.Canonical()
					if !allocated[name] {
						init[i] = captureCellAllocation(decl.X)
						allocated[name] = true
					}
				}
				stmt.(ir.InitNode).SetInit(init)
			}
			if decl, ok := stmt.(*ir.Decl); ok {
				if captureCell(decl.X) != nil {
					name := decl.X.Canonical()
					if !allocated[name] {
						filtered = append(filtered,
							captureCellAllocation(decl.X))
						allocated[name] = true
					}
					continue
				}
				addDeclaration(decl)
				continue
			}
			switch stmt := stmt.(type) {
			case *ir.AssignStmt:
				if stmt.Def {
					if name, ok := stmt.X.(*ir.Name); ok {
						source := name.Canonical()
						if allocation := captureCellAllocation(name); allocation != nil &&
							!allocated[source] {
							filtered = append(filtered, allocation)
							allocated[source] = true
						}
					}
				}
			case *ir.AssignListStmt:
				if stmt.Def {
					for _, target := range stmt.Lhs {
						if name, ok := target.(*ir.Name); ok {
							source := name.Canonical()
							if allocation := captureCellAllocation(name); allocation != nil &&
								!allocated[source] {
								filtered = append(filtered, allocation)
								allocated[source] = true
							}
						}
					}
				}
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
			if _, ok := node.(*ir.ReturnStmt); ok {
				required = true
				return
			}
			if candidate.channels[node] != nil {
				required = true
				return
			}
			if selectStmt, ok := node.(*ir.SelectStmt); ok &&
				candidate.selects[selectStmt] != nil {
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
		case *ir.BlockStmt, *ir.IfStmt, *ir.ForStmt, *ir.SelectStmt:
			return requiresControlLowering(stmt)
		}
		if channelOperation(stmt, candidate.channels) != nil {
			return true
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
				if candidate.channels[stmt] != nil {
					break
				}
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

		case *ir.RangeStmt:
			channel := candidate.channels[stmt]
			if channel == nil || channel.rangeStmt != stmt {
				panic("unexpected coroutine range boundary")
			}
			channelType := channel.channel.Type()
			elementType := channelType.Elem()
			rangeChan := typecheck.TempAt(stmt.Pos(), factory, channelType)
			rangeValue := typecheck.TempAt(stmt.Pos(), factory, elementType)
			rangeOK := typecheck.TempAt(stmt.Pos(), factory,
				types.Types[types.TBOOL])
			channel.rangeChan = rangeChan
			channel.recvValue = rangeValue
			channel.recvOK = rangeOK
			for _, name := range []*ir.Name{
				rangeChan, rangeValue, rangeOK,
			} {
				addDeclaration(ir.NewDecl(stmt.Pos(), ir.ODCL, name))
			}

			receiveState := addState(lowerState{
				transition: SiteChannel,
				channel:    channel,
				statement:  stmt,
				next:       -1,
				thenState:  -1,
				elseState:  -1,
			})
			bodyNext := receiveState
			if elementType.HasPointers() {
				clear := ir.NewAssignStmt(stmt.Pos(), channel.recvValue,
					ir.NewZero(stmt.Pos(), elementType))
				bodyNext = newBodyState(ir.Nodes{clear}, bodyNext)
			}
			bodyState := lowerStatements(stmt.Body, bodyNext)
			if stmt.Key != nil && !ir.IsBlank(stmt.Key) {
				value := ir.Node(channel.recvValue)
				if !types.Identical(stmt.Key.Type(), elementType) {
					conversion := ir.NewConvExpr(stmt.Pos(), ir.OCONV,
						stmt.Key.Type(), value)
					conversion.TypeWord = stmt.KeyTypeWord
					conversion.SrcRType = stmt.KeySrcRType
					value = conversion
				}
				assignment := ir.NewAssignStmt(stmt.Pos(), stmt.Key, value)
				bodyState = newBodyState(ir.Nodes{assignment}, bodyState)
			}
			branchState := addState(lowerState{
				condition: channel.recvOK,
				next:      -1,
				thenState: bodyState,
				elseState: next,
			})
			states[receiveState].next = branchState

			initialize := ir.NewAssignStmt(stmt.Pos(), channel.rangeChan,
				channel.channel)
			entryState := newBodyState(ir.Nodes{initialize}, receiveState)
			return lowerStatements(stmt.Init(), entryState)

		case *ir.SelectStmt:
			selection := candidate.selects[stmt]
			if selection == nil || selection.statement != stmt {
				panic("unexpected coroutine select boundary")
			}
			if len(selection.cases) == 0 &&
				selection.defaultCase != nil {
				entry := lowerStatements(selection.defaultCase.Body, next)
				entry = lowerStatements(selection.defaultCase.Init(), entry)
				return lowerStatements(stmt.Init(), entry)
			}

			selection.chosen = typecheck.TempAt(stmt.Pos(), factory,
				types.Types[types.TINT])
			selection.received = typecheck.TempAt(stmt.Pos(), factory,
				types.Types[types.TBOOL])
			for _, name := range []*ir.Name{
				selection.chosen, selection.received,
			} {
				addDeclaration(ir.NewDecl(stmt.Pos(), ir.ODCL, name))
			}
			if len(selection.cases) != 0 {
				caseType := stacklessSelectCaseType(stmt.Pos())
				selection.descriptors = typecheck.TempAt(stmt.Pos(), factory,
					types.NewArray(caseType, int64(len(selection.cases))))
				addDeclaration(ir.NewDecl(stmt.Pos(), ir.ODCL,
					selection.descriptors))
			}
			for _, selected := range selection.cases {
				channelType := selected.channel.Type()
				selected.channelValue = typecheck.TempAt(
					selected.operation.Pos(), factory, channelType)
				addDeclaration(ir.NewDecl(selected.operation.Pos(),
					ir.ODCL, selected.channelValue))
				if selected.sendValue != nil ||
					(selected.recvValue != nil &&
						!ir.IsBlank(selected.recvValue)) {
					selected.elementValue = typecheck.TempAt(
						selected.operation.Pos(), factory,
						channelType.Elem())
					addDeclaration(ir.NewDecl(selected.operation.Pos(),
						ir.ODCL, selected.elementValue))
				}
			}

			selectedByClause := make(map[*ir.CommClause]*lowerSelectCase,
				len(selection.cases))
			for _, selected := range selection.cases {
				selectedByClause[selected.clause] = selected
			}
			var initialize ir.Nodes
			for _, clause := range stmt.Cases {
				initialize = append(initialize, clause.Init()...)
				selected := selectedByClause[clause]
				if selected == nil {
					continue
				}
				initialize = append(initialize, ir.NewAssignStmt(
					selected.operation.Pos(), selected.channelValue,
					selected.channel))
				if selected.sendValue != nil {
					initialize = append(initialize, ir.NewAssignStmt(
						selected.operation.Pos(), selected.elementValue,
						selected.sendValue))
				}
				descriptor := ir.NewIndexExpr(selected.operation.Pos(),
					selection.descriptors,
					ir.NewInt(selected.operation.Pos(),
						int64(selected.index)))
				channelField := ir.NewSelectorExpr(selected.operation.Pos(),
					ir.ODOT, descriptor, typecheck.Lookup("c"))
				channelPointer := typecheck.ConvNop(selected.channelValue,
					types.Types[types.TUNSAFEPTR])
				initialize = append(initialize, ir.NewAssignStmt(
					selected.operation.Pos(), channelField,
					channelPointer))
				if selected.elementValue != nil {
					descriptor = ir.NewIndexExpr(selected.operation.Pos(),
						selection.descriptors,
						ir.NewInt(selected.operation.Pos(),
							int64(selected.index)))
					elementField := ir.NewSelectorExpr(
						selected.operation.Pos(), ir.ODOT, descriptor,
						typecheck.Lookup("elem"))
					elementPointer := typecheck.ConvNop(
						typecheck.NodAddr(selected.elementValue),
						types.Types[types.TUNSAFEPTR])
					initialize = append(initialize, ir.NewAssignStmt(
						selected.operation.Pos(), elementField,
						elementPointer))
				}
			}

			clearValues := func(selected *lowerSelectCase) ir.Nodes {
				var body ir.Nodes
				if selected != nil && !selected.send &&
					selected.recvValue != nil &&
					!ir.IsBlank(selected.recvValue) {
					body = append(body, ir.NewAssignStmt(
						selected.operation.Pos(), selected.recvValue,
						selected.elementValue))
				}
				if selected != nil && !selected.send &&
					selected.recvOK != nil &&
					!ir.IsBlank(selected.recvOK) {
					body = append(body, ir.NewAssignStmt(
						selected.operation.Pos(), selected.recvOK,
						selection.received))
				}
				for _, candidate := range selection.cases {
					body = append(body, ir.NewAssignStmt(
						candidate.operation.Pos(), candidate.channelValue,
						ir.NewZero(candidate.operation.Pos(),
							candidate.channelValue.Type())))
					if candidate.elementValue != nil &&
						candidate.elementValue.Type().HasPointers() {
						body = append(body, ir.NewAssignStmt(
							candidate.operation.Pos(),
							candidate.elementValue,
							ir.NewZero(candidate.operation.Pos(),
								candidate.elementValue.Type())))
					}
				}
				return body
			}

			dispatch := next
			for i := len(selection.cases) - 1; i >= 0; i-- {
				selected := selection.cases[i]
				caseState := lowerStatements(selected.clause.Body, next)
				caseState = lowerStatements(clearValues(selected), caseState)
				condition := ir.NewBinaryExpr(selected.operation.Pos(),
					ir.OEQ, selection.chosen,
					ir.NewInt(selected.operation.Pos(),
						int64(selected.index)))
				dispatch = addState(lowerState{
					condition: condition,
					next:      -1,
					thenState: caseState,
					elseState: dispatch,
				})
			}
			if selection.defaultCase != nil {
				defaultState := lowerStatements(
					selection.defaultCase.Body, next)
				defaultState = lowerStatements(
					clearValues(nil), defaultState)
				condition := ir.NewBinaryExpr(stmt.Pos(), ir.OLT,
					selection.chosen, ir.NewInt(stmt.Pos(), 0))
				dispatch = addState(lowerState{
					condition: condition,
					next:      -1,
					thenState: defaultState,
					elseState: dispatch,
				})
			}

			selectState := addState(lowerState{
				transition: SiteChannel,
				selection:  selection,
				statement:  stmt,
				next:       dispatch,
				thenState:  -1,
				elseState:  -1,
			})
			entryState := lowerStatements(initialize, selectState)
			return lowerStatements(stmt.Init(), entryState)
		}
		if init := ir.TakeInit(stmt); len(init) != 0 {
			continuation := next
			if transitionCall(stmt, candidate.transitions) != nil ||
				channelOperation(stmt, candidate.channels) != nil {
				continuation = lowerStatement(stmt, next)
			} else {
				continuation = newBodyState(ir.Nodes{stmt}, next)
			}
			return lowerStatements(init, continuation)
		}
		if channel := candidate.channels[stmt]; channel != nil {
			stateNext := next
			if channel.recvOK != nil && !ir.IsBlank(channel.recvOK) &&
				!types.Identical(channel.recvOK.Type(),
					types.Types[types.TBOOL]) {
				if channel.recvOKTemp == nil {
					channel.recvOKTemp = typecheck.TempAt(
						channel.node.Pos(), factory,
						types.Types[types.TBOOL])
					addDeclaration(ir.NewDecl(channel.node.Pos(), ir.ODCL,
						channel.recvOKTemp))
				}
				conversion := ir.NewAssignStmt(channel.node.Pos(),
					channel.recvOK, typecheck.Conv(channel.recvOKTemp,
						channel.recvOK.Type()))
				stateNext = newBodyState(ir.Nodes{conversion}, next)
			}
			return addState(lowerState{
				transition: SiteChannel,
				channel:    channel,
				statement:  stmt,
				next:       stateNext,
				thenState:  -1,
				elseState:  -1,
			})
		}
		if channel := channelOperation(stmt, candidate.channels); channel != nil {
			init := takeNodeInit(stmt, channel.node)
			if len(init) == 0 {
				panic("coroutine channel operation has no normalized init")
			}
			continuation := newBodyState(ir.Nodes{stmt}, next)
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
				var read *lowerRead
				if ordinaryReadOperation(call) {
					read = &lowerRead{call: call, statement: stmt}
					stateNext = addState(lowerState{
						read:      read,
						next:      stateNext,
						thenState: -1,
						elseState: -1,
					})
				}
				return addState(lowerState{
					transition: candidate.transitions[call],
					call:       call,
					read:       read,
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
	factoryCall := func(call *ir.CallExpr,
		statement ir.Node) (lowerFactoryCall, error) {
		edge := edgeForCall(function, call)
		child := factories[edge.Callee]
		if child == nil {
			return lowerFactoryCall{}, fmt.Errorf("%s: missing factory for %s",
				ir.PkgFuncName(fn), edge.CalleeName)
		}
		childABI := resumeFactoryABI(child, edge.Callee)
		if childABI == NoFactory {
			return lowerFactoryCall{}, fmt.Errorf("%s: invalid factory for %s",
				ir.PkgFuncName(fn), edge.CalleeName)
		}
		args := make(ir.Nodes, 0, len(call.Args)+1)
		if childABI == FactoryABI3 {
			args = append(args, ctx)
		}
		for _, arg := range call.Args {
			args = append(args, edit(arg))
		}
		targets, err := callResultTargets(call, statement,
			edge.Callee.Type().NumResults())
		if err != nil {
			return lowerFactoryCall{}, fmt.Errorf("%s: %v",
				ir.PkgFuncName(fn), err)
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
		factoryCall := typecheck.Call(call.Pos(), child.Nname, args, false)
		if child.Type().NumResults() == 1 {
			return lowerFactoryCall{resume: factoryCall}, nil
		}
		frame := typecheck.TempAt(call.Pos(), resume,
			types.Types[types.TUNSAFEPTR])
		childResume := typecheck.TempAt(call.Pos(), resume,
			stacklessResumeType())
		assignment := ir.NewAssignListStmt(call.Pos(), ir.OAS2,
			ir.Nodes{frame, childResume}, ir.Nodes{factoryCall})
		setup := ir.Nodes{
			ir.NewDecl(call.Pos(), ir.ODCL, frame),
			ir.NewDecl(call.Pos(), ir.ODCL, childResume),
		}
		if candidate.fusedRequests[call] {
			setup = append(setup, typecheck.Call(call.Pos(),
				typecheck.LookupRuntime("coroRequestFusedFrame"),
				ir.Nodes{ctx}, false))
		}
		setup = append(setup, assignment)
		return lowerFactoryCall{
			setup: setup,
			frame: frame, resume: childResume,
		}, nil
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
			if direct := candidate.directCalls[node]; direct.function != nil {
				node.Fun = direct.function.Nname
				if direct.errno {
					setCgoDirectCallType(node, direct.function.Type())
				}
			}
		}
		if name, ok := node.(*ir.Name); ok {
			if cell := captureCell(name); cell != nil {
				return typedDeref(name.Pos(), capture(cell), name.Type())
			}
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

	deferClosures := make(map[*ir.Func]bool, len(candidate.defers))
	for _, deferred := range candidate.defers {
		if closure, ok := deferred.call.Fun.(*ir.ClosureExpr); ok {
			deferClosures[closure.Func] = true
		}
	}
	if err := reparentClosureCaptures(fn, resume, deferClosures, edit); err != nil {
		return err
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
	rewriteLiteralCapture := func(fn *ir.Func, variable, pointer *ir.Name) {
		var rewrite func(ir.Node) ir.Node
		rewrite = func(node ir.Node) ir.Node {
			if node == variable {
				return typedDeref(node.Pos(), pointer, variable.Type())
			}
			ir.EditChildren(node, rewrite)
			return node
		}
		for i, stmt := range fn.Body {
			fn.Body[i] = rewrite(stmt)
		}
		fn.ClosureVars = slices.DeleteFunc(fn.ClosureVars,
			func(candidate *ir.Name) bool {
				return candidate == variable
			})
	}
	for _, deferred := range candidate.defers {
		closure, ok := deferred.call.Fun.(*ir.ClosureExpr)
		if deferred.directGoexit {
			var err error
			closure, err = rewriteDeferGoexitCall(deferred, resume,
				resumeDeferToken, deferred.terminalTarget)
			if err != nil {
				return err
			}
			ok = true
		} else if deferred.runTerminal {
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
		closureVars := slices.Clone(closure.Func.ClosureVars)
		if deferred.sourceLiteral {
			literalCaptures := make(map[*ir.Name]bool,
				len(deferred.literalCaptures))
			for _, variable := range deferred.literalCaptures {
				literalCaptures[variable] = true
				cell := captureCell(variable)
				snapshot := typecheck.TempAt(variable.Pos(), resume,
					cell.Type())
				pointer := ir.NewClosureVar(variable.Pos(), closure.Func,
					snapshot)
				rewriteLiteralCapture(closure.Func, variable, pointer)
				deferred.captures = append(deferred.captures,
					lowerDeferCapture{
						source: capture(cell), snapshot: snapshot,
					})
			}
			closureVars = slices.DeleteFunc(closureVars,
				func(variable *ir.Name) bool {
					return literalCaptures[variable]
				})
		}
		for _, variable := range closureVars {
			outer := edit(variable.Outer)
			if candidate.dynamicDefers {
				if outer == nil || outer.Type() == nil {
					return fmt.Errorf("%s: defer capture has no type",
						ir.PkgFuncName(fn))
				}
				snapshot := typecheck.TempAt(variable.Pos(), resume,
					outer.Type())
				deferred.captures = append(deferred.captures,
					lowerDeferCapture{source: outer, snapshot: snapshot})
				variable.Outer = snapshot
				variable.Defn = snapshot
			} else {
				name, ok := outer.(*ir.Name)
				if !ok {
					return fmt.Errorf("%s: defer capture is not a variable",
						ir.PkgFuncName(fn))
				}
				variable.Outer = name
				variable.Defn = name.Canonical()
			}
		}
	}

	preparedReads := make(map[*lowerRead]bool)
	for i := range states {
		state := &states[i]
		read := state.read
		if read == nil || state.transition == SiteInvalid || preparedReads[read] {
			continue
		}
		preparedReads[read] = true
		if len(read.call.Args) != 2 {
			return fmt.Errorf("%s: ordinary read has %d arguments, want receiver and buffer",
				ir.PkgFuncName(fn), len(read.call.Args))
		}
		targets, err := callResultTargets(read.call, read.statement, 2)
		if err != nil {
			return fmt.Errorf("%s: %v", ir.PkgFuncName(fn), err)
		}
		for i, target := range targets {
			if target == nil {
				continue
			}
			if _, ok := target.(*ir.Name); !ok {
				return fmt.Errorf("%s: ordinary read result %d is not a variable",
					ir.PkgFuncName(fn), i)
			}
		}
		read.start, err = ordinaryReadMethod(read.call.Pos(),
			read.call.Args[0].Type(), "coroReadStart")
		if err != nil {
			return fmt.Errorf("%s: %v", ir.PkgFuncName(fn), err)
		}
		read.finish, err = ordinaryReadMethod(read.call.Pos(),
			read.call.Args[0].Type(), "coroReadFinish")
		if err != nil {
			return fmt.Errorf("%s: %v", ir.PkgFuncName(fn), err)
		}
		read.receiver = typecheck.TempAt(read.call.Pos(), factory,
			read.call.Args[0].Type())
		read.buffer = typecheck.TempAt(read.call.Pos(), resume,
			read.call.Args[1].Type())
		read.wait = typecheck.TempAt(read.call.Pos(), resume,
			types.Types[types.TBOOL])
		read.n, _ = targets[0].(*ir.Name)
		if read.n == nil {
			read.n = typecheck.TempAt(read.call.Pos(), factory,
				types.Types[types.TINT])
			addDeclaration(ir.NewDecl(read.call.Pos(), ir.ODCL, read.n))
		}
		read.err, _ = targets[1].(*ir.Name)
		if read.err == nil {
			read.dummyErr = typecheck.TempAt(read.call.Pos(), factory,
				types.ErrorType)
			read.err = read.dummyErr
			addDeclaration(ir.NewDecl(read.call.Pos(), ir.ODCL,
				read.dummyErr))
		}
		read.direct = typecheck.TempAt(read.call.Pos(), factory,
			types.Types[types.TBOOL])
		read.status = typecheck.TempAt(read.call.Pos(), factory,
			types.Types[types.TUINTPTR])
		for _, name := range []*ir.Name{
			read.receiver, read.direct, read.status,
		} {
			addDeclaration(ir.NewDecl(read.call.Pos(), ir.ODCL, name))
		}
	}

	// Dispatch once on entry, then use direct branches between states. Persist
	// the next state only before handing control to the scheduler or terminal
	// machinery; ordinary state edges do not need a frame write.
	labels := make([]*types.Sym, len(states))
	cases := make([]*ir.CaseClause, len(states))
	for i := range states {
		// Lower is also called directly by tests, where ir.CurFunc is unset, so
		// allocate labels from resume instead of using typecheck.AutoLabel.
		labels[i] = resume.Sym().Pkg.LookupNum(".coro", int(resume.Label))
		resume.Label++
		label := typedInt(pos, types.Types[types.TUINT32], int64(i))
		cases[i] = ir.NewCaseStmt(pos, []ir.Node{label}, ir.Nodes{
			ir.NewBranchStmt(pos, ir.OGOTO, labels[i]),
		})
	}
	assignResumePC := func(state int) ir.Node {
		return ir.NewAssignStmt(pos, resumePC,
			typedInt(pos, types.Types[types.TUINT32], int64(state)))
	}
	gotoState := func(state int) ir.Node {
		return ir.NewBranchStmt(pos, ir.OGOTO, labels[state])
	}
	var stateBodies ir.Nodes
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
					body = append(body, child.setup...)
					name := "coroSpawn"
					args := ir.Nodes{ctx, child.resume}
					if child.frame != nil {
						name = "coroSpawnFrame"
						args = ir.Nodes{ctx, child.frame, child.resume}
					}
					body = append(body, typecheck.Call(goStmt.Pos(),
						typecheck.LookupRuntime(name), args, false))
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
			post, err := rewriteCgoErrnoResult(stmt, foreignCall,
				candidate.directCalls[foreignCall], resume, edit)
			if err != nil {
				return fmt.Errorf("%s: %v", ir.PkgFuncName(fn), err)
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
				body = append(body, post...)
			case DirectMayBlock:
				body = append(body, blockingForeignEnter(stmt.Pos(), ctx)...)
				body = append(body, edited)
				body = append(body, blockingForeignExit(stmt.Pos())...)
				body = append(body, post...)
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
				assignResumePC(state.next),
				gotoState(state.next),
			)
		} else if state.terminal != actionInvalid {
			if state.next < 0 {
				return fmt.Errorf("%s: state %d terminal action has no cleanup state",
					ir.PkgFuncName(fn), i)
			}
			body = append(body,
				assignResumePC(state.next),
				gotoState(state.next),
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
					ir.NewStarExpr(pos, pointer), edit(value)))
			}
			body = append(body, ir.NewReturnStmt(pos, []ir.Node{
				typedInt(pos, types.Types[types.TUINT8], int64(actionComplete)),
			}))
		} else if state.read != nil && state.transition == SiteInvalid {
			if state.next < 0 {
				return fmt.Errorf("%s: read finish state %d has no continuation",
					ir.PkgFuncName(fn), i)
			}
			read := state.read
			body = append(body, typecheck.Call(read.call.Pos(), read.finish,
				ir.Nodes{
					edit(read.receiver),
					typecheck.NodAddr(edit(read.n)),
					typecheck.NodAddr(edit(read.err)),
					edit(read.direct), edit(read.status),
				}, false))
			for _, name := range []*ir.Name{read.receiver, read.dummyErr} {
				if name == nil {
					continue
				}
				body = append(body, ir.NewAssignStmt(read.call.Pos(),
					edit(name), ir.NewZero(read.call.Pos(), name.Type())))
			}
			body = append(body,
				assignResumePC(state.next),
				gotoState(state.next))
		} else if state.transition != SiteInvalid {
			if state.next < 0 {
				return fmt.Errorf("%s: state %d transition has no continuation",
					ir.PkgFuncName(fn), i)
			}
			body = append(body, assignResumePC(state.next))
			action := actionInvalid
			var actionResult ir.Node
			switch state.transition {
			case SiteYield:
				action = actionYield
			case SiteGoexit:
				body = append(body,
					typecheck.Call(state.call.Pos(),
						typecheck.LookupRuntime("coroGoexit"),
						ir.Nodes{ctx}, false),
					gotoState(state.next),
				)
			case SiteAwait:
				for _, init := range state.call.Init() {
					body = append(body, edit(init))
				}
				child, err := factoryCall(state.call, state.statement)
				if err != nil {
					return err
				}
				body = append(body, child.setup...)
				name := "coroAwait"
				args := ir.Nodes{ctx, child.resume}
				if child.frame != nil {
					name = "coroAwaitFrame"
					args = ir.Nodes{ctx, child.frame, child.resume}
				}
				fusedAwait := child.frame != nil &&
					candidate.fusedAwaits[state.call]
				if fusedAwait {
					helper := "coroAwaitFusedFrame"
					edge := edgeForCall(function, state.call)
					if edge.Callee == fn && candidate.fusedSelf &&
						!candidate.fusedResume {
						helper = "coroAwaitSelfFrame"
					}
					actionResult = typecheck.Call(state.call.Pos(),
						typecheck.LookupRuntime(helper),
						args, false)
				} else {
					body = append(body, typecheck.Call(state.call.Pos(),
						typecheck.LookupRuntime(name), args, false))
					action = actionWait
				}
			case SiteChannel:
				if selection := state.selection; selection != nil {
					casePointerType := types.NewPtr(
						types.Types[types.TUINT8])
					var casesPointer ir.Node = ir.NewNilExpr(
						state.statement.Pos(), casePointerType)
					if selection.descriptors != nil {
						descriptors := edit(selection.descriptors)
						first := ir.NewIndexExpr(state.statement.Pos(),
							descriptors, ir.NewInt(state.statement.Pos(), 0))
						casesPointer = typecheck.ConvNop(
							typecheck.NodAddr(first), casePointerType)
					}
					nsends := 0
					for _, selected := range selection.cases {
						if !selected.send {
							break
						}
						nsends++
					}
					wait := typecheck.Call(
						state.statement.Pos(),
						typecheck.LookupRuntime("coroSelect"),
						ir.Nodes{
							ctx,
							casesPointer,
							ir.NewInt(state.statement.Pos(),
								int64(nsends)),
							ir.NewInt(state.statement.Pos(),
								int64(len(selection.cases)-nsends)),
							ir.NewBasicLit(state.statement.Pos(),
								types.Types[types.TBOOL],
								constant.MakeBool(
									selection.defaultCase == nil)),
							typecheck.NodAddr(edit(selection.chosen)),
							typecheck.NodAddr(edit(selection.received)),
						}, false)
					body = append(body,
						ir.NewIfStmt(state.statement.Pos(), wait, ir.Nodes{
							ir.NewReturnStmt(state.statement.Pos(), []ir.Node{
								typedInt(state.statement.Pos(),
									types.Types[types.TUINT8],
									int64(actionWait)),
							}),
						}, nil),
						gotoState(state.next),
					)
					break
				}
				channel := state.channel
				if channel == nil {
					return fmt.Errorf("%s: state %d has no channel operation",
						ir.PkgFuncName(fn), i)
				}
				channelType := channel.channel.Type()
				elementType := channelType.Elem()
				channelValue := typecheck.TempAt(channel.node.Pos(), resume,
					channelType)
				operationChannel := channel.channel
				if channel.rangeChan != nil {
					operationChannel = channel.rangeChan
				}
				body = append(body, ir.NewAssignStmt(channel.node.Pos(),
					channelValue, edit(operationChannel)))
				if channel.sendValue != nil {
					value := typecheck.TempAt(channel.node.Pos(), factory,
						elementType)
					addDeclaration(ir.NewDecl(channel.node.Pos(), ir.ODCL,
						value))
					value = capture(value)
					body = append(body, ir.NewAssignStmt(channel.node.Pos(),
						value, edit(channel.sendValue)))
					body = append(body, typecheck.Call(channel.node.Pos(),
						typecheck.LookupRuntime("coroChanSend",
							elementType, elementType),
						ir.Nodes{ctx, channelValue,
							typecheck.NodAddr(value)}, false))
				} else {
					resultPointer := ir.Node(typecheck.NodNil())
					if channel.recvValue != nil &&
						!ir.IsBlank(channel.recvValue) {
						resultPointer = typecheck.NodAddr(
							edit(channel.recvValue))
					}
					okPointer := ir.Node(typecheck.NodNil())
					if channel.recvOK != nil && !ir.IsBlank(channel.recvOK) {
						target := channel.recvOK
						if channel.recvOKTemp != nil {
							target = channel.recvOKTemp
						}
						okPointer = typecheck.NodAddr(edit(target))
					}
					body = append(body, typecheck.Call(channel.node.Pos(),
						typecheck.LookupRuntime("coroChanRecv",
							elementType, elementType),
						ir.Nodes{ctx, channelValue, resultPointer,
							okPointer}, false))
				}
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
				wait := typecheck.Call(state.call.Pos(),
					typecheck.LookupRuntime("coroSleep"),
					ir.Nodes{ctx, duration}, false)
				body = append(body,
					ir.NewIfStmt(state.call.Pos(), wait, ir.Nodes{
						ir.NewReturnStmt(state.call.Pos(), []ir.Node{
							typedInt(state.call.Pos(),
								types.Types[types.TUINT8],
								int64(actionWait)),
						}),
					}, nil),
					gotoState(state.next),
				)
			case SiteFile, SitePoll:
				if ordinaryReadOperation(state.call) {
					read := state.read
					if read == nil {
						return fmt.Errorf("%s: ordinary read has no lowering plan",
							ir.PkgFuncName(fn))
					}
					for _, init := range state.call.Init() {
						body = append(body, edit(init))
					}
					body = append(body,
						ir.NewAssignStmt(state.call.Pos(), edit(read.receiver),
							edit(state.call.Args[0])),
						ir.NewAssignStmt(state.call.Pos(), edit(read.buffer),
							edit(state.call.Args[1])))
					wait := typecheck.Call(state.call.Pos(), read.start, ir.Nodes{
						edit(read.receiver), ctx, edit(read.buffer),
						typecheck.NodAddr(edit(read.n)),
						typecheck.NodAddr(edit(read.err)),
						typecheck.NodAddr(edit(read.direct)),
						typecheck.NodAddr(edit(read.status)),
					}, false)
					body = append(body,
						ir.NewAssignStmt(state.call.Pos(), edit(read.wait), wait),
						ir.NewAssignStmt(state.call.Pos(), edit(read.buffer),
							ir.NewZero(state.call.Pos(), read.buffer.Type())),
						ir.NewIfStmt(state.call.Pos(), edit(read.wait), ir.Nodes{
							ir.NewReturnStmt(state.call.Pos(), []ir.Node{
								typedInt(state.call.Pos(),
									types.Types[types.TUINT8], int64(actionWait)),
							}),
						}, nil),
						gotoState(state.next))
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
			if actionResult == nil {
				actionResult = typedInt(pos, types.Types[types.TUINT8],
					int64(action))
			}
			body = append(body, ir.NewReturnStmt(pos,
				[]ir.Node{actionResult}))
		} else if state.thenState >= 0 {
			thenPC := gotoState(state.thenState)
			if state.condition == nil {
				body = append(body, thenPC)
			} else {
				if state.elseState < 0 {
					return fmt.Errorf("%s: state %d branch has no false continuation",
						ir.PkgFuncName(fn), i)
				}
				elsePC := gotoState(state.elseState)
				body = append(body, ir.NewIfStmt(pos, edit(state.condition),
					ir.Nodes{thenPC}, ir.Nodes{elsePC}))
			}
		} else if state.next >= 0 {
			body = append(body, gotoState(state.next))
		} else {
			return fmt.Errorf("%s: state %d has no terminator",
				ir.PkgFuncName(fn), i)
		}
		stateBodies = append(stateBodies, ir.NewLabelStmt(pos, labels[i]))
		stateBodies = append(stateBodies, body...)
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
		dispatch,
		ir.NewReturnStmt(pos, []ir.Node{
			typedInt(pos, types.Types[types.TUINT8], int64(actionInvalid)),
		}))
	resume.Body = append(resume.Body, stateBodies...)

	finishLowering(candidate, resume, declarations)
	return nil
}

func finishLowering(candidate *lowerCandidate, resume *ir.Func,
	declarations ir.Nodes) {
	if candidate.factoryABI == FactoryABI3 {
		finishExplicitFrameLowering(candidate, resume, declarations)
		return
	}
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

	// The source inline body no longer describes the physical primary.
	fn.Inl = nil
}

func explicitFrameField(pos src.XPos, frame ir.Node,
	field *types.Field) *ir.SelectorExpr {
	selector := ir.NewSelectorExpr(pos, ir.ODOTPTR, frame, field.Sym)
	selector.Selection = field
	selector.SetType(field.Type)
	selector.SetTypecheck(1)
	return selector
}

func finishExplicitFrameLowering(candidate *lowerCandidate, resume *ir.Func,
	declarations ir.Nodes) {
	fn := candidate.function.Func
	factory := candidate.factory
	pos := fn.Pos()
	closureVars := slices.Clone(resume.ClosureVars)
	fusedFrame := candidate.fusedFrame
	selfOnly := candidate.fusedSelf && !candidate.fusedResume
	fields := make([]*types.Field, len(closureVars))
	replacements := make(map[*ir.Name]*types.Field, len(closureVars))
	outerFields := make(map[*ir.Name]*types.Field, len(closureVars))
	for i, variable := range closureVars {
		field := types.NewField(variable.Pos(),
			typecheck.LookupNum("F", i), variable.Type())
		fields[i] = field
		replacements[variable] = field
		outerFields[variable.Outer.Canonical()] = field
	}
	resultValueFields := make([]*types.Field, len(candidate.resultValues))
	rootResultValueFields := make(map[*types.Field]bool,
		len(candidate.resultValues))
	rootResultTargets := make(map[*ir.Name]*types.Field,
		len(candidate.resultPtrs))
	for i, value := range candidate.resultValues {
		resultValueFields[i] = outerFields[value.Canonical()]
		rootResultValueFields[resultValueFields[i]] = true
		rootResultTargets[candidate.resultPtrs[i].Canonical()] =
			resultValueFields[i]
	}
	frameFields := fields
	var fusedParentField, fusedOwnerField, fusedMarkerField,
		fusedResumeField *types.Field
	if fusedFrame {
		fusedParentField = types.NewField(pos,
			typecheck.Lookup(".coroParent"), types.Types[types.TUNSAFEPTR])
		fusedOwnerField = types.NewField(pos,
			typecheck.Lookup(".coroOwner"), types.Types[types.TUNSAFEPTR])
		fusedMarkerField = types.NewField(pos,
			typecheck.Lookup(".coroMarker"), types.Types[types.TUINT8])
		frameFields = make([]*types.Field, 0, len(fields)+4)
		frameFields = append(frameFields, fusedParentField, fusedOwnerField,
			fusedMarkerField)
		if candidate.fusedResume {
			fusedResumeField = types.NewField(pos,
				typecheck.Lookup(".coroResume"),
				types.Types[types.TUNSAFEPTR])
			frameFields = append(frameFields, fusedResumeField)
		}
		frameFields = append(frameFields, fields...)
	}
	frameType := types.NewStruct(frameFields)
	frameType.SetNoalg(true)
	framePointerType := types.NewPtr(frameType)
	frame := typecheck.TempAt(pos, resume, framePointerType)

	var rewrite func(ir.Node) ir.Node
	rewrite = func(node ir.Node) ir.Node {
		if variable, ok := node.(*ir.Name); ok {
			if field := replacements[variable]; field != nil {
				return explicitFrameField(node.Pos(), frame, field)
			}
		}
		ir.EditChildren(node, rewrite)
		return node
	}
	for i, stmt := range resume.Body {
		resume.Body[i] = rewrite(stmt)
	}
	resume.ClosureVars = nil
	var pointerClears ir.Nodes
	for _, field := range fields {
		if !field.Type.HasPointers() || rootResultValueFields[field] {
			continue
		}
		pointerClears = append(pointerClears, ir.NewAssignStmt(pos,
			explicitFrameField(pos, frame, field),
			ir.NewZero(pos, field.Type)))
	}
	var resultPointerClears ir.Nodes
	for i, resultField := range resultValueFields {
		if !resultField.Type.HasPointers() {
			continue
		}
		targetField := outerFields[candidate.resultPtrs[i].Canonical()]
		target := explicitFrameField(pos, frame, targetField)
		selfTarget := typecheck.NodAddr(
			explicitFrameField(pos, frame, resultField))
		externalTarget := ir.NewBinaryExpr(pos, ir.ONE, target, selfTarget)
		clear := ir.NewAssignStmt(pos,
			explicitFrameField(pos, frame, resultField),
			ir.NewZero(pos, resultField.Type))
		resultPointerClears = append(resultPointerClears,
			ir.NewIfStmt(pos, externalTarget, ir.Nodes{clear}, nil))
	}
	var clearCompletedFrame func(ir.Node) ir.Node
	clearCompletedFrame = func(node ir.Node) ir.Node {
		if ret, ok := node.(*ir.ReturnStmt); ok &&
			len(ret.Results) == 1 &&
			ir.IsConst(ret.Results[0], constant.Int) &&
			ir.Int64Val(ret.Results[0]) == int64(actionComplete) {
			var completion ir.Nodes
			if len(pointerClears)+len(resultPointerClears) != 0 {
				cacheFrame := typecheck.Call(ret.Pos(),
					typecheck.LookupRuntime("coroFrameNeedsClear"),
					ir.Nodes{resume.Dcl[0]}, false)
				clears := slices.Clone(resultPointerClears)
				clears = append(clears, pointerClears...)
				completion = append(completion,
					ir.NewIfStmt(ret.Pos(), cacheFrame, clears, nil))
			}
			if fusedFrame {
				parent := explicitFrameField(ret.Pos(), frame,
					fusedParentField)
				hasParent := ir.NewBinaryExpr(ret.Pos(), ir.ONE, parent,
					ir.NewNilExpr(ret.Pos(), fusedParentField.Type))
				completionHelper := "coroCompleteFusedFrame"
				if selfOnly {
					completionHelper = "coroCompleteSelfFrame"
				}
				action := typecheck.Call(ret.Pos(),
					typecheck.LookupRuntime(completionHelper),
					ir.Nodes{resume.Dcl[0]}, false)
				completion = append(completion, ir.NewIfStmt(ret.Pos(),
					hasParent, ir.Nodes{ir.NewReturnStmt(ret.Pos(),
						[]ir.Node{action})}, nil))
			}
			if len(completion) != 0 {
				completion = append(completion, ret)
				return ir.NewBlockStmt(ret.Pos(), completion)
			}
		}
		ir.EditChildren(node, clearCompletedFrame)
		return node
	}
	for i, stmt := range resume.Body {
		resume.Body[i] = clearCompletedFrame(stmt)
	}

	ctx := resume.Dcl[0]
	unsafePointerType := types.Types[types.TUNSAFEPTR]
	packetPointer := typecheck.ConvNop(ctx, types.NewPtr(unsafePointerType))
	packetFrame := typedDeref(pos, packetPointer, unsafePointerType)
	loadFrame := typecheck.ConvNop(packetFrame, framePointerType)
	resume.Body = append(ir.Nodes{
		ir.NewDecl(pos, ir.ODCL, frame),
		ir.NewAssignStmt(pos, frame, loadFrame),
	}, resume.Body...)

	autoInitializers := make(map[*ir.Name]bool)
	for _, variable := range closureVars {
		outer := variable.Outer.Canonical()
		switch outer.Class {
		case ir.PAUTO, ir.PAUTOHEAP:
			autoInitializers[outer] = true
		}
	}
	initializedDeclarations := make(ir.Nodes, 0,
		len(declarations)+len(autoInitializers))
	for _, declaration := range declarations {
		initializedDeclarations = append(initializedDeclarations, declaration)
		decl, ok := declaration.(*ir.Decl)
		if !ok {
			continue
		}
		name := decl.X.Canonical()
		if autoInitializers[name] {
			initializedDeclarations = append(initializedDeclarations,
				ir.NewAssignStmt(name.Pos(), name,
					ir.NewZero(name.Pos(), name.Type())))
			delete(autoInitializers, name)
		}
	}
	declarations = initializedDeclarations

	factoryFrame := typecheck.TempAt(pos, factory, framePointerType)
	declarations = append(declarations, ir.NewDecl(pos, ir.ODCL, factoryFrame))
	types.CalcSize(frameType)
	factoryResume := typecheck.TempAt(pos, factory, stacklessResumeType())
	factoryCtx, _ := factory.Type().Param(0).Nname.(*ir.Name)
	declarations = append(declarations,
		ir.NewDecl(pos, ir.ODCL, factoryResume),
		ir.NewAssignStmt(pos, factoryResume, resume.OClosure))
	frameSize := func() ir.Node {
		return typedInt(pos, types.Types[types.TUINTPTR], frameType.Size())
	}
	takeRootFrame := typecheck.Call(pos,
		typecheck.LookupRuntime("coroTakeRootFrame"), ir.Nodes{
			factoryResume, frameSize(),
		}, false)
	rootAssignment := ir.NewAssignStmt(pos, factoryFrame,
		typecheck.ConvNop(takeRootFrame, framePointerType))
	var takeChildFrame ir.Node
	// Await suspends the parent before starting its child. A recursive spawn
	// can create concurrent siblings from the same parent, so those factories
	// must not hand out the same adjacent array element.
	if fusedFrame {
		frameChunkType := types.NewArray(frameType, explicitFrameChunkSize)
		takeHelper := "coroTakeFusedFrame"
		takeArgs := ir.Nodes{
			factoryCtx, factoryResume, frameSize(),
			reflectdata.TypePtrAt(pos, frameChunkType),
		}
		if selfOnly {
			takeHelper = "coroTakeSelfFrame"
		} else {
			takeArgs = append(takeArgs,
				ir.NewBool(pos, candidate.fusedSelf))
		}
		takeFrame := typecheck.Call(pos,
			typecheck.LookupRuntime(takeHelper), takeArgs, false)
		takeChildFrame = ir.NewAssignStmt(pos, factoryFrame,
			typecheck.ConvNop(takeFrame, framePointerType))
	} else {
		takeFrame := typecheck.Call(pos,
			typecheck.LookupRuntime("coroTakeFrame"), ir.Nodes{
				factoryCtx, factoryResume, frameSize(),
			}, false)
		takeChildFrame = ir.NewAssignStmt(pos, factoryFrame,
			typecheck.ConvNop(takeFrame, framePointerType))
	}
	isRootFactory := func() ir.Node {
		return ir.NewBinaryExpr(pos, ir.OEQ, factoryCtx,
			ir.NewNilExpr(pos, factoryCtx.Type()))
	}
	declarations = append(declarations, ir.NewIfStmt(pos, isRootFactory(),
		ir.Nodes{rootAssignment}, ir.Nodes{takeChildFrame}))
	missingFrame := ir.NewBinaryExpr(pos, ir.OEQ, factoryFrame,
		ir.NewNilExpr(pos, framePointerType))
	declarations = append(declarations, ir.NewIfStmt(pos, missingFrame,
		ir.Nodes{ir.NewAssignStmt(pos, factoryFrame,
			typedNew(pos, frameType))}, nil))
	if fusedFrame {
		declarations = append(declarations,
			ir.NewAssignStmt(pos,
				explicitFrameField(pos, factoryFrame, fusedParentField),
				ir.NewNilExpr(pos, fusedParentField.Type)),
			ir.NewAssignStmt(pos,
				explicitFrameField(pos, factoryFrame, fusedOwnerField),
				ir.NewNilExpr(pos, fusedOwnerField.Type)),
			ir.NewAssignStmt(pos,
				explicitFrameField(pos, factoryFrame, fusedMarkerField),
				typedInt(pos, types.Types[types.TUINT8], 0)))
		if fusedResumeField != nil {
			declarations = append(declarations, ir.NewAssignStmt(pos,
				explicitFrameField(pos, factoryFrame, fusedResumeField),
				ir.NewNilExpr(pos, fusedResumeField.Type)))
		}
	}
	for i, variable := range closureVars {
		target := explicitFrameField(variable.Pos(), factoryFrame, fields[i])
		resultField := rootResultTargets[variable.Outer.Canonical()]
		if resultField == nil {
			declarations = append(declarations,
				ir.NewAssignStmt(pos, target, variable.Outer))
			continue
		}
		selfTarget := typecheck.NodAddr(explicitFrameField(variable.Pos(),
			factoryFrame, resultField))
		rootResult := ir.NewAssignStmt(pos, target, selfTarget)
		childResult := ir.NewAssignStmt(pos, target, variable.Outer)
		declarations = append(declarations, ir.NewIfStmt(pos, isRootFactory(),
			ir.Nodes{rootResult}, ir.Nodes{childResult}))
	}

	oldCurFunc := ir.CurFunc
	ir.CurFunc = resume
	typecheck.Stmts(resume.Body)

	ir.CurFunc = factory
	frameResult := typecheck.ConvNop(factoryFrame, unsafePointerType)
	factory.Body = append(declarations, ir.NewReturnStmt(pos, []ir.Node{
		frameResult, factoryResume,
	}))
	typecheck.Stmts(factory.Body)

	ir.CurFunc = fn
	inputs := fn.Type().RecvParams()
	args := make(ir.Nodes, 0, len(inputs)+len(fn.Type().Results())+1)
	if candidate.factoryABI == FactoryABI3 {
		args = append(args, ir.NewNilExpr(pos, unsafePointerType))
	}
	for _, field := range inputs {
		argument, _ := field.Nname.(ir.Node)
		args = append(args, argument)
	}
	for _, field := range fn.Type().Results() {
		args = append(args, ir.NewNilExpr(pos, types.NewPtr(field.Type)))
	}
	newFrame := typecheck.TempAt(pos, fn, unsafePointerType)
	newResume := typecheck.TempAt(pos, fn, stacklessResumeType())
	quiescentRoot := typecheck.TempAt(pos, fn, types.Types[types.TBOOL])
	makeFrame := typecheck.Call(pos, factory.Nname, args, false)
	assignFrame := ir.NewAssignListStmt(pos, ir.OAS2,
		ir.Nodes{newFrame, newResume}, ir.Nodes{makeFrame})
	run := typecheck.Call(pos, typecheck.LookupRuntime("coroRunFrame"),
		ir.Nodes{newFrame, newResume, frameSize()}, false)
	runFrame := ir.NewAssignStmt(pos, quiescentRoot, run)
	copyResults := make(ir.Nodes, 0, len(resultValueFields))
	var clearResultPointers ir.Nodes
	for i, field := range fn.Type().Results() {
		result, _ := field.Nname.(ir.Node)
		typedFrame := typecheck.ConvNop(newFrame, framePointerType)
		copyResults = append(copyResults, ir.NewAssignStmt(pos, result,
			explicitFrameField(pos, typedFrame, resultValueFields[i])))
		if resultValueFields[i].Type.HasPointers() {
			typedFrame = typecheck.ConvNop(newFrame, framePointerType)
			clearResultPointers = append(clearResultPointers,
				ir.NewAssignStmt(pos,
					explicitFrameField(pos, typedFrame, resultValueFields[i]),
					ir.NewZero(pos, resultValueFields[i].Type)))
		}
	}
	releaseFrame := typecheck.Call(pos,
		typecheck.LookupRuntime("coroReleaseRootFrame"), ir.Nodes{
			newFrame, newResume, frameSize(),
		}, false)
	fn.Body = []ir.Node{
		ir.NewDecl(pos, ir.ODCL, newFrame),
		ir.NewDecl(pos, ir.ODCL, newResume),
		ir.NewDecl(pos, ir.ODCL, quiescentRoot),
		assignFrame,
		runFrame,
	}
	fn.Body = append(fn.Body, copyResults...)
	fn.Body = append(fn.Body, clearResultPointers...)
	fn.Body = append(fn.Body,
		ir.NewIfStmt(pos, quiescentRoot, ir.Nodes{releaseFrame}, nil),
		ir.NewReturnStmt(pos, nil))
	typecheck.Stmts(fn.Body)
	ir.CurFunc = oldCurFunc

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

// normalizedReceiveAssignment reports whether statement evaluates recv into
// temporaries that container only projects into simple assignment targets.
// order introduces this shape to preserve assignment evaluation order.
func normalizedReceiveAssignment(recv *ir.UnaryExpr, statement,
	container ir.Node) bool {
	if recv == nil || recv.Op() != ir.ORECV ||
		initNodeContaining(container, recv) == nil {
		return false
	}
	switch statement := statement.(type) {
	case *ir.AssignStmt:
		outer, ok := container.(*ir.AssignStmt)
		if !ok || statement.Y != recv ||
			(!ir.IsBlank(outer.X) && outer.X.Op() != ir.ONAME) {
			return false
		}
		return isResultProjection(outer.Y, statement.X)

	case *ir.AssignListStmt:
		outer, ok := container.(*ir.AssignListStmt)
		if !ok || len(statement.Lhs) != 2 ||
			len(statement.Rhs) != 1 || statement.Rhs[0] != recv ||
			len(outer.Lhs) != len(outer.Rhs) {
			return false
		}
		projection := 0
		for _, result := range statement.Lhs {
			if ir.IsBlank(result) {
				continue
			}
			if projection == len(outer.Lhs) {
				return false
			}
			target := outer.Lhs[projection]
			if !ir.IsBlank(target) && target.Op() != ir.ONAME {
				return false
			}
			if !isResultProjection(outer.Rhs[projection], result) {
				return false
			}
			projection++
		}
		return projection == len(outer.Lhs)
	}
	return false
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

func initNodeContaining(node, target ir.Node) ir.Node {
	if init := node.Init(); len(init) != 0 {
		found := false
		ir.VisitList(init, func(node ir.Node) {
			if node == target {
				found = true
			}
		})
		if found {
			return node
		}
	}

	var result ir.Node
	ir.DoChildren(node, func(child ir.Node) bool {
		result = initNodeContaining(child, target)
		return result != nil
	})
	return result
}

func callInitNode(node ir.Node, call *ir.CallExpr) ir.Node {
	return initNodeContaining(node, call)
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

func takeNodeInit(node, target ir.Node) ir.Nodes {
	if init := initNodeContaining(node, target); init != nil {
		return ir.TakeInit(init)
	}
	return nil
}

func channelOperation(stmt ir.Node,
	channels map[ir.Node]*lowerChannel) *lowerChannel {
	if channel := channels[stmt]; channel != nil {
		return channel
	}
	var found *lowerChannel
	ir.Visit(stmt, func(node ir.Node) {
		if found == nil {
			found = channels[node]
		}
	})
	return found
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

func ordinaryReadMethod(pos src.XPos, receiver *types.Type, name string) (ir.Node, error) {
	base := types.ReceiverBaseType(receiver)
	if base == nil || base.Sym() == nil || base.Sym().Pkg == nil {
		return nil, fmt.Errorf("ordinary read receiver %v has no named base type",
			receiver)
	}
	sym := base.Sym().Pkg.Lookup(name)
	typecheck.CalcMethods(base)
	for _, method := range base.AllMethods() {
		if method.Sym == sym {
			return typecheck.NewMethodExpr(pos, receiver, sym), nil
		}
	}
	return nil, fmt.Errorf("ordinary read receiver %v has no %s method",
		receiver, name)
}

func validateCgoErrnoCall(call *ir.CallExpr, stmt ir.Node,
	direct lowerDirectCall) error {
	_, err := cgoErrnoAssignment(call, stmt, direct)
	return err
}

func cgoErrnoAssignment(call *ir.CallExpr, stmt ir.Node,
	direct lowerDirectCall) (*ir.AssignListStmt, error) {
	if call == nil || direct.function == nil || !direct.errno {
		return nil, fmt.Errorf("missing direct cgo errno call")
	}
	results := direct.function.Type().NumResults()
	assignment, ok := stmt.(*ir.AssignListStmt)
	if !ok || len(assignment.Rhs) != 1 || assignment.Rhs[0] != call {
		assignment, ok = normalizedResultAssignment(call, stmt, results)
		if !ok {
			return nil, fmt.Errorf(
				"direct cgo errno call has no matching result assignment")
		}
	}
	if results < 2 || len(assignment.Lhs) != results {
		return nil, fmt.Errorf("direct cgo errno call has %d result targets, want %d",
			len(assignment.Lhs), results)
	}
	target := assignment.Lhs[results-1]
	if outer, ok := stmt.(*ir.AssignListStmt); ok && outer != assignment {
		target = outer.Lhs[results-1]
	}
	if !ir.IsBlank(target) {
		if target.Op() != ir.ONAME {
			return nil, fmt.Errorf("direct cgo errno result has a non-variable target")
		}
		if target.Type() == nil || !target.Type().IsInterface() {
			return nil, fmt.Errorf("direct cgo errno result target is not an interface")
		}
	}
	errnoType := direct.function.Type().Result(results - 1).Type
	if errnoType == nil || !errnoType.IsInteger() {
		return nil, fmt.Errorf("direct cgo errno entry has a non-integer result")
	}
	return assignment, nil
}

// rewriteCgoErrnoResult keeps the raw errno value inside the foreign-call
// window. Converting syscall.Errno to error may allocate, so that conversion
// must run only after the runtime has left the blocking state.
func rewriteCgoErrnoResult(stmt ir.Node, call *ir.CallExpr,
	direct lowerDirectCall, owner *ir.Func,
	edit func(ir.Node) ir.Node) (ir.Nodes, error) {
	if !direct.errno {
		return nil, nil
	}
	assignment, err := cgoErrnoAssignment(call, stmt, direct)
	if err != nil {
		return nil, err
	}
	index := len(assignment.Lhs) - 1
	target := assignment.Lhs[index]
	targetType := target.Type()
	if outer, ok := stmt.(*ir.AssignListStmt); ok && outer != assignment {
		target = outer.Lhs[index]
		outer.Rhs[index] = ir.NewZero(call.Pos(), targetType)
	}

	errnoType := direct.function.Type().Result(index).Type
	errno := typecheck.TempAt(call.Pos(), owner, errnoType)
	assignment.Lhs[index] = errno
	if ir.IsBlank(target) {
		return nil, nil
	}

	target = edit(target)
	condition := ir.NewBinaryExpr(call.Pos(), ir.ONE, errno,
		ir.NewZero(call.Pos(), errnoType))
	nonzero := ir.NewAssignStmt(call.Pos(), target,
		typecheck.AssignConv(errno, target.Type(), "cgo errno result"))
	zero := ir.NewAssignStmt(call.Pos(), target,
		ir.NewZero(call.Pos(), target.Type()))
	return ir.Nodes{ir.NewIfStmt(call.Pos(), condition,
		ir.Nodes{nonzero}, ir.Nodes{zero})}, nil
}

func setCgoDirectCallType(call *ir.CallExpr, typ *types.Type) {
	switch typ.NumResults() {
	case 0:
		call.SetType(nil)
	case 1:
		call.SetType(typ.Result(0).Type)
	default:
		call.SetType(typ.ResultsTuple())
	}
}

// blockingForeignEnter and blockingForeignExit surround a direct call with
// the runtime's blocking foreign-call state.
func blockingForeignEnter(pos src.XPos, ctx ir.Node) ir.Nodes {
	return ir.Nodes{
		typecheck.Call(pos, typecheck.LookupRuntime("coroEnterBlocking"),
			ir.Nodes{ctx}, false),
	}
}

func blockingForeignExit(pos src.XPos) ir.Nodes {
	return ir.Nodes{
		typecheck.Call(pos, typecheck.LookupRuntime("coroExitBlocking"), nil, false),
	}
}

func typedInt(pos src.XPos, typ *types.Type, value int64) ir.Node {
	return ir.NewBasicLit(pos, typ, constant.MakeInt64(value))
}

func typedDeref(pos src.XPos, pointer ir.Node, typ *types.Type) ir.Node {
	value := ir.NewStarExpr(pos, pointer)
	value.SetType(typ)
	value.SetTypecheck(1)
	return value
}

func typedNew(pos src.XPos, typ *types.Type) ir.Node {
	allocation := ir.NewUnaryExpr(pos, ir.ONEW, ir.TypeNode(typ))
	allocation.SetType(types.NewPtr(typ))
	allocation.SetTypecheck(1)
	return allocation
}
