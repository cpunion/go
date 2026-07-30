// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/obj/x86"
	"cmd/internal/src"
	"go/constant"
	"slices"
	"strings"
	"sync"
	"testing"
)

var initLowerTest sync.Once

func prepareLowerTest(t *testing.T) {
	t.Helper()
	initLowerTest.Do(func() {
		types.PtrSize = 8
		types.RegSize = 8
		types.MaxWidth = 1 << 50
		base.Ctxt = obj.Linknew(&x86.Linkamd64)
		types.LocalPkg = types.NewPkg("example.com/coro/testinit", "testinit")
		typecheck.InitUniverse()
		ir.Pkgs.Runtime = types.NewPkg("runtime", "runtime")
		typecheck.InitRuntime()
	})
}

func newLowerTestFunc(pkg *types.Pkg, name string) *ir.Func {
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name),
		types.NewSignature(nil, nil, nil))
	fn.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, fn)
	return fn
}

func newLowerTestCall(fn *ir.Func) *ir.CallExpr {
	call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, fn.Nname, nil)
	call.SetTypecheck(1)
	return call
}

func newLowerTestReturn() *ir.ReturnStmt {
	ret := ir.NewReturnStmt(src.NoXPos, nil)
	ret.SetTypecheck(1)
	return ret
}

func TestResumeFactorySupported(t *testing.T) {
	prepareLowerTest(t)

	if resumeFactorySupported(nil) {
		t.Fatal("nil function supports a resume factory")
	}
	pkg := types.NewPkg("example.com/coro/factorysupport", "factorysupport")
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("F"),
		types.NewSignature(nil, nil, nil))
	if !resumeFactorySupported(fn) {
		t.Fatal("plain function does not support a resume factory")
	}
	recv := types.NewField(src.NoXPos, pkg.Lookup("recv"),
		types.NewStruct(nil))
	method := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("M"),
		types.NewSignature(recv, nil, nil))
	method.DeclareParams(true)
	if !resumeFactorySupported(method) {
		t.Fatal("method does not support a resume factory")
	}
	methodFactory := resumeFactoryType(method)
	if methodFactory.NumRecvs() != 0 || methodFactory.NumParams() != 1 ||
		methodFactory.Param(0).Type != recv.Type {
		t.Fatalf("method factory type = %v, want explicit receiver parameter",
			methodFactory)
	}
	results := []*types.Field{
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
	}
	pair := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("Pair"),
		types.NewSignature(nil, nil, results))
	if !resumeFactorySupported(pair) {
		t.Fatal("multi-result function does not support a resume factory")
	}
	values := types.NewField(src.NoXPos, pkg.Lookup("values"),
		types.NewSlice(types.Types[types.TINT]))
	values.SetIsDDD(true)
	variadic := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("Variadic"),
		types.NewSignature(nil, []*types.Field{values}, nil))
	variadic.DeclareParams(true)
	if !resumeFactorySupported(variadic) {
		t.Fatal("variadic function does not support a resume factory")
	}
	variadicFactory := resumeFactoryType(variadic)
	if variadicFactory.IsVariadic() || variadicFactory.NumParams() != 1 ||
		variadicFactory.Param(0).Type != values.Type {
		t.Fatalf("variadic factory type = %v, want explicit slice parameter",
			variadicFactory)
	}
	shapeType := types.NewSignature(nil, nil, nil)
	shapeType.SetHasShape(true)
	shape := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("Shape"), shapeType)
	if resumeFactorySupported(shape) {
		t.Fatal("generic shape supports a resume factory")
	}
	if _, err := newLowerCandidate(&Plan{}, &Function{Func: shape}); err == nil ||
		!strings.Contains(err.Error(), "generic shape") {
		t.Fatalf("newLowerCandidate error = %v, want generic-shape rejection",
			err)
	}
}

func TestLowerMethodReceiver(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/methodreceiver", "methodreceiver")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	yield := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("yield"),
		types.NewSignature(nil, nil, nil))
	yield.DeclareParams(true)

	recv := types.NewField(src.NoXPos, pkg.Lookup("receiver"),
		types.NewPtr(types.Types[types.TINT]))
	param := types.NewField(src.NoXPos, pkg.Lookup("values"),
		types.NewSlice(types.Types[types.TINT]))
	param.SetIsDDD(true)
	method := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("Method"),
		types.NewSignature(recv, []*types.Field{param}, nil))
	method.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, method)

	receiverName := recv.Nname.(*ir.Name)
	paramName := param.Nname.(*ir.Name)
	receiverCopy := method.NewLocal(src.NoXPos, pkg.Lookup("receiverCopy"),
		recv.Type)
	paramCopy := method.NewLocal(src.NoXPos, pkg.Lookup("paramCopy"),
		param.Type)
	copyReceiver := ir.NewAssignStmt(src.NoXPos, receiverCopy, receiverName)
	copyReceiver.SetTypecheck(1)
	copyParam := ir.NewAssignStmt(src.NoXPos, paramCopy, paramName)
	copyParam.SetTypecheck(1)
	yieldCall := newLowerTestCall(yield)
	method.Body = ir.Nodes{
		yieldCall, copyReceiver, copyParam, newLowerTestReturn(),
	}

	function := &Function{
		Func:    method,
		Local:   MaySuspend,
		Effect:  MaySuspend,
		Primary: CoroPrimary,
		Sites: []Site{{
			ID: 1, Kind: SiteYield, Node: yieldCall,
		}},
	}
	result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{
		method: function,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Lowered != 1 || result.Skipped != 0 ||
		function.Factory != FactoryABI1 {
		t.Fatalf("Lower result = %+v, factory = %v", result, function.Factory)
	}

	wantFactory := symbolName(method.Nname) + ".coro"
	var factoryCall *ir.CallExpr
	ir.Visit(method, func(node ir.Node) {
		call, ok := node.(*ir.CallExpr)
		if !ok {
			return
		}
		if got := symbolName(ir.StaticCalleeName(ir.StaticValue(call.Fun))); got == wantFactory {
			factoryCall = call
		}
	})
	if factoryCall == nil {
		t.Fatalf("method wrapper does not call %s", wantFactory)
	}
	if factoryCall.Fun.Type().IsVariadic() {
		t.Fatalf("method factory call remains variadic: %v", factoryCall.Fun.Type())
	}
	if len(factoryCall.Args) != 2 || factoryCall.Args[0] != receiverName ||
		factoryCall.Args[1] != paramName {
		t.Fatalf("method factory arguments = %v, want receiver and parameter",
			factoryCall.Args)
	}
}

func TestNeedsTerminalEntry(t *testing.T) {
	prepareLowerTest(t)

	pkg := types.NewPkg("example.com/coro/terminalentry", "terminalentry")
	sequence := 0
	candidate := func(body ...ir.Node) *lowerCandidate {
		sequence++
		fn := ir.NewFunc(src.NoXPos, src.NoXPos,
			pkg.LookupNum("candidate", sequence),
			types.NewSignature(nil, nil, nil))
		fn.Body = body
		return &lowerCandidate{
			function:     &Function{Func: fn},
			transitions:  make(map[*ir.CallExpr]SiteKind),
			foreignCalls: make(map[*ir.CallExpr]ForeignCallClass),
		}
	}
	check := func(name string, candidate *lowerCandidate, want bool) {
		t.Helper()
		if got := needsTerminalEntry(candidate); got != want {
			t.Errorf("%s: needsTerminalEntry = %v, want %v", name, got, want)
		}
	}
	intValue := func(value int64) ir.Node {
		return ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(value))
	}
	name := func(symbol string, typ *types.Type) *ir.Name {
		return ir.NewNameAt(src.NoXPos, pkg.Lookup(symbol), typ)
	}

	terminal := candidate()
	terminal.function.Terminal = MayPanic
	check("terminal effect", terminal, true)

	deferred := candidate()
	deferred.defers = []*lowerDefer{{}}
	check("defer", deferred, true)

	awaited := candidate()
	awaited.transitions[nil] = SiteAwait
	check("await", awaited, true)

	exiting := candidate()
	exiting.transitions[nil] = SiteGoexit
	check("goexit", exiting, true)

	receiving := candidate()
	receiving.channels = map[ir.Node]*lowerChannel{
		nil: {},
	}
	check("channel receive", receiving, false)

	sending := candidate()
	sending.channels = map[ir.Node]*lowerChannel{
		nil: {sendValue: ir.NewInt(src.NoXPos, 1)},
	}
	check("channel send", sending, true)

	callTarget := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("callTarget"),
		types.NewSignature(nil, nil, nil))
	newCall := func() *ir.CallExpr {
		return ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
			callTarget.Nname, nil)
	}
	yieldCall := newCall()
	yieldOnly := candidate(yieldCall)
	yieldOnly.transitions[yieldCall] = SiteYield
	check("yield call", yieldOnly, false)

	foreignCall := newCall()
	foreignOnly := candidate(foreignCall)
	foreignOnly.foreignCalls[foreignCall] = DirectNoBlock
	check("foreign call", foreignOnly, false)

	check("ordinary call", candidate(newCall()), true)

	nilComparison := ir.NewBinaryExpr(src.NoXPos, ir.OEQ,
		ir.NewNilExpr(src.NoXPos, types.NewPtr(types.Types[types.TINT])),
		ir.NewNilExpr(src.NoXPos, types.NewPtr(types.Types[types.TINT])))
	check("nil comparison", candidate(nilComparison), false)

	missingTypeComparison := ir.NewBinaryExpr(src.NoXPos, ir.OEQ,
		name("missingTypeLeft", nil), name("missingTypeRight", nil))
	check("missing comparison type", candidate(missingTypeComparison), true)

	orderedComparison := ir.NewBinaryExpr(src.NoXPos, ir.OLT,
		intValue(1), intValue(2))
	check("ordered comparison", candidate(orderedComparison), false)

	scalarComparison := ir.NewBinaryExpr(src.NoXPos, ir.OEQ,
		intValue(1), intValue(2))
	check("scalar equality", candidate(scalarComparison), false)

	interfaceType := types.NewInterface(nil)
	interfaceComparison := ir.NewBinaryExpr(src.NoXPos, ir.OEQ,
		name("interfaceLeft", interfaceType),
		name("interfaceRight", interfaceType))
	check("interface equality", candidate(interfaceComparison), true)

	add := ir.NewAssignOpStmt(src.NoXPos, ir.OADD,
		name("add", types.Types[types.TINT]), intValue(1))
	check("add assignment", candidate(add), false)

	stringAdd := ir.NewAssignOpStmt(src.NoXPos, ir.OADD,
		name("stringAdd", types.Types[types.TSTRING]),
		ir.NewBasicLit(src.NoXPos, types.Types[types.TSTRING],
			constant.MakeString("x")))
	check("string add assignment", candidate(stringAdd), true)

	shift := ir.NewAssignOpStmt(src.NoXPos, ir.OLSH,
		name("shift", types.Types[types.TINT]), intValue(1))
	check("shift assignment", candidate(shift), true)

	binaryAdd := ir.NewBinaryExpr(src.NoXPos, ir.OADD,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TSTRING],
			constant.MakeString("x")),
		ir.NewBasicLit(src.NoXPos, types.Types[types.TSTRING],
			constant.MakeString("y")))
	check("string addition", candidate(binaryAdd), true)

	subtract := ir.NewBinaryExpr(src.NoXPos, ir.OSUB,
		intValue(2), intValue(1))
	check("subtraction", candidate(subtract), false)

	divide := ir.NewBinaryExpr(src.NoXPos, ir.ODIV,
		intValue(1), intValue(0))
	check("division", candidate(divide), true)

	dereference := ir.NewStarExpr(src.NoXPos,
		name("pointer", types.NewPtr(types.Types[types.TINT])))
	check("dereference", candidate(dereference), true)

	check("empty", candidate(), false)
}

func TestLowerStateMachines(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/lowertest", "lowertest")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	yield := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("yield"),
		types.NewSignature(nil, nil, nil))
	yield.DeclareParams(true)

	child := newLowerTestFunc(pkg, "child")
	local := child.NewLocal(src.NoXPos, pkg.Lookup("local"), types.Types[types.TINT])
	decl := ir.NewDecl(src.NoXPos, ir.ODCL, local)
	assign := ir.NewAssignStmt(src.NoXPos, local,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT], constant.MakeInt64(1)))
	assign.Def = true
	local.Defn = assign
	childYield := newLowerTestCall(yield)
	child.Body = []ir.Node{decl, assign, childYield, newLowerTestReturn()}

	parent := newLowerTestFunc(pkg, "parent")
	childCall := newLowerTestCall(child)
	parent.Body = []ir.Node{childCall, newLowerTestReturn()}

	spawned := newLowerTestFunc(pkg, "spawned")
	spawnedYield := newLowerTestCall(yield)
	spawned.Body = []ir.Node{spawnedYield, newLowerTestReturn()}

	spawner := newLowerTestFunc(pkg, "spawner")
	spawnCall := newLowerTestCall(spawned)
	goSpawn := ir.NewGoDeferStmt(src.NoXPos, ir.OGO, spawnCall)
	goSpawn.SetTypecheck(1)
	spawnerYield := newLowerTestCall(yield)
	spawner.Body = []ir.Node{goSpawn, spawnerYield, newLowerTestReturn()}

	timerParam := types.NewField(src.NoXPos, pkg.Lookup("ns"), types.Types[types.TINT64])
	timerOp := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("timerOp"),
		types.NewSignature(nil, []*types.Field{timerParam}, nil))
	timerOp.DeclareParams(true)
	timerCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, timerOp.Nname,
		[]ir.Node{ir.NewBasicLit(src.NoXPos, types.Types[types.TINT64],
			constant.MakeInt64(1))})
	timerCall.SetTypecheck(1)
	sleeper := newLowerTestFunc(pkg, "sleeper")
	sleeper.Body = []ir.Node{timerCall, newLowerTestReturn()}

	newReadCall := func(name string) *ir.CallExpr {
		readParams := []*types.Field{
			types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
			types.NewField(src.NoXPos, nil, types.NewSlice(types.ByteType)),
			types.NewField(src.NoXPos, nil, types.NewPtr(types.Types[types.TINT])),
			types.NewField(src.NoXPos, nil, types.NewPtr(types.Types[types.TUINTPTR])),
		}
		op := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name),
			types.NewSignature(nil, readParams, nil))
		op.DeclareParams(true)
		call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, op.Nname, []ir.Node{
			ir.NewBasicLit(src.NoXPos, types.Types[types.TINT], constant.MakeInt64(1)),
			ir.NewNilExpr(src.NoXPos, types.NewSlice(types.ByteType)),
			ir.NewNilExpr(src.NoXPos, types.NewPtr(types.Types[types.TINT])),
			ir.NewNilExpr(src.NoXPos, types.NewPtr(types.Types[types.TUINTPTR])),
		})
		call.SetTypecheck(1)
		return call
	}
	fileCall := newReadCall("fileOp")
	fileReader := newLowerTestFunc(pkg, "fileReader")
	fileReader.Body = []ir.Node{fileCall, newLowerTestReturn()}
	socketCall := newReadCall("socketOp")
	socketReader := newLowerTestFunc(pkg, "socketReader")
	socketReader.Body = []ir.Node{socketCall, newLowerTestReturn()}

	osPkg := types.NewPkg("os", "os")
	ordinaryReadResults := []*types.Field{
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
		types.NewField(src.NoXPos, nil, types.ErrorType),
	}
	ordinaryRead := ir.NewFunc(src.NoXPos, src.NoXPos,
		osPkg.Lookup("(*File).Read"),
		types.NewSignature(nil, nil, ordinaryReadResults))
	ordinaryRead.DeclareParams(true)
	ordinaryReadCall := newLowerTestCall(ordinaryRead)
	ordinaryReadCall.SetType(ordinaryRead.Type().ResultsTuple())
	ordinaryFileReader := newLowerTestFunc(pkg, "ordinaryFileReader")
	ordinaryReadN := ordinaryFileReader.NewLocal(src.NoXPos,
		pkg.Lookup("ordinaryReadN"), types.Types[types.TINT])
	ordinaryReadErr := ordinaryFileReader.NewLocal(src.NoXPos,
		pkg.Lookup("ordinaryReadErr"), types.ErrorType)
	ordinaryReadAssign := ir.NewAssignListStmt(src.NoXPos,
		ir.OAS2FUNC, ir.Nodes{ordinaryReadN, ordinaryReadErr},
		ir.Nodes{ordinaryReadCall})
	ordinaryReadAssign.SetTypecheck(1)
	ordinaryFileReader.Body = []ir.Node{
		ir.NewDecl(src.NoXPos, ir.ODCL, ordinaryReadN),
		ir.NewDecl(src.NoXPos, ir.ODCL, ordinaryReadErr),
		ordinaryReadAssign,
		newLowerTestReturn(),
	}

	asyncParams := []*types.Field{
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
		types.NewField(src.NoXPos, nil, types.Types[types.TUINT64]),
		types.NewField(src.NoXPos, nil, types.NewPtr(types.Types[types.TUINT64])),
		types.NewField(src.NoXPos, nil, types.NewPtr(types.Types[types.TUINTPTR])),
	}
	asyncOp := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("asyncOp"),
		types.NewSignature(nil, asyncParams, nil))
	asyncOp.DeclareParams(true)
	asyncCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, asyncOp.Nname, []ir.Node{
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT], constant.MakeInt64(1)),
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT], constant.MakeInt64(2)),
		ir.NewBasicLit(src.NoXPos, types.Types[types.TUINT64], constant.MakeInt64(21)),
		ir.NewNilExpr(src.NoXPos, types.NewPtr(types.Types[types.TUINT64])),
		ir.NewNilExpr(src.NoXPos, types.NewPtr(types.Types[types.TUINTPTR])),
	})
	asyncCall.SetTypecheck(1)
	asyncCaller := newLowerTestFunc(pkg, "asyncCaller")
	asyncCaller.Body = []ir.Node{asyncCall, newLowerTestReturn()}

	directParam := types.NewField(src.NoXPos, nil, types.Types[types.TINT])
	directType := types.NewSignature(nil, []*types.Field{directParam}, nil)
	directOp := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("directOp"),
		directType)
	directOp.DeclareParams(true)
	directEntry := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("directEntry"),
		directType)
	directEntry.DeclareParams(true)
	directValueResult := types.NewField(src.NoXPos, nil, types.Types[types.TINT])
	directValueType := types.NewSignature(nil, []*types.Field{directParam},
		[]*types.Field{directValueResult})
	directValueOp := ir.NewFunc(src.NoXPos, src.NoXPos,
		pkg.Lookup("directValueOp"), directValueType)
	directValueOp.DeclareParams(true)
	directValueEntry := ir.NewFunc(src.NoXPos, src.NoXPos,
		pkg.Lookup("directValueEntry"), directValueType)
	directValueEntry.DeclareParams(true)
	argumentResult := types.NewField(src.NoXPos, nil, types.Types[types.TINT])
	argument := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("argument"),
		types.NewSignature(nil, nil, []*types.Field{argumentResult}))
	argument.DeclareParams(true)
	argumentCall := newLowerTestCall(argument)
	argumentCall.SetType(types.Types[types.TINT])
	directCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, directOp.Nname,
		ir.Nodes{argumentCall})
	directCall.SetTypecheck(1)
	blockOp := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("blockOp"),
		types.NewSignature(nil, nil, nil))
	blockOp.DeclareParams(true)
	blockCall := newLowerTestCall(blockOp)
	foreignYield := newLowerTestCall(yield)
	foreignCaller := newLowerTestFunc(pkg, "foreignCaller")
	foreignCaller.Body = []ir.Node{
		directCall, blockCall, foreignYield, newLowerTestReturn(),
	}

	runDirectCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, directOp.Nname,
		ir.Nodes{ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(1))})
	runDirectCall.SetTypecheck(1)
	runLoop := ir.NewForStmt(src.NoXPos, nil,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TBOOL],
			constant.MakeBool(false)),
		nil, ir.Nodes{runDirectCall}, false)
	runLoop.SetTypecheck(1)
	runToCompletion := newLowerTestFunc(pkg, "runToCompletion")
	runToCompletion.Body = ir.Nodes{runLoop, newLowerTestReturn()}

	runFaultingCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
		directOp.Nname, ir.Nodes{
			ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
				constant.MakeInt64(1)),
		})
	runFaultingCall.SetTypecheck(1)
	runFaulting := newLowerTestFunc(pkg, "runFaulting")
	runFaultingPointer := runFaulting.NewLocal(src.NoXPos,
		pkg.Lookup("runFaultingPointer"),
		types.NewPtr(types.Types[types.TINT]))
	runFaultingValue := runFaulting.NewLocal(src.NoXPos,
		pkg.Lookup("runFaultingValue"), types.Types[types.TINT])
	runFaultingLoad := ir.NewAssignStmt(src.NoXPos, runFaultingValue,
		ir.NewStarExpr(src.NoXPos, runFaultingPointer))
	runFaulting.Body = ir.Nodes{
		ir.NewDecl(src.NoXPos, ir.ODCL, runFaultingPointer),
		ir.NewDecl(src.NoXPos, ir.ODCL, runFaultingValue),
		runFaultingLoad,
		runFaultingCall,
		newLowerTestReturn(),
	}

	singleResult := types.NewField(src.NoXPos, pkg.Lookup("singleResult"),
		types.Types[types.TINT])
	runSingle := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("runSingle"),
		types.NewSignature(nil, nil, []*types.Field{singleResult}))
	runSingle.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, runSingle)
	singleValue := runSingle.NewLocal(src.NoXPos, pkg.Lookup("singleValue"),
		types.Types[types.TINT])
	singleDecl := ir.NewDecl(src.NoXPos, ir.ODCL, singleValue)
	singleCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
		directValueOp.Nname, ir.Nodes{
			ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
				constant.MakeInt64(1)),
		})
	singleCall.SetType(types.Types[types.TINT])
	singleCall.SetTypecheck(1)
	singleAssign := ir.NewAssignStmt(src.NoXPos, singleValue, singleCall)
	singleAssign.Def = true
	singleValue.Defn = singleAssign
	singleReturn := ir.NewReturnStmt(src.NoXPos, ir.Nodes{singleValue})
	singleReturn.SetTypecheck(1)
	runSingle.Body = ir.Nodes{singleDecl, singleAssign, singleReturn}

	structuredResults := []*types.Field{
		types.NewField(src.NoXPos, pkg.Lookup("first"), types.Types[types.TINT]),
		types.NewField(src.NoXPos, pkg.Lookup("second"), types.Types[types.TINT]),
	}
	runStructured := ir.NewFunc(src.NoXPos, src.NoXPos,
		pkg.Lookup("runStructured"),
		types.NewSignature(nil, nil, structuredResults))
	runStructured.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, runStructured)
	runValue := runStructured.NewLocal(src.NoXPos, pkg.Lookup("runValue"),
		types.Types[types.TINT])
	runValueDecl := ir.NewDecl(src.NoXPos, ir.ODCL, runValue)
	runValueInit := ir.NewAssignStmt(src.NoXPos, runValue,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(0)))
	runValueInit.Def = true
	runValue.Defn = runValueInit
	runValueCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
		directValueOp.Nname, ir.Nodes{
			ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
				constant.MakeInt64(2)),
		})
	runValueCall.SetType(types.Types[types.TINT])
	runValueCall.SetTypecheck(1)
	runValueAssign := ir.NewAssignStmt(src.NoXPos, runValue, runValueCall)
	runElseAssign := ir.NewAssignStmt(src.NoXPos, runValue,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(3)))
	runIf := ir.NewIfStmt(src.NoXPos,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TBOOL],
			constant.MakeBool(true)),
		ir.Nodes{runValueAssign}, ir.Nodes{runElseAssign})
	runIf.SetInit(ir.Nodes{runValueInit})
	runIf.SetTypecheck(1)
	runBlock := ir.NewBlockStmt(src.NoXPos, ir.Nodes{runIf})
	runReturn := ir.NewReturnStmt(src.NoXPos, ir.Nodes{
		runValue,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(4)),
	})
	runReturn.SetTypecheck(1)
	runStructured.Body = ir.Nodes{runValueDecl, runBlock, runReturn}

	structured := newLowerTestFunc(pkg, "structured")
	loopVar := structured.NewLocal(src.NoXPos, pkg.Lookup("loop"), types.Types[types.TINT])
	loopDecl := ir.NewDecl(src.NoXPos, ir.ODCL, loopVar)
	loopInit := ir.NewAssignStmt(src.NoXPos, loopVar,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT], constant.MakeInt64(0)))
	loopInit.Def = true
	loopVar.Defn = loopInit
	thenYield := newLowerTestCall(yield)
	elseYield := newLowerTestCall(yield)
	ifStmt := ir.NewIfStmt(src.NoXPos,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TBOOL], constant.MakeBool(true)),
		ir.Nodes{thenYield, newLowerTestReturn()}, ir.Nodes{elseYield})
	ifStmt.SetTypecheck(1)
	structuredTimer := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, timerOp.Nname,
		[]ir.Node{ir.NewBasicLit(src.NoXPos, types.Types[types.TINT64],
			constant.MakeInt64(1))})
	structuredTimer.SetTypecheck(1)
	loopPost := ir.NewAssignOpStmt(src.NoXPos, ir.OADD, loopVar,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT], constant.MakeInt64(1)))
	loopPost.IncDec = true
	loopPost.SetTypecheck(1)
	loop := ir.NewForStmt(src.NoXPos, loopInit,
		nil,
		loopPost, ir.Nodes{structuredTimer}, false)
	loop.SetTypecheck(1)
	structured.Body = []ir.Node{
		loopDecl,
		ir.NewBlockStmt(src.NoXPos, ir.Nodes{ifStmt}),
		loop,
		newLowerTestReturn(),
	}

	cleanupParam := types.NewField(src.NoXPos, pkg.Lookup("value"),
		types.Types[types.TINT])
	cleanup := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("cleanup"),
		types.NewSignature(nil, []*types.Field{cleanupParam}, nil))
	cleanup.DeclareParams(true)
	deferred := newLowerTestFunc(pkg, "deferred")
	deferValue := deferred.NewLocal(src.NoXPos, pkg.Lookup("deferValue"),
		types.Types[types.TINT])
	deferDecl := ir.NewDecl(src.NoXPos, ir.ODCL, deferValue)
	deferAssign := ir.NewAssignStmt(src.NoXPos, deferValue,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(42)))
	deferAssign.Def = true
	deferValue.Defn = deferAssign
	deferWrapper := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.ODEFER,
		types.NewSignature(nil, nil, nil), deferred, typecheck.Target, 0)
	deferWrapper.DeclareParams(true)
	deferWrapper.SetWrapper(true)
	deferWrapper.WrappedFunc = cleanup
	deferCapture := ir.NewClosureVar(src.NoXPos, deferWrapper, deferValue)
	deferCleanupCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
		cleanup.Nname, ir.Nodes{deferCapture})
	deferCleanupCall.SetTypecheck(1)
	deferWrapper.Body = ir.Nodes{deferCleanupCall}
	deferCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
		deferWrapper.OClosure, nil)
	deferCall.SetTypecheck(1)
	deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, deferCall)
	deferStmt.SetTypecheck(1)
	deferStmt.SetInit(ir.Nodes{deferAssign})
	deferIf := ir.NewIfStmt(src.NoXPos,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TBOOL],
			constant.MakeBool(true)),
		ir.Nodes{deferStmt}, nil)
	deferIf.SetTypecheck(1)
	cleanupNoArgs := newLowerTestFunc(pkg, "cleanupNoArgs")
	deferNoArgsCall := newLowerTestCall(cleanupNoArgs)
	deferNoArgsStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER,
		deferNoArgsCall)
	deferNoArgsStmt.SetTypecheck(1)
	deferredYield := newLowerTestCall(yield)
	deferred.Body = ir.Nodes{
		deferDecl, deferIf, deferNoArgsStmt, deferredYield,
		newLowerTestReturn(),
	}

	dynamicDeferred := newLowerTestFunc(pkg, "dynamicDeferred")
	dynamicDeferValue := dynamicDeferred.NewLocal(src.NoXPos,
		pkg.Lookup("dynamicDeferValue"), types.Types[types.TINT])
	dynamicDeferDecl := ir.NewDecl(src.NoXPos, ir.ODCL, dynamicDeferValue)
	dynamicDeferAssign := ir.NewAssignStmt(src.NoXPos, dynamicDeferValue,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(7)))
	dynamicDeferAssign.Def = true
	dynamicDeferValue.Defn = dynamicDeferAssign
	dynamicDeferWrapper := ir.NewClosureFunc(src.NoXPos, src.NoXPos,
		ir.ODEFER, types.NewSignature(nil, nil, nil), dynamicDeferred,
		typecheck.Target, 0)
	dynamicDeferWrapper.DeclareParams(true)
	dynamicDeferWrapper.SetWrapper(true)
	dynamicDeferWrapper.WrappedFunc = cleanup
	dynamicDeferCapture := ir.NewClosureVar(src.NoXPos,
		dynamicDeferWrapper, dynamicDeferValue)
	dynamicCleanupCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
		cleanup.Nname, ir.Nodes{dynamicDeferCapture})
	dynamicCleanupCall.SetTypecheck(1)
	dynamicDeferWrapper.Body = ir.Nodes{dynamicCleanupCall}
	dynamicDeferCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
		dynamicDeferWrapper.OClosure, nil)
	dynamicDeferCall.SetTypecheck(1)
	dynamicDeferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER,
		dynamicDeferCall)
	dynamicDeferStmt.SetTypecheck(1)
	dynamicDeferStmt.SetInit(ir.Nodes{dynamicDeferAssign})
	dynamicYield := newLowerTestCall(yield)
	dynamicLoop := ir.NewForStmt(src.NoXPos, nil,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TBOOL],
			constant.MakeBool(false)),
		nil, ir.Nodes{dynamicDeferStmt, dynamicYield}, false)
	dynamicLoop.SetTypecheck(1)
	dynamicDeferred.Body = ir.Nodes{
		dynamicDeferDecl, dynamicLoop, newLowerTestReturn(),
	}

	repeatedParamField := types.NewField(src.NoXPos,
		pkg.Lookup("repeatedParam"), types.Types[types.TINT])
	repeatedResultField := types.NewField(src.NoXPos,
		pkg.Lookup("repeatedResult"), types.Types[types.TINT])
	repeatedDeferred := ir.NewFunc(src.NoXPos, src.NoXPos,
		pkg.Lookup("repeatedDeferred"), types.NewSignature(nil,
			[]*types.Field{repeatedParamField},
			[]*types.Field{repeatedResultField}))
	repeatedDeferred.DeclareParams(true)
	repeatedParam := repeatedParamField.Nname.(*ir.Name)
	repeatedResult := repeatedResultField.Nname.(*ir.Name)
	repeatedLocal := repeatedDeferred.NewLocal(src.NoXPos,
		pkg.Lookup("repeatedLocal"), types.Types[types.TINT])
	repeatedDecl := ir.NewDecl(src.NoXPos, ir.ODCL, repeatedLocal)
	repeatedAssign := ir.NewAssignStmt(src.NoXPos, repeatedLocal,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(7)))
	repeatedAssign.Def = true
	repeatedAssign.SetTypecheck(1)
	repeatedLocal.Defn = repeatedAssign
	repeatedInitLocal := repeatedDeferred.NewLocal(src.NoXPos,
		pkg.Lookup("repeatedInitLocal"), types.Types[types.TINT])
	repeatedInitDecl := ir.NewDecl(src.NoXPos, ir.ODCL,
		repeatedInitLocal)
	repeatedInitAssign := ir.NewAssignStmt(src.NoXPos,
		repeatedInitLocal, ir.NewBasicLit(src.NoXPos,
			types.Types[types.TINT], constant.MakeInt64(8)))
	repeatedInitAssign.Def = true
	repeatedInitAssign.SetTypecheck(1)
	repeatedInitAssign.SetInit(ir.Nodes{repeatedInitDecl})
	repeatedInitLocal.Defn = repeatedInitAssign
	repeatedImplicit := repeatedDeferred.NewLocal(src.NoXPos,
		pkg.Lookup("repeatedImplicit"), types.Types[types.TINT])
	repeatedImplicitAssign := ir.NewAssignStmt(src.NoXPos,
		repeatedImplicit, ir.NewBasicLit(src.NoXPos,
			types.Types[types.TINT], constant.MakeInt64(9)))
	repeatedImplicitAssign.Def = true
	repeatedImplicitAssign.SetTypecheck(1)
	repeatedImplicit.Defn = repeatedImplicitAssign
	repeatedListFirst := repeatedDeferred.NewLocal(src.NoXPos,
		pkg.Lookup("repeatedListFirst"), types.Types[types.TINT])
	repeatedListSecond := repeatedDeferred.NewLocal(src.NoXPos,
		pkg.Lookup("repeatedListSecond"), types.Types[types.TINT])
	repeatedListAssign := ir.NewAssignListStmt(src.NoXPos, ir.OAS2,
		ir.Nodes{repeatedListFirst, repeatedListSecond},
		ir.Nodes{
			ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
				constant.MakeInt64(10)),
			ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
				constant.MakeInt64(11)),
		})
	repeatedListAssign.Def = true
	repeatedListAssign.SetTypecheck(1)
	repeatedListFirst.Defn = repeatedListAssign
	repeatedListSecond.Defn = repeatedListAssign
	repeatedResultAssign := ir.NewAssignStmt(src.NoXPos,
		repeatedResult, repeatedParam)
	repeatedResultAssign.SetTypecheck(1)

	repeatedLiteral := ir.NewClosureFunc(src.NoXPos, src.NoXPos,
		ir.OCLOSURE, types.NewSignature(nil, nil, nil),
		repeatedDeferred, typecheck.Target, 0)
	repeatedLiteral.DeclareParams(true)
	repeatedLocalCapture := ir.NewClosureVar(src.NoXPos,
		repeatedLiteral, repeatedLocal)
	repeatedParamCapture := ir.NewClosureVar(src.NoXPos,
		repeatedLiteral, repeatedParam)
	repeatedResultCapture := ir.NewClosureVar(src.NoXPos,
		repeatedLiteral, repeatedResult)
	repeatedInitCapture := ir.NewClosureVar(src.NoXPos,
		repeatedLiteral, repeatedInitLocal)
	repeatedImplicitCapture := ir.NewClosureVar(src.NoXPos,
		repeatedLiteral, repeatedImplicit)
	repeatedListFirstCapture := ir.NewClosureVar(src.NoXPos,
		repeatedLiteral, repeatedListFirst)
	repeatedListSecondCapture := ir.NewClosureVar(src.NoXPos,
		repeatedLiteral, repeatedListSecond)
	repeatedTarget := ir.NewNameAt(src.NoXPos,
		pkg.Lookup("repeatedTarget"), types.Types[types.TINT])
	repeatedTarget.Class = ir.PEXTERN
	for _, value := range []*ir.Name{
		repeatedLocalCapture, repeatedParamCapture, repeatedResultCapture,
		repeatedInitCapture, repeatedImplicitCapture,
		repeatedListFirstCapture, repeatedListSecondCapture,
	} {
		assign := ir.NewAssignStmt(src.NoXPos, repeatedTarget, value)
		assign.SetTypecheck(1)
		repeatedLiteral.Body = append(repeatedLiteral.Body, assign)
	}
	repeatedDeferCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
		repeatedLiteral.OClosure, nil)
	repeatedDeferCall.SetTypecheck(1)
	repeatedDeferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER,
		repeatedDeferCall)
	repeatedDeferStmt.SetTypecheck(1)
	repeatedSecondLiteral := ir.NewClosureFunc(src.NoXPos, src.NoXPos,
		ir.OCLOSURE, types.NewSignature(nil, nil, nil),
		repeatedDeferred, typecheck.Target, 0)
	repeatedSecondLiteral.DeclareParams(true)
	repeatedSecondCapture := ir.NewClosureVar(src.NoXPos,
		repeatedSecondLiteral, repeatedLocal)
	repeatedSecondStore := ir.NewAssignStmt(src.NoXPos,
		repeatedTarget, repeatedSecondCapture)
	repeatedSecondStore.SetTypecheck(1)
	repeatedSecondLiteral.Body = ir.Nodes{repeatedSecondStore}
	repeatedSecondCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
		repeatedSecondLiteral.OClosure, nil)
	repeatedSecondCall.SetTypecheck(1)
	repeatedSecondStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER,
		repeatedSecondCall)
	repeatedSecondStmt.SetTypecheck(1)
	repeatedYield := newLowerTestCall(yield)
	repeatedLoop := ir.NewForStmt(src.NoXPos, nil,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TBOOL],
			constant.MakeBool(false)),
		nil, ir.Nodes{
			repeatedDecl, repeatedAssign, repeatedInitAssign,
			repeatedImplicitAssign, repeatedListAssign,
			repeatedDeferStmt, repeatedSecondStmt, repeatedYield,
		}, false)
	repeatedLoop.SetTypecheck(1)
	repeatedDeferred.Body = ir.Nodes{
		repeatedResultAssign, repeatedLoop, newLowerTestReturn(),
	}

	repeatedRead := newLowerTestFunc(pkg, "repeatedRead")
	repeatedReadN := repeatedRead.NewLocal(src.NoXPos,
		pkg.Lookup("repeatedReadN"), types.Types[types.TINT])
	repeatedReadErr := repeatedRead.NewLocal(src.NoXPos,
		pkg.Lookup("repeatedReadErr"), types.ErrorType)
	repeatedReadNDecl := ir.NewDecl(src.NoXPos, ir.ODCL, repeatedReadN)
	repeatedReadErrDecl := ir.NewDecl(src.NoXPos, ir.ODCL,
		repeatedReadErr)
	repeatedReadCall := newLowerTestCall(ordinaryRead)
	repeatedReadCall.SetType(ordinaryRead.Type().ResultsTuple())
	repeatedReadAssign := ir.NewAssignListStmt(src.NoXPos,
		ir.OAS2FUNC, ir.Nodes{repeatedReadN, repeatedReadErr},
		ir.Nodes{repeatedReadCall})
	repeatedReadAssign.SetTypecheck(1)
	repeatedReadLiteral := ir.NewClosureFunc(src.NoXPos, src.NoXPos,
		ir.OCLOSURE, types.NewSignature(nil, nil, nil),
		repeatedRead, typecheck.Target, 0)
	repeatedReadLiteral.DeclareParams(true)
	repeatedReadNCapture := ir.NewClosureVar(src.NoXPos,
		repeatedReadLiteral, repeatedReadN)
	repeatedReadErrCapture := ir.NewClosureVar(src.NoXPos,
		repeatedReadLiteral, repeatedReadErr)
	repeatedReadNStore := ir.NewAssignStmt(src.NoXPos,
		repeatedTarget, repeatedReadNCapture)
	repeatedReadNStore.SetTypecheck(1)
	repeatedReadErrStore := ir.NewAssignStmt(src.NoXPos,
		ir.BlankNode, repeatedReadErrCapture)
	repeatedReadErrStore.SetTypecheck(1)
	repeatedReadLiteral.Body = ir.Nodes{
		repeatedReadNStore, repeatedReadErrStore,
	}
	repeatedReadDeferCall := ir.NewCallExpr(src.NoXPos,
		ir.OCALLFUNC, repeatedReadLiteral.OClosure, nil)
	repeatedReadDeferCall.SetTypecheck(1)
	repeatedReadDeferStmt := ir.NewGoDeferStmt(src.NoXPos,
		ir.ODEFER, repeatedReadDeferCall)
	repeatedReadDeferStmt.SetTypecheck(1)
	repeatedReadYield := newLowerTestCall(yield)
	repeatedReadLoop := ir.NewForStmt(src.NoXPos, nil,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TBOOL],
			constant.MakeBool(false)),
		nil, ir.Nodes{
			repeatedReadNDecl, repeatedReadErrDecl, repeatedReadAssign,
			repeatedReadDeferStmt, repeatedReadYield,
		}, false)
	repeatedReadLoop.SetTypecheck(1)
	repeatedRead.Body = ir.Nodes{repeatedReadLoop, newLowerTestReturn()}

	functions := map[*ir.Func]*Function{
		child: {
			Func:    child,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteYield, Node: childYield,
			}},
		},
		parent: {
			Func:    parent,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: child, CalleeName: symbolName(child.Nname),
				Node: childCall,
			}},
			Sites: []Site{{
				ID: 1, Kind: SiteAwait, Node: childCall,
			}},
		},
		spawned: {
			Func:    spawned,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteYield, Node: spawnedYield,
			}},
		},
		spawner: {
			Func:    spawner,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Edges: []Edge{
				{
					Kind: GoCall, Callee: spawned,
					CalleeName: symbolName(spawned.Nname), Node: spawnCall,
				},
			},
			Sites: []Site{
				{ID: 1, Kind: SiteSpawn, Node: spawnCall},
				{ID: 2, Kind: SiteYield, Node: spawnerYield},
			},
		},
		sleeper: {
			Func:    sleeper,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteTimer, Node: timerCall,
			}},
		},
		fileReader: {
			Func:    fileReader,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteFile, Node: fileCall,
			}},
		},
		socketReader: {
			Func:    socketReader,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SitePoll, Node: socketCall,
			}},
		},
		ordinaryFileReader: {
			Func:    ordinaryFileReader,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteFile, Node: ordinaryReadCall,
			}},
		},
		asyncCaller: {
			Func:    asyncCaller,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Exec:    NeedsSystemABI,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteForeign, Node: asyncCall,
				Foreign: AsyncOperation,
			}},
		},
		foreignCaller: {
			Func:    foreignCaller,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Exec:    NeedsSystemABI | MayBlockThread,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: directOp,
				CalleeName: symbolName(directOp.Nname),
				Recipe: OperationRecipe{
					Kind: SiteForeign, Foreign: DirectNoBlock,
					Direct: symbolName(directEntry.Nname),
				},
				Node: directCall, Direct: directEntry,
			}},
			Sites: []Site{
				{
					ID: 1, Kind: SiteForeign, Node: directCall,
					Foreign: DirectNoBlock,
				},
				{
					ID: 2, Kind: SiteForeign, Node: blockCall,
					Foreign: DirectMayBlock,
				},
				{ID: 3, Kind: SiteYield, Node: foreignYield},
			},
		},
		runToCompletion: {
			Func:    runToCompletion,
			Effect:  NoSuspend,
			Exec:    NeedsSystemABI | MayBlockThread,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: directOp,
				CalleeName: symbolName(directOp.Nname),
				Recipe: OperationRecipe{
					Kind: SiteForeign, Foreign: DirectMayBlock,
					Direct: symbolName(directEntry.Nname),
				},
				Node: runDirectCall, Direct: directEntry,
			}},
			Sites: []Site{{
				ID: 1, Kind: SiteForeign, Node: runDirectCall,
				Foreign: DirectMayBlock,
			}},
		},
		runFaulting: {
			Func:    runFaulting,
			Effect:  NoSuspend,
			Exec:    NeedsSystemABI,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: directOp,
				CalleeName: symbolName(directOp.Nname),
				Recipe: OperationRecipe{
					Kind: SiteForeign, Foreign: DirectNoBlock,
					Direct: symbolName(directEntry.Nname),
				},
				Node: runFaultingCall, Direct: directEntry,
			}},
			Sites: []Site{{
				ID: 1, Kind: SiteForeign, Node: runFaultingCall,
				Foreign: DirectNoBlock,
			}},
		},
		runSingle: {
			Func:    runSingle,
			Effect:  NoSuspend,
			Exec:    NeedsSystemABI,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: directValueOp,
				CalleeName: symbolName(directValueOp.Nname),
				Recipe: OperationRecipe{
					Kind: SiteForeign, Foreign: DirectNoBlock,
					Direct: symbolName(directValueEntry.Nname),
				},
				Node: singleCall, Direct: directValueEntry,
			}},
			Sites: []Site{{
				ID: 1, Kind: SiteForeign, Node: singleCall,
				Foreign: DirectNoBlock,
			}},
		},
		runStructured: {
			Func:    runStructured,
			Effect:  NoSuspend,
			Exec:    NeedsSystemABI,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: directValueOp,
				CalleeName: symbolName(directValueOp.Nname),
				Recipe: OperationRecipe{
					Kind: SiteForeign, Foreign: DirectNoBlock,
					Direct: symbolName(directValueEntry.Nname),
				},
				Node: runValueCall, Direct: directValueEntry,
			}},
			Sites: []Site{{
				ID: 1, Kind: SiteForeign, Node: runValueCall,
				Foreign: DirectNoBlock,
			}},
		},
		structured: {
			Func:    structured,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{
				{ID: 1, Kind: SiteYield, Node: thenYield},
				{ID: 2, Kind: SiteYield, Node: elseYield},
				{ID: 3, Kind: SiteTimer, Node: structuredTimer},
			},
		},
		deferred: {
			Func:    deferred,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Edges: []Edge{
				{
					Kind: DeferCall, Callee: deferWrapper,
					CalleeName: symbolName(deferWrapper.Nname), Node: deferCall,
				},
				{
					Kind: DeferCall, Callee: cleanupNoArgs,
					CalleeName: symbolName(cleanupNoArgs.Nname),
					Node:       deferNoArgsCall,
				},
			},
			Sites: []Site{{
				ID: 1, Kind: SiteYield, Node: deferredYield,
			}},
		},
		deferWrapper: {
			Func:    deferWrapper,
			Primary: PlainPrimary,
		},
		dynamicDeferred: {
			Func:    dynamicDeferred,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DeferCall, Callee: dynamicDeferWrapper,
				CalleeName: symbolName(dynamicDeferWrapper.Nname),
				Node:       dynamicDeferCall,
			}},
			Sites: []Site{{
				ID: 1, Kind: SiteYield, Node: dynamicYield,
			}},
		},
		dynamicDeferWrapper: {
			Func:    dynamicDeferWrapper,
			Primary: PlainPrimary,
		},
		repeatedDeferred: {
			Func:    repeatedDeferred,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Edges: []Edge{
				{
					Kind: DeferCall, Callee: repeatedLiteral,
					CalleeName: symbolName(repeatedLiteral.Nname),
					Node:       repeatedDeferCall,
				},
				{
					Kind: DeferCall, Callee: repeatedSecondLiteral,
					CalleeName: symbolName(repeatedSecondLiteral.Nname),
					Node:       repeatedSecondCall,
				},
			},
			Sites: []Site{{
				ID: 1, Kind: SiteYield, Node: repeatedYield,
			}},
		},
		repeatedLiteral: {
			Func:    repeatedLiteral,
			Primary: PlainPrimary,
		},
		repeatedSecondLiteral: {
			Func:    repeatedSecondLiteral,
			Primary: PlainPrimary,
		},
		repeatedRead: {
			Func:    repeatedRead,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DeferCall, Callee: repeatedReadLiteral,
				CalleeName: symbolName(repeatedReadLiteral.Nname),
				Node:       repeatedReadDeferCall,
			}},
			Sites: []Site{
				{ID: 1, Kind: SiteFile, Node: repeatedReadCall},
				{ID: 2, Kind: SiteYield, Node: repeatedReadYield},
			},
		},
		repeatedReadLiteral: {
			Func:    repeatedReadLiteral,
			Primary: PlainPrimary,
		},
	}
	result, err := Lower(&Plan{Functions: functions})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if result.Lowered != 19 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want 19 lowered and 0 skipped", result)
	}
	var noSplitResumes int
	for _, generated := range typecheck.Target.Funcs {
		if generated.OClosure != nil && generated.Pragma&ir.Nosplit != 0 {
			noSplitResumes++
		}
	}
	if noSplitResumes != result.Lowered {
		t.Fatalf("nosplit resume functions = %d, want %d", noSplitResumes, result.Lowered)
	}
	checksTerminal := func(fn *ir.Func) (bool, bool) {
		for _, generated := range typecheck.Target.Funcs {
			if generated.OClosure == nil || generated.ClosureParent == nil ||
				generated.ClosureParent.Sym().Name != fn.Sym().Name+".coro" {
				continue
			}
			found := false
			ir.Visit(generated, func(node ir.Node) {
				call, ok := node.(*ir.CallExpr)
				if !ok || call.Fun == nil {
					return
				}
				name := ir.StaticCalleeName(call.Fun)
				if name != nil && name.Sym() != nil &&
					name.Sym().Name == "coroTerminalAction" {
					found = true
				}
			})
			return found, true
		}
		return false, false
	}
	for _, test := range []struct {
		fn   *ir.Func
		want bool
	}{
		{child, false},
		{parent, true},
		{foreignCaller, true},
		{runToCompletion, false},
		{runFaulting, true},
		{deferred, true},
	} {
		got, found := checksTerminal(test.fn)
		if !found {
			t.Errorf("%s has no generated resume function", test.fn.Sym().Name)
		} else if got != test.want {
			t.Errorf("%s terminal check = %v, want %v",
				test.fn.Sym().Name, got, test.want)
		}
	}

	var foreignResume *ir.Func
	for _, generated := range typecheck.Target.Funcs {
		if generated.OClosure != nil && generated.ClosureParent != nil &&
			generated.ClosureParent.Sym().Name == "foreignCaller.coro" {
			foreignResume = generated
			break
		}
	}
	if foreignResume == nil {
		t.Fatal("foreign caller has no generated resume function")
	}
	var callOrder []string
	ir.Visit(foreignResume, func(node ir.Node) {
		call, ok := node.(*ir.CallExpr)
		if !ok {
			return
		}
		name := ir.StaticCalleeName(call.Fun)
		if name != nil && name.Sym() != nil {
			callOrder = append(callOrder, name.Sym().Name)
		}
	})
	callIndex := func(name string) int {
		return slices.Index(callOrder, name)
	}
	argumentIndex := callIndex("argument")
	enterIndex := callIndex("coroEnterForeign")
	directIndex := callIndex("directEntry")
	exitIndex := callIndex("coroExitForeign")
	if argumentIndex < 0 || enterIndex < 0 || directIndex < 0 || exitIndex < 0 ||
		!(argumentIndex < enterIndex && enterIndex < directIndex &&
			directIndex < exitIndex) {
		t.Errorf("foreign call order = %v, want argument, enter, direct, exit",
			callOrder)
	}
	if callIndex("directOp") >= 0 {
		t.Errorf("foreign call retained wrapper target: %v", callOrder)
	}

	var dynamicDeferResume *ir.Func
	for _, generated := range typecheck.Target.Funcs {
		if generated.OClosure != nil && generated.ClosureParent != nil &&
			generated.ClosureParent.Sym().Name == "dynamicDeferred.coro" {
			dynamicDeferResume = generated
			break
		}
	}
	if dynamicDeferResume == nil {
		t.Fatal("dynamic defer caller has no generated resume function")
	}
	var appends, clearedEntries int
	ir.Visit(dynamicDeferResume, func(node ir.Node) {
		switch node := node.(type) {
		case *ir.CallExpr:
			if node.Op() == ir.OAPPEND {
				appends++
			}
		case *ir.AssignStmt:
			if node.X.Op() == ir.OINDEX && ir.IsNil(node.Y) {
				clearedEntries++
			}
		}
		if node.Op() == ir.ODEFER {
			t.Errorf("dynamic defer resume retains ODEFER")
		}
	})
	if appends != 1 || clearedEntries != 1 {
		t.Errorf("dynamic defer resume has %d appends and %d cleared entries, want 1 and 1",
			appends, clearedEntries)
	}

	var repeatedFactory, repeatedResume *ir.Func
	for _, generated := range typecheck.Target.Funcs {
		if generated.Sym().Name == "repeatedDeferred.coro" {
			repeatedFactory = generated
		}
		if generated.OClosure != nil && generated.ClosureParent != nil &&
			generated.ClosureParent.Sym().Name == "repeatedDeferred.coro" {
			repeatedResume = generated
		}
	}
	if repeatedFactory == nil || repeatedResume == nil {
		t.Fatalf("repeated source defer generated factory=%v resume=%v",
			repeatedFactory, repeatedResume)
	}
	countNew := func(fn *ir.Func) int {
		count := 0
		ir.Visit(fn, func(node ir.Node) {
			if node.Op() == ir.ONEW {
				count++
			}
		})
		return count
	}
	if got := countNew(repeatedFactory); got != 2 {
		t.Errorf("repeated source defer factory allocations = %d, want 2", got)
	}
	if got := countNew(repeatedResume); got != 5 {
		t.Errorf("repeated source defer resume allocations = %d, want 5", got)
	}
	if len(repeatedLiteral.ClosureVars) != 7 {
		t.Fatalf("repeated literal captures = %d, want 7",
			len(repeatedLiteral.ClosureVars))
	}
	for _, capture := range repeatedLiteral.ClosureVars {
		if capture.Type() == nil || !capture.Type().IsPtr() ||
			capture.Type().Elem() != types.Types[types.TINT] {
			t.Errorf("repeated literal capture type = %v, want *int",
				capture.Type())
		}
	}
	if len(repeatedSecondLiteral.ClosureVars) != 1 ||
		!repeatedSecondLiteral.ClosureVars[0].Type().IsPtr() {
		t.Errorf("second repeated literal captures = %v, want one pointer",
			repeatedSecondLiteral.ClosureVars)
	}

	var repeatedReadResume *ir.Func
	for _, generated := range typecheck.Target.Funcs {
		if generated.OClosure != nil && generated.ClosureParent != nil &&
			generated.ClosureParent.Sym().Name == "repeatedRead.coro" {
			repeatedReadResume = generated
			break
		}
	}
	if repeatedReadResume == nil {
		t.Fatal("repeated read has no generated resume function")
	}
	var readResultStores int
	for _, generated := range typecheck.Target.Funcs {
		if generated.ClosureParent != repeatedReadResume {
			continue
		}
		ir.Visit(generated, func(node ir.Node) {
			assign, ok := node.(*ir.AssignListStmt)
			if !ok {
				return
			}
			for _, target := range assign.Lhs {
				if _, ok := target.(*ir.StarExpr); ok {
					readResultStores++
				}
			}
		})
	}
	if readResultStores != 2 {
		t.Errorf("repeated read pointer result stores = %d, want 2",
			readResultStores)
	}

	var runResume *ir.Func
	for _, generated := range typecheck.Target.Funcs {
		if generated.OClosure != nil && generated.ClosureParent != nil &&
			generated.ClosureParent.Sym().Name == "runToCompletion.coro" {
			runResume = generated
			break
		}
	}
	if runResume == nil {
		t.Fatal("run-to-completion caller has no generated resume function")
	}
	var runCalls []string
	var runLoops, runSwitches int
	ir.Visit(runResume, func(node ir.Node) {
		switch node := node.(type) {
		case *ir.ForStmt:
			runLoops++
		case *ir.SwitchStmt:
			runSwitches++
		case *ir.CallExpr:
			name := ir.StaticCalleeName(node.Fun)
			if name != nil && name.Sym() != nil {
				runCalls = append(runCalls, name.Sym().Name)
			}
		}
	})
	if runLoops != 1 || runSwitches != 0 {
		t.Errorf("run-to-completion control flow has %d loops and %d switches, want 1 and 0",
			runLoops, runSwitches)
	}
	if want := []string{"coroEnterBlocking", "directEntry", "coroExitBlocking"}; !slices.Equal(runCalls, want) {
		t.Errorf("run-to-completion calls = %v, want %v", runCalls, want)
	}

	for _, test := range []struct {
		name       string
		wantIf     bool
		wantResult int
	}{
		{"runSingle", false, 1},
		{"runStructured", true, 2},
	} {
		var resume *ir.Func
		for _, generated := range typecheck.Target.Funcs {
			if generated.OClosure != nil && generated.ClosureParent != nil &&
				generated.ClosureParent.Sym().Name == test.name+".coro" {
				resume = generated
				break
			}
		}
		if resume == nil {
			t.Errorf("%s has no generated resume function", test.name)
			continue
		}
		var calls []string
		var ifs, blocks, switches, resultStores int
		ir.Visit(resume, func(node ir.Node) {
			switch node := node.(type) {
			case *ir.IfStmt:
				ifs++
			case *ir.BlockStmt:
				blocks++
			case *ir.SwitchStmt:
				switches++
			case *ir.AssignStmt:
				if _, ok := node.X.(*ir.StarExpr); ok {
					resultStores++
				}
			case *ir.CallExpr:
				name := ir.StaticCalleeName(node.Fun)
				if name != nil && name.Sym() != nil {
					calls = append(calls, name.Sym().Name)
				}
			}
		})
		wantCalls := []string{
			"coroEnterForeign", "directValueEntry", "coroExitForeign",
		}
		if !slices.Equal(calls, wantCalls) {
			t.Errorf("%s calls = %v, want %v", test.name, calls, wantCalls)
		}
		if switches != 0 {
			t.Errorf("%s has %d switches, want none", test.name, switches)
		}
		if test.wantIf && (ifs == 0 || blocks == 0) {
			t.Errorf("%s has %d ifs and %d blocks, want structured control",
				test.name, ifs, blocks)
		}
		// Both an explicit return and the implicit fallthrough completion copy
		// every result to the wrapper-owned slots.
		if want := 2 * test.wantResult; resultStores != want {
			t.Errorf("%s result stores = %d, want %d",
				test.name, resultStores, want)
		}
	}

	for _, fn := range []*ir.Func{
		child, parent, spawned, spawner, sleeper, fileReader, socketReader,
		ordinaryFileReader, asyncCaller, foreignCaller, runToCompletion,
		runSingle, runStructured, structured, deferred, dynamicDeferred,
		repeatedDeferred, repeatedRead,
	} {
		if len(fn.Body) != 2 {
			t.Errorf("%s body has %d statements, want 2", fn.Sym().Name, len(fn.Body))
			continue
		}
		call, ok := fn.Body[0].(*ir.CallExpr)
		if !ok {
			t.Errorf("%s first statement is %T, want call", fn.Sym().Name, fn.Body[0])
			continue
		}
		callee := ir.StaticCalleeName(call.Fun)
		if callee == nil || callee.Sym().Pkg != ir.Pkgs.Runtime ||
			callee.Sym().Name != "coroRun" {
			t.Errorf("%s wrapper calls %v, want runtime.coroRun",
				fn.Sym().Name, callee)
		}
		if fn.Inl != nil {
			t.Errorf("%s retained stale inline body", fn.Sym().Name)
		}
		hasFactory := false
		for _, generated := range typecheck.Target.Funcs {
			if generated.Sym().Name == fn.Sym().Name+".coro" {
				hasFactory = true
				break
			}
		}
		if !hasFactory {
			t.Errorf("%s has no resume factory", fn.Sym().Name)
		}
	}
}

func TestCanLowerRunToCompletion(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/runpredicate", "runpredicate")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	tests := []struct {
		name   string
		change func(*lowerCandidate, *ir.CallExpr)
		want   bool
	}{
		{"direct no block", func(*lowerCandidate, *ir.CallExpr) {}, true},
		{"direct may block", func(candidate *lowerCandidate, call *ir.CallExpr) {
			candidate.function.Sites[0].Foreign = DirectMayBlock
			candidate.foreignCalls[call] = DirectMayBlock
		}, true},
		{"may suspend", func(candidate *lowerCandidate, _ *ir.CallExpr) {
			candidate.function.Effect = MaySuspend
		}, false},
		{"defer", func(candidate *lowerCandidate, _ *ir.CallExpr) {
			candidate.defers = []*lowerDefer{{}}
		}, false},
		{"terminal", func(candidate *lowerCandidate, _ *ir.CallExpr) {
			candidate.function.Terminal = MayPanic
		}, false},
		{"no foreign call", func(candidate *lowerCandidate, call *ir.CallExpr) {
			delete(candidate.foreignCalls, call)
		}, false},
		{"non-foreign site", func(candidate *lowerCandidate, _ *ir.CallExpr) {
			candidate.function.Sites[0].Kind = SiteYield
		}, false},
		{"async foreign site", func(candidate *lowerCandidate, call *ir.CallExpr) {
			candidate.function.Sites[0].Foreign = AsyncOperation
			candidate.foreignCalls[call] = AsyncOperation
		}, false},
		{"foreign call in for post", func(candidate *lowerCandidate, call *ir.CallExpr) {
			loop := ir.NewForStmt(src.NoXPos, nil, nil, call, nil, false)
			candidate.function.Func.Body = ir.Nodes{loop, newLowerTestReturn()}
		}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := newLowerTestFunc(pkg, "target."+strings.ReplaceAll(test.name, " ", "."))
			call := newLowerTestCall(target)
			fn := newLowerTestFunc(pkg, "caller."+strings.ReplaceAll(test.name, " ", "."))
			fn.Body = ir.Nodes{call, newLowerTestReturn()}
			candidate := &lowerCandidate{
				function: &Function{
					Func:   fn,
					Effect: NoSuspend,
					Sites: []Site{{
						ID: 1, Kind: SiteForeign, Node: call,
						Foreign: DirectNoBlock,
					}},
				},
				foreignCalls: map[*ir.CallExpr]ForeignCallClass{
					call: DirectNoBlock,
				},
			}
			test.change(candidate, call)
			if got := canLowerRunToCompletion(candidate); got != test.want {
				t.Errorf("canLowerRunToCompletion = %t, want %t", got, test.want)
			}
		})
	}
}

func TestLowerPanicOutcome(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/lowerpanic", "lowerpanic")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	yield := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("yield"),
		types.NewSignature(nil, nil, nil))
	yield.DeclareParams(true)
	yieldCall := newLowerTestCall(yield)
	panicStmt := ir.NewUnaryExpr(src.NoXPos, ir.OPANIC,
		ir.NewNilExpr(src.NoXPos, types.Types[types.TINTER]))
	panicStmt.SetTypecheck(1)
	panicIf := ir.NewIfStmt(src.NoXPos,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TBOOL],
			constant.MakeBool(true)),
		ir.Nodes{panicStmt}, nil)
	panicIf.SetTypecheck(1)
	fn := newLowerTestFunc(pkg, "panicking")
	fn.Body = ir.Nodes{yieldCall, panicIf}
	function := &Function{
		Func:          fn,
		Local:         MaySuspend,
		Effect:        MaySuspend,
		LocalTerminal: MayPanic,
		Terminal:      MayPanic,
		Primary:       CoroPrimary,
		Sites: []Site{
			{ID: 1, Kind: SiteYield, Node: yieldCall},
			{ID: 2, Kind: SitePanic, Node: panicStmt},
		},
	}
	result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{
		fn: function,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Lowered != 1 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want one lowered function", result)
	}

	var resume *ir.Func
	for _, generated := range typecheck.Target.Funcs {
		if generated.OClosure != nil && generated.ClosureParent != nil &&
			generated.ClosureParent.Sym().Name == "panicking.coro" {
			resume = generated
			break
		}
	}
	if resume == nil {
		t.Fatal("panicking function has no generated resume function")
	}
	var calls []string
	ir.Visit(resume, func(node ir.Node) {
		call, ok := node.(*ir.CallExpr)
		if !ok {
			return
		}
		name := ir.StaticCalleeName(call.Fun)
		if name != nil && name.Sym() != nil {
			calls = append(calls, name.Sym().Name)
		}
	})
	for _, want := range []string{"coroTerminalAction", "coroPanic"} {
		if !slices.Contains(calls, want) {
			t.Errorf("generated resume calls = %v, want %s", calls, want)
		}
	}
	if slices.Contains(calls, "gopanic") {
		t.Errorf("generated resume retains gopanic call: %v", calls)
	}
}

func TestLowerDeferTerminal(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/lowerdeferterminal",
		"lowerdeferterminal")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	yield := newLowerTestFunc(pkg, "yield")
	fn := newLowerTestFunc(pkg, "terminalDeferred")
	closure := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
		types.NewSignature(nil, nil, nil), fn, typecheck.Target, 0)
	closure.DeclareParams(true)
	recoverCall := ir.NewCallExpr(src.NoXPos, ir.ORECOVER, nil, nil)
	recoverCall.SetType(types.Types[types.TINTER])
	recoverCall.SetTypecheck(1)
	recoverAssign := ir.NewAssignStmt(src.NoXPos, ir.BlankNode, recoverCall)
	recoverAssign.SetTypecheck(1)
	panicStmt := ir.NewUnaryExpr(src.NoXPos, ir.OPANIC,
		ir.NewNilExpr(src.NoXPos, types.Types[types.TINTER]))
	panicStmt.SetTypecheck(1)
	panicInit := ir.NewAssignStmt(src.NoXPos, ir.BlankNode,
		ir.NewInt(src.NoXPos, 1))
	panicInit.SetTypecheck(1)
	panicStmt.SetInit(ir.Nodes{panicInit})
	runtimePkg := types.NewPkg("runtime", "runtime")
	goexit := newLowerTestFunc(runtimePkg, "Goexit")
	goexitCall := newLowerTestCall(goexit)
	goexitInit := ir.NewAssignStmt(src.NoXPos, ir.BlankNode,
		ir.NewInt(src.NoXPos, 2))
	goexitInit.SetTypecheck(1)
	goexitCall.SetInit(ir.Nodes{goexitInit})
	closure.Body = ir.Nodes{recoverAssign, panicStmt, goexitCall}

	deferCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
		closure.OClosure, nil)
	deferCall.SetTypecheck(1)
	deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, deferCall)
	deferStmt.SetTypecheck(1)
	yieldCall := newLowerTestCall(yield)
	fn.Body = ir.Nodes{deferStmt, yieldCall, newLowerTestReturn()}

	const terminal = MayPanic | UsesRecover | MayGoexit
	function := &Function{
		Func:     fn,
		Local:    MaySuspend,
		Effect:   MaySuspend,
		Terminal: terminal,
		Primary:  CoroPrimary,
		Edges: []Edge{{
			Kind: DeferCall, Callee: closure,
			CalleeName: symbolName(closure.Nname), Node: deferCall,
		}},
		Sites: []Site{{ID: 1, Kind: SiteYield, Node: yieldCall}},
	}
	deferredFunction := &Function{
		Func:          closure,
		LocalTerminal: terminal,
		Terminal:      terminal,
		Primary:       CoroPrimary,
		Edges: []Edge{{
			Kind: DirectCall, Callee: goexit,
			CalleeName: symbolName(goexit.Nname), Node: goexitCall,
			Recipe: operationRecipes["runtime.Goexit"],
		}},
		Sites: []Site{
			{ID: 1, Kind: SitePanic, Node: panicStmt},
			{ID: 2, Kind: SiteGoexit, Node: goexitCall},
		},
	}
	result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{
		fn:      function,
		closure: deferredFunction,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Lowered != 1 || result.Skipped != 1 {
		t.Fatalf("Lower result = %+v, want one lowered and one skipped function",
			result)
	}

	var resume *ir.Func
	for _, generated := range typecheck.Target.Funcs {
		if generated.OClosure != nil && generated.ClosureParent != nil &&
			generated.ClosureParent.Sym().Name == "terminalDeferred.coro" {
			resume = generated
			break
		}
	}
	if resume == nil {
		t.Fatal("terminal defer function has no generated resume function")
	}

	calls := make(map[string]bool)
	for _, generated := range []*ir.Func{resume, closure} {
		ir.Visit(generated, func(node ir.Node) {
			if node.Op() == ir.ORECOVER || node.Op() == ir.OPANIC {
				t.Errorf("%s retains %v", generated.Sym().Name, node.Op())
			}
			call, ok := node.(*ir.CallExpr)
			if !ok || call.Fun == nil {
				return
			}
			name := ir.StaticCalleeName(call.Fun)
			if name != nil && name.Sym() != nil {
				calls[name.Sym().Name] = true
			}
		})
	}
	for _, want := range []string{
		"coroDeferToken", "coroDeferPanic", "coroDeferRecover",
		"coroDeferGoexit", "coroTerminalAction",
	} {
		if !calls[want] {
			t.Errorf("generated terminal defer calls = %v, want %s", calls, want)
		}
	}
}

func TestPlanNamedDeferTerminal(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/plannameddefer",
		"plannameddefer")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	target := newLowerTestFunc(pkg, "target")
	const terminal = MayPanic | UsesRecover
	targetFunction := &Function{
		Func:          target,
		LocalTerminal: terminal,
		Terminal:      terminal,
	}
	plan := &Plan{Functions: map[*ir.Func]*Function{
		target: targetFunction,
	}}

	got, ok := planDeferTerminal(plan, target,
		FuncSummary{Terminal: terminal})
	if !ok || got.target != target || got.rewrite != 0 || !got.factory {
		t.Fatalf("direct named terminal plan = (%+v, %t), want target %v",
			got, ok, target)
	}

	recoverOnly := newLowerTestFunc(pkg, "recoverOnly")
	recoverOnlyFunction := &Function{
		Func:          recoverOnly,
		LocalTerminal: UsesRecover,
		Terminal:      UsesRecover,
	}
	plan.Functions[recoverOnly] = recoverOnlyFunction
	got, ok = planDeferTerminal(plan, recoverOnly,
		FuncSummary{Terminal: UsesRecover})
	if !ok || got.target != recoverOnly || got.factory {
		t.Fatalf("direct recover-only plan = (%+v, %t)", got, ok)
	}
	recoverOnlyFunction.LocalTerminal = 0
	if got, ok := planDeferTerminal(plan, recoverOnly,
		FuncSummary{Terminal: UsesRecover}); ok {
		t.Fatalf("indirect recover-only plan = %+v", got)
	}

	wrapper := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.ODEFER,
		types.NewSignature(nil, nil, nil),
		newLowerTestFunc(pkg, "outer"), typecheck.Target, 0)
	wrapper.DeclareParams(true)
	wrapper.SetWrapper(true)
	wrapper.WrappedFunc = target
	call := newLowerTestCall(target)
	wrapper.Body = ir.Nodes{call}
	plan.Functions[wrapper] = &Function{
		Func:     wrapper,
		Terminal: terminal,
		Edges: []Edge{
			{
				Kind: GoCall, Callee: target,
				CalleeName: symbolName(target.Nname), Node: call,
			},
			{
				Kind: DirectCall, Callee: newLowerTestFunc(pkg, "plain"),
				CalleeName: "plain", Node: call,
			},
			{
				Kind: DirectCall, Callee: target,
				CalleeName: symbolName(target.Nname), Node: call,
			},
		},
	}
	plan.Functions[plan.Functions[wrapper].Edges[1].Callee] = &Function{
		Func: plan.Functions[wrapper].Edges[1].Callee,
	}
	got, ok = planDeferTerminal(plan, wrapper,
		FuncSummary{Terminal: terminal})
	if !ok || got.target != target || got.rewrite != 0 || !got.factory {
		t.Fatalf("wrapped named terminal plan = (%+v, %t), want target %v",
			got, ok, target)
	}

	makeWrapperPlan := func() (*ir.Func, *ir.Func, *Function, *Plan) {
		target := newLowerTestFunc(pkg, "invalidTarget")
		targetFunction := &Function{
			Func:          target,
			LocalTerminal: terminal,
			Terminal:      terminal,
		}
		wrapper := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.ODEFER,
			types.NewSignature(nil, nil, nil),
			newLowerTestFunc(pkg, "invalidOuter"), typecheck.Target, 0)
		wrapper.DeclareParams(true)
		wrapper.SetWrapper(true)
		wrapper.WrappedFunc = target
		call := newLowerTestCall(target)
		wrapper.Body = ir.Nodes{call}
		wrapperFunction := &Function{
			Func:     wrapper,
			Terminal: terminal,
			Edges: []Edge{{
				Kind: DirectCall, Callee: target,
				CalleeName: symbolName(target.Nname), Node: call,
			}},
		}
		return wrapper, target, wrapperFunction, &Plan{
			Functions: map[*ir.Func]*Function{
				wrapper: wrapperFunction,
				target:  targetFunction,
			},
		}
	}

	tests := []struct {
		name   string
		change func(*ir.Func, *ir.Func, *Function, *Plan)
	}{
		{
			name: "missing-target",
			change: func(wrapper, _ *ir.Func, _ *Function, _ *Plan) {
				wrapper.WrappedFunc = nil
			},
		},
		{
			name: "wrapper-local-terminal",
			change: func(_, _ *ir.Func, wrapper *Function, _ *Plan) {
				wrapper.LocalTerminal = MayPanic
			},
		},
		{
			name: "unknown-edge",
			change: func(_, _ *ir.Func, wrapper *Function, _ *Plan) {
				wrapper.Edges[0].Unknown = true
			},
		},
		{
			name: "wrong-target",
			change: func(_, _ *ir.Func, wrapper *Function, plan *Plan) {
				other := newLowerTestFunc(pkg, "otherTarget")
				plan.Functions[other] = &Function{
					Func:          other,
					LocalTerminal: terminal,
					Terminal:      terminal,
				}
				wrapper.Edges[0].Callee = other
			},
		},
		{
			name: "duplicate-target",
			change: func(_, _ *ir.Func, wrapper *Function, _ *Plan) {
				wrapper.Edges = append(wrapper.Edges, wrapper.Edges[0])
			},
		},
		{
			name: "missing-terminal-edge",
			change: func(_, _ *ir.Func, wrapper *Function, plan *Plan) {
				plain := newLowerTestFunc(pkg, "plainTarget")
				plan.Functions[plain] = &Function{Func: plain}
				wrapper.Edges[0].Callee = plain
			},
		},
		{
			name: "target-terminal-mismatch",
			change: func(_, target *ir.Func, _ *Function, plan *Plan) {
				plan.Functions[target].LocalTerminal = UsesRecover
			},
		},
		{
			name: "target-unknown-edge",
			change: func(_, target *ir.Func, _ *Function, plan *Plan) {
				call := newLowerTestCall(target)
				plan.Functions[target].Edges = []Edge{{
					Kind: DirectCall, CalleeName: "<dynamic>",
					Unknown: true, Node: call,
				}}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wrapper, target, wrapperFunction, plan := makeWrapperPlan()
			tc.change(wrapper, target, wrapperFunction, plan)
			if got, ok := planDeferTerminal(plan, wrapper,
				FuncSummary{Terminal: terminal}); ok {
				t.Fatalf("invalid named terminal plan = %+v, want rejection",
					got)
			}
		})
	}

	missing := newLowerTestFunc(pkg, "missing")
	if _, ok := planDeferTerminal(plan, missing,
		FuncSummary{Terminal: terminal}); ok {
		t.Fatal("missing function has a terminal defer plan")
	}
	if _, ok := planDeferTerminal(plan, target,
		FuncSummary{Terminal: MayGoexit}); ok {
		t.Fatal("Goexit has a terminal defer plan")
	}
	if hasDirectDeferTerminal(plan, missing, terminal) {
		t.Fatal("missing function has direct terminal control")
	}
}

func TestPlanNamedDeferGoexit(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/plandefergoexit",
		"plandefergoexit")
	runtimePkg := types.NewPkg("runtime", "runtime")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	goexit := newLowerTestFunc(runtimePkg, "Goexit")
	newPlan := func() (*ir.Func, *Function, *Plan) {
		target := newLowerTestFunc(pkg, "target")
		call := newLowerTestCall(goexit)
		target.Body = ir.Nodes{call}
		function := &Function{
			Func:          target,
			LocalTerminal: MayGoexit,
			Terminal:      MayGoexit,
			Primary:       CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: goexit,
				CalleeName: symbolName(goexit.Nname), Node: call,
				Recipe: operationRecipes["runtime.Goexit"],
			}},
			Sites: []Site{{ID: 1, Kind: SiteGoexit, Node: call}},
		}
		return target, function, &Plan{Functions: map[*ir.Func]*Function{
			target: function,
		}}
	}

	target, function, plan := newPlan()
	got, ok := planDeferTerminal(plan, target,
		FuncSummary{Terminal: MayGoexit})
	if !ok || got.target != target || got.rewrite != 0 {
		t.Fatalf("named Goexit plan = (%+v, %t), want target %v",
			got, ok, target)
	}

	cleanup := newLowerTestFunc(pkg, "cleanup")
	cleanupFunction := &Function{
		Func:          cleanup,
		LocalTerminal: UsesRecover,
		Terminal:      UsesRecover,
	}
	cleanupCall := newLowerTestCall(cleanup)
	function.Edges = append(function.Edges, Edge{
		Kind: DeferCall, Callee: cleanup,
		CalleeName: symbolName(cleanup.Nname), Node: cleanupCall,
	})
	function.Terminal |= UsesRecover
	plan.Functions[cleanup] = cleanupFunction
	got, ok = planDeferTerminal(plan, target,
		FuncSummary{Terminal: MayGoexit | UsesRecover})
	if !ok || got.target != target {
		t.Fatalf("Goexit target with terminal cleanup plan = (%+v, %t)",
			got, ok)
	}

	wrapper := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.ODEFER,
		types.NewSignature(nil, nil, nil),
		newLowerTestFunc(pkg, "outer"), typecheck.Target, 0)
	wrapper.DeclareParams(true)
	wrapper.SetWrapper(true)
	wrapper.WrappedFunc = target
	targetCall := newLowerTestCall(target)
	wrapper.Body = ir.Nodes{targetCall}
	plan.Functions[wrapper] = &Function{
		Func:     wrapper,
		Terminal: function.Terminal,
		Edges: []Edge{{
			Kind: DirectCall, Callee: target,
			CalleeName: symbolName(target.Nname), Node: targetCall,
		}},
	}
	got, ok = planDeferTerminal(plan, wrapper,
		FuncSummary{Terminal: function.Terminal})
	if !ok || got.target != target {
		t.Fatalf("wrapped Goexit plan = (%+v, %t), want target %v",
			got, ok, target)
	}

	missing := newLowerTestFunc(pkg, "missing")
	if canPlanDeferFactoryTarget(plan, missing, MayGoexit) {
		t.Fatal("missing function is a Goexit defer target")
	}

	plain := newLowerTestFunc(pkg, "plain")
	direct := newLowerTestFunc(pkg, "direct")
	plainCall := newLowerTestCall(plain)
	directGoexitCall := newLowerTestCall(goexit)
	directFunction := &Function{
		Func:          direct,
		LocalTerminal: MayGoexit,
		Terminal:      MayGoexit,
		Edges: []Edge{
			{
				Kind: DirectCall, Callee: plain,
				CalleeName: symbolName(plain.Nname), Node: plainCall,
			},
			{
				Kind: DirectCall, Callee: goexit,
				CalleeName: symbolName(goexit.Nname), Node: directGoexitCall,
				Recipe: operationRecipes["runtime.Goexit"],
			},
		},
	}
	directPlan := &Plan{Functions: map[*ir.Func]*Function{
		direct: directFunction,
		plain:  {Func: plain},
	}}
	if !hasDirectDeferTerminal(directPlan, direct, MayGoexit) {
		t.Fatal("direct Goexit with a plain edge has no terminal plan")
	}
	directFunction.Edges = append([]Edge{{
		Kind: GoCall, Callee: plain,
		CalleeName: symbolName(plain.Nname), Node: newLowerTestCall(plain),
	}}, directFunction.Edges...)
	if hasDirectDeferTerminal(directPlan, direct, MayGoexit) {
		t.Fatal("direct Goexit with a go statement has a terminal plan")
	}

	tests := []struct {
		name   string
		change func(*ir.Func, *Function, *Plan)
	}{
		{
			name: "no-local-Goexit",
			change: func(_ *ir.Func, function *Function, _ *Plan) {
				function.LocalTerminal = 0
			},
		},
		{
			name: "go-call",
			change: func(_ *ir.Func, function *Function, plan *Plan) {
				child := newLowerTestFunc(pkg, "goChild")
				plan.Functions[child] = &Function{Func: child}
				function.Edges = append(function.Edges, Edge{
					Kind: GoCall, Callee: child,
					CalleeName: symbolName(child.Nname),
					Node:       newLowerTestCall(child),
				})
			},
		},
		{
			name: "unknown-call",
			change: func(_ *ir.Func, function *Function, _ *Plan) {
				function.Edges = append(function.Edges, Edge{
					Kind: DirectCall, CalleeName: "<dynamic>", Unknown: true,
					Node: newLowerTestCall(goexit),
				})
			},
		},
		{
			name: "called-terminal",
			change: func(_ *ir.Func, function *Function, plan *Plan) {
				child := newLowerTestFunc(pkg, "recoverChild")
				plan.Functions[child] = &Function{
					Func: child, LocalTerminal: UsesRecover,
					Terminal: UsesRecover,
				}
				function.Terminal |= UsesRecover
				function.Edges = append(function.Edges, Edge{
					Kind: DirectCall, Callee: child,
					CalleeName: symbolName(child.Nname),
					Node:       newLowerTestCall(child),
				})
			},
		},
		{
			name: "wrong-operation",
			change: func(_ *ir.Func, function *Function, _ *Plan) {
				function.Edges = append(function.Edges, Edge{
					Kind: DirectCall, Callee: goexit,
					CalleeName: symbolName(goexit.Nname),
					Node:       newLowerTestCall(goexit),
					Recipe: OperationRecipe{
						Kind: SiteYield, Terminal: MayGoexit,
					},
				})
			},
		},
		{
			name: "wrong-terminal",
			change: func(_ *ir.Func, function *Function, _ *Plan) {
				function.LocalTerminal |= MayPanic
				function.Terminal |= MayPanic
				function.Edges = append(function.Edges, Edge{
					Kind: DirectCall, Callee: goexit,
					CalleeName: symbolName(goexit.Nname),
					Node:       newLowerTestCall(goexit),
					Recipe: OperationRecipe{
						Kind: SiteGoexit, Terminal: MayPanic,
					},
				})
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target, function, plan := newPlan()
			tc.change(target, function, plan)
			if canPlanDeferFactoryTarget(plan, target, function.Terminal) {
				t.Fatal("invalid function is a Goexit defer target")
			}
			if _, ok := planDeferTerminal(plan, target,
				FuncSummary{Terminal: function.Terminal}); ok {
				t.Fatal("invalid function has a Goexit defer plan")
			}
		})
	}
}

func TestPlanImportedDeferTerminal(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/importeddefer",
		"importeddefer")
	importedPkg := types.NewPkg("example.com/coro/importeddefer/target",
		"target")
	runtimePkg := types.NewPkg("runtime", "runtime")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)
	plan := &Plan{Functions: make(map[*ir.Func]*Function)}

	plain := newLowerTestFunc(importedPkg, "Recover")
	plainSummary := FuncSummary{
		Terminal: UsesRecover,
		Defer:    DeferABI1,
	}
	SetSummary(plain, plainSummary)
	got, ok := planDeferTerminal(plan, plain, plainSummary)
	if !ok || got.target != plain || got.factory || got.directGoexit {
		t.Fatalf("imported plain terminal plan = (%+v, %t)", got, ok)
	}

	coroutine := newLowerTestFunc(importedPkg, "Panic")
	coroutineSummary := FuncSummary{
		Terminal: MayPanic,
		Factory:  FactoryABI1,
		Defer:    DeferABI1,
	}
	SetSummary(coroutine, coroutineSummary)
	got, ok = planDeferTerminal(plan, coroutine, coroutineSummary)
	if !ok || got.target != coroutine || !got.factory ||
		got.directGoexit {
		t.Fatalf("imported coroutine terminal plan = (%+v, %t)", got, ok)
	}

	goexit := newLowerTestFunc(runtimePkg, "Goexit")
	got, ok = planDeferTerminal(plan, goexit,
		FuncSummary{Terminal: MayGoexit})
	if !ok || got.target != goexit || !got.directGoexit || got.factory {
		t.Fatalf("direct Goexit terminal plan = (%+v, %t)", got, ok)
	}

	tests := []struct {
		name    string
		summary FuncSummary
		input   FuncSummary
		shape   bool
	}{
		{
			name:    "missing-capability",
			summary: FuncSummary{Terminal: UsesRecover},
			input:   FuncSummary{Terminal: UsesRecover},
		},
		{
			name: "missing-factory",
			summary: FuncSummary{
				Terminal: MayPanic, Defer: DeferABI1,
			},
			input: FuncSummary{Terminal: MayPanic},
		},
		{
			name: "missing-coroutine-capability",
			summary: FuncSummary{
				Terminal: MayPanic, Factory: FactoryABI1,
			},
			input: FuncSummary{Terminal: MayPanic},
		},
		{
			name: "suspending",
			summary: FuncSummary{
				Effect: MaySuspend, Terminal: UsesRecover,
				Defer: DeferABI1,
			},
			input: FuncSummary{Terminal: UsesRecover},
		},
		{
			name: "execution-constraint",
			summary: FuncSummary{
				Exec: NeedsPreempt, Terminal: UsesRecover,
				Defer: DeferABI1,
			},
			input: FuncSummary{Terminal: UsesRecover},
		},
		{
			name: "terminal-mismatch",
			summary: FuncSummary{
				Terminal: MayPanic, Factory: FactoryABI1,
				Defer: DeferABI1,
			},
			input: FuncSummary{Terminal: MayGoexit},
		},
		{
			name: "unsupported-shape",
			summary: FuncSummary{
				Terminal: MayPanic, Factory: FactoryABI1,
				Defer: DeferABI1,
			},
			input: FuncSummary{Terminal: MayPanic},
			shape: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fn := newLowerTestFunc(importedPkg, test.name)
			if test.shape {
				fn.Type().SetHasShape(true)
			}
			SetSummary(fn, test.summary)
			if got, ok := planDeferTerminal(plan, fn, test.input); ok {
				t.Fatalf("invalid imported terminal plan = %+v", got)
			}
		})
	}

	missing := newLowerTestFunc(importedPkg, "Missing")
	if got, ok := planDeferTerminal(plan, missing,
		FuncSummary{Terminal: UsesRecover}); ok {
		t.Fatalf("missing imported summary plan = %+v", got)
	}
	gosched := newLowerTestFunc(runtimePkg, "Gosched")
	if got, ok := planDeferTerminal(plan, gosched,
		FuncSummary{Terminal: MayGoexit}); ok {
		t.Fatalf("non-Goexit operation plan = %+v", got)
	}
	if got, ok := planDeferTerminal(plan, nil,
		FuncSummary{Terminal: MayPanic}); ok {
		t.Fatalf("nil terminal plan = %+v", got)
	}
}

func TestDeferFactoryCandidates(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/defercandidates",
		"defercandidates")
	importedPkg := types.NewPkg("example.com/coro/defercandidates/imported",
		"imported")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)
	newCandidate := func(name string) (*ir.Func, *Function,
		*lowerCandidate) {
		fn := newLowerTestFunc(pkg, name)
		function := &Function{
			Func: fn, Terminal: MayPanic, Primary: CoroPrimary,
		}
		return fn, function, &lowerCandidate{
			function:     function,
			dependencies: make(map[*ir.Func]bool),
		}
	}

	leaf, leafFunction, leafCandidate := newCandidate("leaf")
	plainDependency := newLowerTestFunc(pkg, "plainDependency")
	plainDependencyFunction := &Function{Func: plainDependency}
	plainCall := newLowerTestCall(plainDependency)
	leafFunction.Edges = []Edge{{
		Kind: DeferCall, Callee: plainDependency,
		CalleeName: symbolName(plainDependency.Nname), Node: plainCall,
	}}
	leafCandidate.dependencies[plainDependency] = true
	parent, parentFunction, parentCandidate := newCandidate("parent")
	parentCandidate.dependencies[leaf] = true

	first, firstFunction, firstCandidate := newCandidate("first")
	second, secondFunction, secondCandidate := newCandidate("second")
	firstCandidate.dependencies[second] = true
	secondCandidate.dependencies[first] = true

	imported := newLowerTestFunc(importedPkg, "good")
	SetSummary(imported, FuncSummary{
		Terminal: MayPanic, Factory: FactoryABI1, Defer: DeferABI1,
	})
	importedParent, importedParentFunction,
		importedParentCandidate := newCandidate("importedParent")
	importedParentCandidate.dependencies[imported] = true

	ordinaryOnly := newLowerTestFunc(importedPkg, "ordinaryOnly")
	SetSummary(ordinaryOnly, FuncSummary{
		Terminal: MayPanic, Factory: FactoryABI1,
	})
	rejected, rejectedFunction, rejectedCandidate :=
		newCandidate("rejected")
	rejectedCandidate.dependencies[ordinaryOnly] = true
	transitive, transitiveFunction, transitiveCandidate :=
		newCandidate("transitive")
	transitiveCandidate.dependencies[rejected] = true

	unknown := newLowerTestFunc(importedPkg, "unknown")
	unknownParent, unknownParentFunction,
		unknownParentCandidate := newCandidate("unknownParent")
	unknownParentCandidate.dependencies[unknown] = true

	plan := &Plan{Functions: map[*ir.Func]*Function{
		leaf:            leafFunction,
		plainDependency: plainDependencyFunction,
		parent:          parentFunction,
		first:           firstFunction,
		second:          secondFunction,
		importedParent:  importedParentFunction,
		rejected:        rejectedFunction,
		transitive:      transitiveFunction,
		unknownParent:   unknownParentFunction,
	}}
	candidates := map[*ir.Func]*lowerCandidate{
		leaf:           leafCandidate,
		parent:         parentCandidate,
		first:          firstCandidate,
		second:         secondCandidate,
		importedParent: importedParentCandidate,
		rejected:       rejectedCandidate,
		transitive:     transitiveCandidate,
		unknownParent:  unknownParentCandidate,
	}
	available := deferFactoryCandidates(plan, candidates)
	for _, fn := range []*ir.Func{
		leaf, parent, first, second, importedParent,
	} {
		if !available[fn] {
			t.Errorf("%s is not a defer factory candidate", fn.Sym().Name)
		}
	}
	for _, fn := range []*ir.Func{rejected, transitive, unknownParent} {
		if available[fn] {
			t.Errorf("%s remains a defer factory candidate", fn.Sym().Name)
		}
	}
	if !hasDeferFactory(plan, available, leaf) ||
		hasDeferFactory(plan, available, rejected) ||
		!hasDeferFactory(plan, available, imported) ||
		hasDeferFactory(plan, available, ordinaryOnly) {
		t.Fatalf("defer factory lookup disagrees with candidates %v",
			available)
	}

	recoverTarget := newLowerTestFunc(pkg, "recoverTarget")
	recoverFunction := &Function{
		Func: recoverTarget, Terminal: UsesRecover,
	}
	plan.Functions[recoverTarget] = recoverFunction
	directRecover, directRecoverFunction,
		directRecoverCandidate := newCandidate("directRecover")
	recoverCall := newLowerTestCall(recoverTarget)
	directRecoverFunction.Edges = []Edge{{
		Kind: DirectCall, Callee: recoverTarget,
		CalleeName: symbolName(recoverTarget.Nname), Node: recoverCall,
	}}
	plan.Functions[directRecover] = directRecoverFunction

	goCandidate, goFunction, goCallCandidate := newCandidate("goCall")
	goFunction.Edges = []Edge{{
		Kind: GoCall, Callee: leaf,
		CalleeName: symbolName(leaf.Nname), Node: newLowerTestCall(leaf),
	}}
	plan.Functions[goCandidate] = goFunction

	unknownCall, unknownCallFunction,
		unknownCallCandidate := newCandidate("unknownCall")
	unknownCallFunction.Edges = []Edge{{
		Kind: DirectCall, CalleeName: "<dynamic>", Unknown: true,
		Node: newLowerTestCall(leaf),
	}}
	plan.Functions[unknownCall] = unknownCallFunction

	invalid := []*lowerCandidate{
		nil,
		{function: &Function{
			Func:   newLowerTestFunc(pkg, "suspending"),
			Effect: MaySuspend, Terminal: MayPanic,
		}},
		{function: &Function{
			Func: newLowerTestFunc(pkg, "constrained"),
			Exec: NeedsPreempt, Terminal: MayPanic,
		}},
		{function: &Function{
			Func: newLowerTestFunc(pkg, "nonterminal"),
		}},
		directRecoverCandidate,
		goCallCandidate,
		unknownCallCandidate,
	}
	shape := newLowerTestFunc(pkg, "shape")
	shape.Type().SetHasShape(true)
	invalid = append(invalid, &lowerCandidate{
		function: &Function{Func: shape, Terminal: MayPanic},
	})
	for i, candidate := range invalid {
		if deferFactoryCandidate(plan, candidate) {
			t.Errorf("invalid candidate %d supports defer factory", i)
		}
	}
}

func TestLowerNamedDeferRecover(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/lowernameddefer",
		"lowernameddefer")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	yield := newLowerTestFunc(pkg, "yield")
	target := newLowerTestFunc(pkg, "target")
	recoverCall := ir.NewCallExpr(src.NoXPos, ir.ORECOVER, nil, nil)
	recoverCall.SetType(types.Types[types.TINTER])
	recoverCall.SetTypecheck(1)
	target.Body = ir.Nodes{ir.NewAssignStmt(src.NoXPos, ir.BlankNode,
		recoverCall)}

	fn := newLowerTestFunc(pkg, "namedDeferred")
	deferCall := newLowerTestCall(target)
	deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, deferCall)
	deferStmt.SetTypecheck(1)
	yieldCall := newLowerTestCall(yield)
	fn.Body = ir.Nodes{deferStmt, yieldCall, newLowerTestReturn()}

	function := &Function{
		Func:     fn,
		Local:    MaySuspend,
		Effect:   MaySuspend,
		Terminal: UsesRecover,
		Primary:  CoroPrimary,
		Edges: []Edge{{
			Kind: DeferCall, Callee: target,
			CalleeName: symbolName(target.Nname), Node: deferCall,
		}},
		Sites: []Site{{ID: 1, Kind: SiteYield, Node: yieldCall}},
	}
	targetFunction := &Function{
		Func:          target,
		LocalTerminal: UsesRecover,
		Terminal:      UsesRecover,
		Primary:       PlainPrimary,
	}
	result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{
		fn:     function,
		target: targetFunction,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Lowered != 1 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want one lowered function", result)
	}

	calls := make(map[string]bool)
	for _, generated := range typecheck.Target.Funcs {
		ir.Visit(generated, func(node ir.Node) {
			call, ok := node.(*ir.CallExpr)
			if !ok || call.Fun == nil {
				return
			}
			name := ir.StaticCalleeName(call.Fun)
			if name != nil && name.Sym() != nil {
				calls[name.Sym().Name] = true
			}
		})
	}
	for _, want := range []string{
		"coroDeferToken", "coroDeferCall", "coroTerminalAction",
	} {
		if !calls[want] {
			t.Errorf("generated named defer calls = %v, want %s", calls, want)
		}
	}
	if !ir.Any(target, func(node ir.Node) bool {
		return node.Op() == ir.ORECOVER
	}) {
		t.Fatal("named defer target no longer contains ordinary recover")
	}
}

func TestLowerNamedDeferGoexit(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/lowernamedgoexit",
		"lowernamedgoexit")
	runtimePkg := types.NewPkg("runtime", "runtime")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	yield := newLowerTestFunc(pkg, "yield")
	goexit := newLowerTestFunc(runtimePkg, "Goexit")
	target := newLowerTestFunc(pkg, "target")
	goexitCall := newLowerTestCall(goexit)
	target.Body = ir.Nodes{goexitCall}

	fn := newLowerTestFunc(pkg, "namedDeferred")
	deferCall := newLowerTestCall(target)
	deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, deferCall)
	deferStmt.SetTypecheck(1)
	yieldCall := newLowerTestCall(yield)
	fn.Body = ir.Nodes{deferStmt, yieldCall, newLowerTestReturn()}

	function := &Function{
		Func:     fn,
		Local:    MaySuspend,
		Effect:   MaySuspend,
		Terminal: MayGoexit,
		Primary:  CoroPrimary,
		Edges: []Edge{{
			Kind: DeferCall, Callee: target,
			CalleeName: symbolName(target.Nname), Node: deferCall,
		}},
		Sites: []Site{{ID: 1, Kind: SiteYield, Node: yieldCall}},
	}
	targetFunction := &Function{
		Func:          target,
		LocalTerminal: MayGoexit,
		Terminal:      MayGoexit,
		Primary:       CoroPrimary,
		Edges: []Edge{{
			Kind: DirectCall, Callee: goexit,
			CalleeName: symbolName(goexit.Nname), Node: goexitCall,
			Recipe: operationRecipes["runtime.Goexit"],
		}},
		Sites: []Site{{ID: 1, Kind: SiteGoexit, Node: goexitCall}},
	}
	result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{
		fn:     function,
		target: targetFunction,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Lowered != 2 || result.Skipped != 0 ||
		len(result.Diagnostics) != 0 {
		t.Fatalf("Lower result = %+v, want caller and target lowered", result)
	}

	calls := make(map[string]bool)
	for _, generated := range typecheck.Target.Funcs {
		ir.Visit(generated, func(node ir.Node) {
			call, ok := node.(*ir.CallExpr)
			if !ok || call.Fun == nil {
				return
			}
			name := ir.StaticCalleeName(call.Fun)
			if name != nil && name.Sym() != nil {
				calls[name.Sym().Name] = true
			}
		})
	}
	for _, want := range []string{
		"coroDeferToken", "coroDeferRun", "target.coro",
		"coroGoexit", "coroTerminalAction",
	} {
		if !calls[want] {
			t.Errorf("generated named Goexit calls = %v, want %s",
				calls, want)
		}
	}
	if calls["Goexit"] {
		t.Errorf("generated named Goexit calls retain runtime.Goexit: %v",
			calls)
	}
}

func TestRewriteDeferGoexitCall(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/rewritedefergoexit",
		"rewritedefergoexit")
	runtimePkg := types.NewPkg("runtime", "runtime")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	goexit := newLowerTestFunc(runtimePkg, "Goexit")
	resume := newLowerTestFunc(pkg, "resume")
	token := resume.NewLocal(src.NoXPos, pkg.Lookup("token"),
		types.Types[types.TUNSAFEPTR])
	ir.CurFunc = resume

	newDirectDefer := func(target *ir.Func) *lowerDefer {
		call := newLowerTestCall(target)
		statement := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, call)
		statement.SetTypecheck(1)
		return &lowerDefer{statement: statement, call: call}
	}
	newWrappedDefer := func(target, callee *ir.Func,
		args ir.Nodes) (*lowerDefer, *ir.ClosureExpr, *ir.CallExpr) {
		wrapper := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.ODEFER,
			types.NewSignature(nil, nil, nil), resume, typecheck.Target, 0)
		wrapper.DeclareParams(true)
		wrapper.SetWrapper(true)
		wrapper.WrappedFunc = target
		call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, callee.Nname, args)
		call.SetTypecheck(1)
		wrapper.Body = ir.Nodes{call}
		deferCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
			wrapper.OClosure, nil)
		deferCall.SetTypecheck(1)
		statement := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, deferCall)
		statement.SetTypecheck(1)
		return &lowerDefer{statement: statement, call: deferCall},
			wrapper.OClosure, call
	}

	deferred := newDirectDefer(goexit)
	got, err := rewriteDeferGoexitCall(deferred, resume, token, goexit)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || deferred.call.Fun != got ||
		got.Func.WrappedFunc != goexit ||
		len(got.Func.ClosureVars) != 1 {
		t.Fatalf("direct Goexit wrapper = %v, call %v, captures %v",
			got, deferred.call.Fun, got.Func.ClosureVars)
	}
	var calls []string
	ir.Visit(got.Func, func(node ir.Node) {
		call, ok := node.(*ir.CallExpr)
		if !ok {
			return
		}
		callee := ir.StaticCalleeName(call.Fun)
		if callee != nil && callee.Sym() != nil {
			calls = append(calls, callee.Sym().Name)
		}
	})
	if !slices.Equal(calls, []string{"coroDeferGoexit"}) {
		t.Fatalf("direct Goexit wrapper calls = %v", calls)
	}

	wrapped, closure, call := newWrappedDefer(goexit, goexit, nil)
	initName := resume.NewLocal(src.NoXPos, pkg.Lookup("init"),
		types.Types[types.TINT])
	init := ir.NewAssignStmt(src.NoXPos, initName, ir.NewInt(src.NoXPos, 1))
	call.SetInit(ir.Nodes{init})
	got, err = rewriteDeferGoexitCall(wrapped, resume, token, goexit)
	if err != nil {
		t.Fatal(err)
	}
	if got != closure || len(closure.Func.Body) != 2 ||
		closure.Func.Body[0] != init {
		t.Fatalf("wrapped Goexit body = %v, want init and runtime call",
			closure.Func.Body)
	}

	if _, err := rewriteDeferGoexitCall(wrapped, resume, token, nil); err == nil ||
		!strings.Contains(err.Error(), "missing direct Goexit target") {
		t.Fatalf("missing target error = %v", err)
	}

	t.Run("wrapper-shape", func(t *testing.T) {
		deferred, closure, _ := newWrappedDefer(goexit, goexit, nil)
		closure.Func.Body = append(closure.Func.Body, newLowerTestReturn())
		if _, err := rewriteDeferGoexitCall(deferred, resume, token,
			goexit); err == nil ||
			!strings.Contains(err.Error(), "has 2 statements") {
			t.Fatalf("wrapper shape error = %v", err)
		}
	})

	t.Run("wrapper-call", func(t *testing.T) {
		deferred, closure, _ := newWrappedDefer(goexit, goexit, nil)
		closure.Func.Body = ir.Nodes{newLowerTestReturn()}
		if _, err := rewriteDeferGoexitCall(deferred, resume, token,
			goexit); err == nil ||
			!strings.Contains(err.Error(), "has no call") {
			t.Fatalf("wrapper call error = %v", err)
		}
	})

	t.Run("wrapper-target", func(t *testing.T) {
		other := newLowerTestFunc(runtimePkg, "Other")
		deferred, _, _ := newWrappedDefer(goexit, other, nil)
		if _, err := rewriteDeferGoexitCall(deferred, resume, token,
			goexit); err == nil ||
			!strings.Contains(err.Error(), "wrapper calls") {
			t.Fatalf("wrapper target error = %v", err)
		}
	})

	t.Run("wrapper-arguments", func(t *testing.T) {
		deferred, _, _ := newWrappedDefer(goexit, goexit,
			ir.Nodes{ir.NewInt(src.NoXPos, 1)})
		if _, err := rewriteDeferGoexitCall(deferred, resume, token,
			goexit); err == nil ||
			!strings.Contains(err.Error(), "has arguments") {
			t.Fatalf("wrapper argument error = %v", err)
		}
	})

	t.Run("direct-target", func(t *testing.T) {
		other := newLowerTestFunc(runtimePkg, "DirectOther")
		deferred := newDirectDefer(other)
		if _, err := rewriteDeferGoexitCall(deferred, resume, token,
			goexit); err == nil ||
			!strings.Contains(err.Error(), "no static target") {
			t.Fatalf("direct target error = %v", err)
		}
	})

	t.Run("direct-normalization", func(t *testing.T) {
		result := types.NewField(src.NoXPos, nil, types.Types[types.TINT])
		target := ir.NewFunc(src.NoXPos, src.NoXPos,
			runtimePkg.Lookup("GoexitResult"),
			types.NewSignature(nil, nil, []*types.Field{result}))
		target.DeclareParams(true)
		deferred := newDirectDefer(target)
		if _, err := rewriteDeferGoexitCall(deferred, resume, token,
			target); err == nil ||
			!strings.Contains(err.Error(), "not normalized") {
			t.Fatalf("direct normalization error = %v", err)
		}
	})
}

func TestRewriteDeferFactoryCall(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/rewritedeferfactory",
		"rewritedeferfactory")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	param := types.NewField(src.NoXPos, pkg.Lookup("value"),
		types.Types[types.TINT])
	result := types.NewField(src.NoXPos, nil, types.Types[types.TINT])
	target := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("target"),
		types.NewSignature(nil, []*types.Field{param}, []*types.Field{result}))
	target.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, target)
	factory := newResumeFactory(target)
	resume := newLowerTestFunc(pkg, "resume")
	token := resume.NewLocal(src.NoXPos, pkg.Lookup("token"),
		types.Types[types.TUNSAFEPTR])

	newWrappedDefer := func(callee *ir.Func, args ir.Nodes) (*lowerDefer,
		*ir.ClosureExpr) {
		wrapper := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.ODEFER,
			types.NewSignature(nil, nil, nil), resume, typecheck.Target, 0)
		wrapper.DeclareParams(true)
		wrapper.SetWrapper(true)
		wrapper.WrappedFunc = target
		call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, callee.Nname, args)
		call.SetType(types.Types[types.TINT])
		call.SetTypecheck(1)
		wrapper.Body = ir.Nodes{call}
		deferCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
			wrapper.OClosure, nil)
		deferCall.SetTypecheck(1)
		statement := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, deferCall)
		statement.SetTypecheck(1)
		return &lowerDefer{statement: statement, call: deferCall},
			wrapper.OClosure
	}

	deferred, closure := newWrappedDefer(target, ir.Nodes{
		ir.NewInt(src.NoXPos, 23),
	})
	ir.CurFunc = closure.Func
	got, err := rewriteDeferFactoryCall(deferred, resume, token,
		target, factory)
	if err != nil {
		t.Fatal(err)
	}
	if got != closure || len(closure.Func.Body) != 2 ||
		closure.Func.Body[0].Op() != ir.ODCL ||
		len(closure.Func.ClosureVars) != 1 {
		t.Fatalf("rewritten wrapper = %v, body %v, captures %v",
			got, closure.Func.Body, closure.Func.ClosureVars)
	}
	var calls []string
	ir.Visit(closure.Func, func(node ir.Node) {
		call, ok := node.(*ir.CallExpr)
		if !ok {
			return
		}
		name := ir.StaticCalleeName(call.Fun)
		if name != nil && name.Sym() != nil {
			calls = append(calls, name.Sym().Name)
		}
	})
	for _, want := range []string{"target.coro", "coroDeferRun"} {
		if !slices.Contains(calls, want) {
			t.Errorf("rewritten wrapper calls = %v, want %s", calls, want)
		}
	}

	if _, err := rewriteDeferFactoryCall(deferred, resume, token,
		target, nil); err == nil ||
		!strings.Contains(err.Error(), "missing terminal defer factory") {
		t.Fatalf("missing factory error = %v", err)
	}

	t.Run("wrapper-shape", func(t *testing.T) {
		deferred, closure := newWrappedDefer(target, ir.Nodes{
			ir.NewInt(src.NoXPos, 1),
		})
		closure.Func.Body = append(closure.Func.Body, newLowerTestReturn())
		if _, err := rewriteDeferFactoryCall(deferred, resume, token,
			target, factory); err == nil ||
			!strings.Contains(err.Error(), "has 2 statements") {
			t.Fatalf("wrapper shape error = %v", err)
		}
	})

	t.Run("wrapper-call", func(t *testing.T) {
		deferred, closure := newWrappedDefer(target, nil)
		closure.Func.Body = ir.Nodes{newLowerTestReturn()}
		if _, err := rewriteDeferFactoryCall(deferred, resume, token,
			target, factory); err == nil ||
			!strings.Contains(err.Error(), "has no call") {
			t.Fatalf("wrapper call error = %v", err)
		}
	})

	t.Run("wrapper-target", func(t *testing.T) {
		other := newLowerTestFunc(pkg, "other")
		deferred, _ := newWrappedDefer(other, nil)
		if _, err := rewriteDeferFactoryCall(deferred, resume, token,
			target, factory); err == nil ||
			!strings.Contains(err.Error(), "wrapper calls") {
			t.Fatalf("wrapper target error = %v", err)
		}
	})

	t.Run("wrapper-arguments", func(t *testing.T) {
		deferred, _ := newWrappedDefer(target, nil)
		if _, err := rewriteDeferFactoryCall(deferred, resume, token,
			target, factory); err == nil ||
			!strings.Contains(err.Error(), "0 arguments, want 1") {
			t.Fatalf("wrapper argument error = %v", err)
		}
	})

	t.Run("direct-target", func(t *testing.T) {
		other := newLowerTestFunc(pkg, "direct")
		call := newLowerTestCall(other)
		statement := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, call)
		statement.SetTypecheck(1)
		deferred := &lowerDefer{statement: statement, call: call}
		if _, err := rewriteDeferFactoryCall(deferred, resume, token,
			target, factory); err == nil ||
			!strings.Contains(err.Error(), "no static target") {
			t.Fatalf("direct target error = %v", err)
		}
	})

	t.Run("direct-normalization", func(t *testing.T) {
		call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, target.Nname, nil)
		call.SetTypecheck(1)
		statement := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, call)
		statement.SetTypecheck(1)
		deferred := &lowerDefer{statement: statement, call: call}
		if _, err := rewriteDeferFactoryCall(deferred, resume, token,
			target, factory); err == nil ||
			!strings.Contains(err.Error(), "not normalized") {
			t.Fatalf("direct normalization error = %v", err)
		}
	})
}

func TestLowerDynamicNamedDeferRecover(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/lowerdynamicnameddefer",
		"lowerdynamicnameddefer")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	yield := newLowerTestFunc(pkg, "yield")
	target := newLowerTestFunc(pkg, "target")
	recoverCall := ir.NewCallExpr(src.NoXPos, ir.ORECOVER, nil, nil)
	recoverCall.SetType(types.Types[types.TINTER])
	recoverCall.SetTypecheck(1)
	target.Body = ir.Nodes{ir.NewAssignStmt(src.NoXPos, ir.BlankNode,
		recoverCall)}

	fn := newLowerTestFunc(pkg, "dynamicNamedDeferred")
	deferCall := newLowerTestCall(target)
	deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, deferCall)
	deferStmt.SetTypecheck(1)
	yieldCall := newLowerTestCall(yield)
	loop := ir.NewForStmt(src.NoXPos, nil,
		ir.NewBool(src.NoXPos, false), nil,
		ir.Nodes{deferStmt, yieldCall}, false)
	loop.SetTypecheck(1)
	fn.Body = ir.Nodes{loop, newLowerTestReturn()}

	function := &Function{
		Func:     fn,
		Local:    MaySuspend,
		Effect:   MaySuspend,
		Terminal: UsesRecover,
		Primary:  CoroPrimary,
		Edges: []Edge{{
			Kind: DeferCall, Callee: target,
			CalleeName: symbolName(target.Nname), Node: deferCall,
		}},
		Sites: []Site{{ID: 1, Kind: SiteYield, Node: yieldCall}},
	}
	targetFunction := &Function{
		Func:          target,
		LocalTerminal: UsesRecover,
		Terminal:      UsesRecover,
	}
	result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{
		fn:     function,
		target: targetFunction,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Lowered != 1 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want one lowered function", result)
	}

	found := false
	for _, generated := range typecheck.Target.Funcs {
		ir.Visit(generated, func(node ir.Node) {
			call, ok := node.(*ir.CallExpr)
			if !ok || call.Fun == nil {
				return
			}
			name := ir.StaticCalleeName(call.Fun)
			if name != nil && name.Sym() != nil &&
				name.Sym().Name == "coroDeferCall" {
				found = true
			}
		})
	}
	if !found {
		t.Fatal("dynamic named defer does not call coroDeferCall")
	}
}

func TestLowerRunsPanickingNamedDeferFactory(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/retainnameddefer",
		"retainnameddefer")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	yield := newLowerTestFunc(pkg, "yield")
	target := newLowerTestFunc(pkg, "target")
	recoverCall := ir.NewCallExpr(src.NoXPos, ir.ORECOVER, nil, nil)
	recoverCall.SetType(types.Types[types.TINTER])
	recoverCall.SetTypecheck(1)
	panicStmt := ir.NewUnaryExpr(src.NoXPos, ir.OPANIC,
		ir.NewNilExpr(src.NoXPos, types.Types[types.TINTER]))
	panicStmt.SetTypecheck(1)
	target.Body = ir.Nodes{
		ir.NewAssignStmt(src.NoXPos, ir.BlankNode, recoverCall),
		panicStmt,
	}

	fn := newLowerTestFunc(pkg, "namedDeferred")
	deferCall := newLowerTestCall(target)
	deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, deferCall)
	deferStmt.SetTypecheck(1)
	yieldCall := newLowerTestCall(yield)
	fn.Body = ir.Nodes{deferStmt, yieldCall, newLowerTestReturn()}

	const terminal = MayPanic | UsesRecover
	function := &Function{
		Func:     fn,
		Local:    MaySuspend,
		Effect:   MaySuspend,
		Terminal: terminal,
		Primary:  CoroPrimary,
		Edges: []Edge{{
			Kind: DeferCall, Callee: target,
			CalleeName: symbolName(target.Nname), Node: deferCall,
		}},
		Sites: []Site{{ID: 1, Kind: SiteYield, Node: yieldCall}},
	}
	targetFunction := &Function{
		Func:          target,
		LocalTerminal: terminal,
		Terminal:      terminal,
		Primary:       CoroPrimary,
		Sites:         []Site{{ID: 1, Kind: SitePanic, Node: panicStmt}},
	}
	result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{
		fn:     function,
		target: targetFunction,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Lowered != 2 || result.Skipped != 0 ||
		len(result.Diagnostics) != 0 {
		t.Fatalf("Lower result = %+v, want target and caller lowered",
			result)
	}
	if targetFunction.Factory != FactoryABI1 ||
		targetFunction.Defer != DeferABI1 {
		t.Fatalf("target capabilities = factory %v, defer %v",
			targetFunction.Factory, targetFunction.Defer)
	}
	if !ir.Any(target, func(node ir.Node) bool {
		call, ok := node.(*ir.CallExpr)
		return ok && symbolName(ir.StaticCalleeName(call.Fun)) ==
			"runtime.coroRun"
	}) {
		t.Fatal("named defer target does not use its coroutine entry")
	}
}

func TestLowerRequiresDeferSafeTransitiveFactory(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/defertransitive",
		"defertransitive")
	importedPkg := types.NewPkg("example.com/coro/defertransitive/imported",
		"imported")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	imported := newLowerTestFunc(importedPkg, "Panic")
	importedSummary := FuncSummary{
		Terminal: MayPanic,
		Factory:  FactoryABI1,
	}
	SetSummary(imported, importedSummary)

	target := newLowerTestFunc(pkg, "target")
	importedCall := newLowerTestCall(imported)
	target.Body = ir.Nodes{importedCall, newLowerTestReturn()}
	targetFunction := &Function{
		Func:     target,
		Terminal: MayPanic,
		Primary:  CoroPrimary,
		Edges: []Edge{{
			Kind: DirectCall, Callee: imported,
			CalleeName: symbolName(imported.Nname),
			Imported:   importedSummary, Node: importedCall,
		}},
		Sites: []Site{{ID: 1, Kind: SiteAwait, Node: importedCall}},
	}

	yield := newLowerTestFunc(pkg, "yield")
	caller := newLowerTestFunc(pkg, "caller")
	deferCall := newLowerTestCall(target)
	deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, deferCall)
	deferStmt.SetTypecheck(1)
	yieldCall := newLowerTestCall(yield)
	caller.Body = ir.Nodes{deferStmt, yieldCall, newLowerTestReturn()}
	callerFunction := &Function{
		Func:     caller,
		Local:    MaySuspend,
		Effect:   MaySuspend,
		Terminal: MayPanic,
		Primary:  CoroPrimary,
		Edges: []Edge{{
			Kind: DeferCall, Callee: target,
			CalleeName: symbolName(target.Nname), Node: deferCall,
		}},
		Sites: []Site{{ID: 1, Kind: SiteYield, Node: yieldCall}},
	}

	result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{
		target: targetFunction,
		caller: callerFunction,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Lowered != 1 || result.Skipped != 1 ||
		len(result.Diagnostics) != 1 ||
		!strings.Contains(result.Diagnostics[0],
			"unsupported coroutine defer dependency") {
		t.Fatalf("Lower result = %+v, want only ordinary target factory",
			result)
	}
	if targetFunction.Factory != FactoryABI1 ||
		targetFunction.Defer != NoDeferABI {
		t.Fatalf("target capabilities = factory %v, defer %v",
			targetFunction.Factory, targetFunction.Defer)
	}
	if callerFunction.Factory != NoFactory {
		t.Fatalf("caller factory = %v, want none", callerFunction.Factory)
	}
}

func TestLowerDirectDeferGoexit(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/directdefergoexit",
		"directdefergoexit")
	runtimePkg := types.NewPkg("runtime", "runtime")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	goexit := newLowerTestFunc(runtimePkg, "Goexit")
	yield := newLowerTestFunc(pkg, "yield")
	fn := newLowerTestFunc(pkg, "deferred")
	deferCall := newLowerTestCall(goexit)
	deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, deferCall)
	deferStmt.SetTypecheck(1)
	yieldCall := newLowerTestCall(yield)
	fn.Body = ir.Nodes{deferStmt, yieldCall, newLowerTestReturn()}

	function := &Function{
		Func:          fn,
		Local:         MaySuspend,
		Effect:        MaySuspend,
		LocalTerminal: MayGoexit,
		Terminal:      MayGoexit,
		Primary:       CoroPrimary,
		Edges: []Edge{{
			Kind: DeferCall, Callee: goexit,
			CalleeName: symbolName(goexit.Nname), Node: deferCall,
			Imported: FuncSummary{Terminal: MayGoexit},
			Recipe:   operationRecipes["runtime.Goexit"],
		}},
		Sites: []Site{
			{ID: 1, Kind: SiteGoexit, Node: deferCall},
			{ID: 2, Kind: SiteYield, Node: yieldCall},
		},
	}
	result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{
		fn: function,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Lowered != 1 || result.Skipped != 0 ||
		len(result.Diagnostics) != 0 {
		t.Fatalf("Lower result = %+v, want direct Goexit defer lowered",
			result)
	}

	var deferGoexit, ordinaryGoexit bool
	for _, generated := range typecheck.Target.Funcs {
		if generated == goexit {
			continue
		}
		ir.Visit(generated, func(node ir.Node) {
			call, ok := node.(*ir.CallExpr)
			if !ok {
				return
			}
			switch symbolName(ir.StaticCalleeName(call.Fun)) {
			case "runtime.coroDeferGoexit":
				deferGoexit = true
			case "runtime.Goexit":
				ordinaryGoexit = true
			}
		})
	}
	if !deferGoexit || ordinaryGoexit {
		t.Fatalf("generated calls: coroDeferGoexit=%t, Goexit=%t",
			deferGoexit, ordinaryGoexit)
	}
}

func TestRewriteDeferTerminalMismatch(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/lowerdefermismatch",
		"lowerdefermismatch")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	fn := newLowerTestFunc(pkg, "deferred")
	token := fn.NewLocal(src.NoXPos, pkg.Lookup("token"),
		types.Types[types.TUNSAFEPTR])
	recoverCall := ir.NewCallExpr(src.NoXPos, ir.ORECOVER, nil, nil)
	recoverCall.SetType(types.Types[types.TINTER])
	recoverCall.SetTypecheck(1)
	fn.Body = ir.Nodes{recoverCall}
	if err := rewriteDeferTerminal(fn, token, MayPanic); err == nil ||
		!strings.Contains(err.Error(),
			"defer terminal body is recover, want panic") {
		t.Fatalf("rewriteDeferTerminal error = %v", err)
	}
}

func TestLowerRejectsCalledTerminalDefer(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/lowerdefercalledterminal",
		"lowerdefercalledterminal")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	yield := newLowerTestFunc(pkg, "yield")
	child := newLowerTestFunc(pkg, "child")
	fn := newLowerTestFunc(pkg, "caller")
	closure := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
		types.NewSignature(nil, nil, nil), fn, typecheck.Target, 0)
	closure.DeclareParams(true)
	childCall := newLowerTestCall(child)
	closure.Body = ir.Nodes{childCall}
	deferCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
		closure.OClosure, nil)
	deferCall.SetTypecheck(1)
	deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, deferCall)
	deferStmt.SetTypecheck(1)
	yieldCall := newLowerTestCall(yield)
	fn.Body = ir.Nodes{deferStmt, yieldCall, newLowerTestReturn()}

	const terminal = MayPanic
	function := &Function{
		Func:     fn,
		Local:    MaySuspend,
		Effect:   MaySuspend,
		Terminal: terminal,
		Primary:  CoroPrimary,
		Edges: []Edge{{
			Kind: DeferCall, Callee: closure,
			CalleeName: symbolName(closure.Nname), Node: deferCall,
		}},
		Sites: []Site{{ID: 1, Kind: SiteYield, Node: yieldCall}},
	}
	closureFunction := &Function{
		Func:          closure,
		LocalTerminal: terminal,
		Terminal:      terminal,
		Primary:       CoroPrimary,
		Edges: []Edge{
			{
				Kind: GoCall, Callee: child,
				CalleeName: symbolName(child.Nname), Node: childCall,
			},
			{
				Kind: DirectCall, Callee: child,
				CalleeName: symbolName(child.Nname), Node: childCall,
			},
		},
	}
	childFunction := &Function{
		Func:          child,
		LocalTerminal: terminal,
		Terminal:      terminal,
		Primary:       CoroPrimary,
	}
	plan := &Plan{Functions: map[*ir.Func]*Function{
		fn:      function,
		closure: closureFunction,
		child:   childFunction,
	}}
	if _, err := newLowerCandidate(plan, function); err == nil ||
		!strings.Contains(err.Error(), "defer target has terminal control panic") {
		t.Fatalf("newLowerCandidate error = %v, want terminal control", err)
	}
}

func TestLowerGoexitOutcome(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/lowerterminal", "lowerterminal")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	goexit := newLowerTestFunc(pkg, "goexit")
	call := newLowerTestCall(goexit)
	fn := newLowerTestFunc(pkg, "exiting")
	fn.Body = ir.Nodes{call}
	function := &Function{
		Func:          fn,
		LocalTerminal: MayGoexit,
		Terminal:      MayGoexit,
		Primary:       CoroPrimary,
		Sites:         []Site{{ID: 1, Kind: SiteGoexit, Node: call}},
	}
	result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{fn: function}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Lowered != 1 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want one lowered function", result)
	}

	var calls []string
	for _, generated := range typecheck.Target.Funcs {
		if generated.OClosure == nil || generated.ClosureParent == nil ||
			generated.ClosureParent.Sym().Name != "exiting.coro" {
			continue
		}
		ir.Visit(generated, func(node ir.Node) {
			call, ok := node.(*ir.CallExpr)
			if !ok {
				return
			}
			name := ir.StaticCalleeName(call.Fun)
			if name != nil && name.Sym() != nil {
				calls = append(calls, name.Sym().Name)
			}
		})
	}
	for _, want := range []string{"coroGoexit", "coroTerminalAction"} {
		if !slices.Contains(calls, want) {
			t.Errorf("generated resume calls = %v, want %s", calls, want)
		}
	}
	if slices.Contains(calls, "Goexit") {
		t.Errorf("generated resume retains runtime.Goexit call: %v", calls)
	}
}

func TestLowerRejectsTerminalControl(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/lowerterminalreject",
		"lowerterminalreject")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	t.Run("invalid-panic-site", func(t *testing.T) {
		fn := newLowerTestFunc(pkg, "invalidPanic")
		ret := newLowerTestReturn()
		fn.Body = ir.Nodes{ret}
		function := &Function{
			Func: fn, Terminal: MayPanic,
			Sites: []Site{{ID: 1, Kind: SitePanic, Node: ret}},
		}
		plan := &Plan{Functions: map[*ir.Func]*Function{fn: function}}
		if _, err := newLowerCandidate(plan, function); err == nil ||
			!strings.Contains(err.Error(), "invalid panic site 1") {
			t.Fatalf("newLowerCandidate error = %v, want invalid panic site",
				err)
		}
	})

	for _, test := range []struct {
		name    string
		unknown bool
	}{
		{"panic", false},
		{"unknown", true},
	} {
		t.Run("spawn-"+test.name, func(t *testing.T) {
			child := newLowerTestFunc(pkg, "spawnChild"+test.name)
			call := newLowerTestCall(child)
			spawn := ir.NewGoDeferStmt(src.NoXPos, ir.OGO, call)
			spawn.SetTypecheck(1)
			parent := newLowerTestFunc(pkg, "spawnParent"+test.name)
			parent.Body = ir.Nodes{spawn, newLowerTestReturn()}
			edge := Edge{
				Kind: GoCall, Callee: child,
				CalleeName: symbolName(child.Nname), Node: call,
				Unknown: test.unknown,
			}
			function := &Function{
				Func: parent, Edges: []Edge{edge},
				Sites: []Site{{ID: 1, Kind: SiteSpawn, Node: call}},
			}
			functions := map[*ir.Func]*Function{parent: function}
			if !test.unknown {
				functions[child] = &Function{
					Func: child, Terminal: MayPanic,
				}
			}
			plan := &Plan{Functions: functions}
			if _, err := newLowerCandidate(plan, function); err == nil ||
				!strings.Contains(err.Error(), "spawn target may panic") {
				t.Fatalf("newLowerCandidate error = %v, want spawn rejection",
					err)
			}
		})
	}
}

func TestLowerRejectsUnsupportedDependency(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/lowerreject", "lowerreject")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	child := newLowerTestFunc(pkg, "child")
	child.Body = []ir.Node{newLowerTestReturn()}
	parent := newLowerTestFunc(pkg, "parent")
	call := newLowerTestCall(child)
	parent.Body = []ir.Node{call, newLowerTestReturn()}

	plan := &Plan{Functions: map[*ir.Func]*Function{
		child: {
			Func:    child,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteChannel, Node: child.Body[0],
			}},
		},
		parent: {
			Func:    parent,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: child, CalleeName: symbolName(child.Nname),
				Node: call,
			}},
			Sites: []Site{{
				ID: 1, Kind: SiteAwait, Node: call,
			}},
		},
	}}
	result, err := Lower(plan)
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if result.Lowered != 0 || result.Skipped != 2 {
		t.Fatalf("Lower result = %+v, want 0 lowered and 2 skipped", result)
	}
}

func TestLowerNestedSystemABIUsesFactory(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/lowernestedroot", "lowernestedroot")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	directOp := newLowerTestFunc(pkg, "directOp")
	directEntry := newLowerTestFunc(pkg, "directEntry")
	newForeignCall := func() *ir.CallExpr {
		return newLowerTestCall(directOp)
	}
	newForeignEdge := func(call *ir.CallExpr) Edge {
		return Edge{
			Kind: DirectCall, Callee: directOp,
			CalleeName: symbolName(directOp.Nname),
			Recipe: OperationRecipe{
				Kind: SiteForeign, Foreign: DirectNoBlock,
				Direct: symbolName(directEntry.Nname),
			},
			Node: call, Direct: directEntry,
		}
	}

	child := newLowerTestFunc(pkg, "child")
	childForeign := newForeignCall()
	child.Body = ir.Nodes{childForeign, newLowerTestReturn()}

	parent := newLowerTestFunc(pkg, "parent")
	parentForeign := newForeignCall()
	childCall := newLowerTestCall(child)
	parent.Body = ir.Nodes{parentForeign, childCall, newLowerTestReturn()}

	plan := &Plan{Functions: map[*ir.Func]*Function{
		child: {
			Func:    child,
			Effect:  NoSuspend,
			Exec:    NeedsSystemABI,
			Primary: CoroPrimary,
			Edges:   []Edge{newForeignEdge(childForeign)},
			Sites: []Site{{
				ID: 1, Kind: SiteForeign, Node: childForeign,
				Foreign: DirectNoBlock,
			}},
		},
		parent: {
			Func:    parent,
			Effect:  NoSuspend,
			Exec:    NeedsSystemABI,
			Primary: CoroPrimary,
			Edges: []Edge{
				newForeignEdge(parentForeign),
				{
					Kind: DirectCall, Callee: child,
					CalleeName: symbolName(child.Nname), Node: childCall,
				},
			},
			Sites: []Site{{
				ID: 1, Kind: SiteForeign, Node: parentForeign,
				Foreign: DirectNoBlock,
			}, {
				ID: 2, Kind: SiteAwait, Node: childCall,
			}},
		},
	}}
	result, err := Lower(plan)
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if result.Lowered != 2 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want 2 lowered and none skipped", result)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("Lower diagnostics = %q, want none", result.Diagnostics)
	}

	wantFactory := symbolName(child.Nname) + ".coro"
	var factoryCalls, wrapperCalls int
	for _, fn := range typecheck.Target.Funcs {
		if fn.ClosureParent == nil || fn.ClosureParent.Sym() == nil ||
			fn.ClosureParent.Sym().Name != parent.Sym().Name+".coro" {
			continue
		}
		ir.Visit(fn, func(node ir.Node) {
			call, ok := node.(*ir.CallExpr)
			if !ok {
				return
			}
			switch symbolName(ir.StaticCalleeName(ir.StaticValue(call.Fun))) {
			case wantFactory:
				factoryCalls++
			case symbolName(child.Nname):
				wrapperCalls++
			}
		})
	}
	if factoryCalls != 1 {
		t.Fatalf("generated IR has %d calls to %s, want 1",
			factoryCalls, wantFactory)
	}
	if wrapperCalls != 0 {
		t.Fatalf("generated IR has %d calls to ordinary child wrapper, want 0",
			wrapperCalls)
	}
}

func TestLowerImportedFactory(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	for _, test := range []struct {
		name       string
		factory    FactoryABI
		lowered    int
		skipped    int
		callers    int
		diagnostic string
	}{
		{
			name: "available", factory: FactoryABI1,
			lowered: 2, callers: 2,
		},
		{
			name: "missing", skipped: 1, callers: 1,
			diagnostic: "unsupported coroutine dependency",
		},
		{
			name: "unknown", factory: FactoryABI(2),
			skipped: 1, callers: 1,
			diagnostic: "unsupported coroutine dependency",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := types.NewPkg("example.com/coro/factorycaller/"+test.name,
				"factorycaller")
			leafPkg := types.NewPkg("example.com/coro/factoryleaf/"+test.name,
				"factoryleaf")
			types.LocalPkg = pkg
			typecheck.Target = new(ir.Package)
			ir.CurFunc = nil

			param := types.NewField(src.NoXPos, leafPkg.Lookup("value"),
				types.Types[types.TINT])
			resultField := types.NewField(src.NoXPos, nil,
				types.Types[types.TINT])
			leaf := ir.NewFunc(src.NoXPos, src.NoXPos,
				leafPkg.Lookup("Suspend"),
				types.NewSignature(nil, []*types.Field{param},
					[]*types.Field{resultField}))
			leaf.DeclareParams(true)
			summary := FuncSummary{
				Effect: MaySuspend, Factory: test.factory,
			}
			SetSummary(leaf, summary)

			plan := &Plan{
				Functions: make(map[*ir.Func]*Function),
			}
			var functions []*Function
			for i := 0; i < test.callers; i++ {
				call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, leaf.Nname,
					ir.Nodes{ir.NewBasicLit(src.NoXPos,
						types.Types[types.TINT], constant.MakeInt64(41))})
				call.SetType(types.Types[types.TINT])
				call.SetTypecheck(1)
				output := ir.NewNameAt(src.NoXPos,
					pkg.LookupNum("output", i), types.Types[types.TINT])
				output.Class = ir.PEXTERN
				assign := ir.NewAssignStmt(src.NoXPos, output, call)
				assign.SetTypecheck(1)

				caller := newLowerTestFunc(pkg,
					pkg.LookupNum("caller", i).Name)
				statement := ir.Node(assign)
				if test.factory == FactoryABI1 && i == 1 {
					statement = call
				}
				caller.Body = ir.Nodes{statement, newLowerTestReturn()}
				function := &Function{
					Func:    caller,
					Effect:  MaySuspend,
					Primary: CoroPrimary,
					Edges: []Edge{{
						Kind: DirectCall, Callee: leaf,
						CalleeName: symbolName(leaf.Nname),
						Imported:   summary, Node: call,
					}},
					Sites: []Site{{
						ID: 1, Kind: SiteAwait, Node: call,
					}},
				}
				plan.Functions[caller] = function
				functions = append(functions, function)
			}

			result, err := Lower(plan)
			if err != nil {
				t.Fatalf("Lower failed: %v", err)
			}
			if result.Lowered != test.lowered ||
				result.Skipped != test.skipped {
				t.Fatalf("Lower result = %+v, want %d lowered and %d skipped",
					result, test.lowered, test.skipped)
			}
			if test.diagnostic != "" {
				if len(result.Diagnostics) != 1 ||
					!strings.Contains(result.Diagnostics[0], test.diagnostic) {
					t.Fatalf("Lower diagnostics = %q, want %q",
						result.Diagnostics, test.diagnostic)
				}
				for _, function := range functions {
					if function.Factory != NoFactory {
						t.Fatalf("skipped function factory = %v, want none",
							function.Factory)
					}
				}
				return
			}
			if len(result.Diagnostics) != 0 {
				t.Fatalf("Lower diagnostics = %q, want none",
					result.Diagnostics)
			}
			for _, function := range functions {
				if function.Factory != FactoryABI1 {
					t.Fatalf("lowered function factory = %v, want v1",
						function.Factory)
				}
			}

			wantFactory := symbolName(leaf.Nname) + ".coro"
			var factoryCalls, wrapperCalls int
			for _, fn := range typecheck.Target.Funcs {
				ir.Visit(fn, func(node ir.Node) {
					call, ok := node.(*ir.CallExpr)
					if !ok {
						return
					}
					name := ir.StaticCalleeName(ir.StaticValue(call.Fun))
					switch symbolName(name) {
					case wantFactory:
						factoryCalls++
					case symbolName(leaf.Nname):
						wrapperCalls++
					}
				})
			}
			if factoryCalls != test.callers {
				t.Fatalf("generated IR has %d calls to %s, want %d",
					factoryCalls, wantFactory, test.callers)
			}
			if wrapperCalls != 0 {
				t.Fatalf("generated IR has %d calls to ordinary wrapper, want 0",
					wrapperCalls)
			}
		})
	}
}

func TestLowerRejectsSpawnResults(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/spawnresults", "spawnresults")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	result := types.NewField(src.NoXPos, nil, types.Types[types.TINT])
	child := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("child"),
		types.NewSignature(nil, nil, []*types.Field{result}))
	child.DeclareParams(true)
	call := newLowerTestCall(child)
	spawn := ir.NewGoDeferStmt(src.NoXPos, ir.OGO, call)
	spawn.SetTypecheck(1)
	parent := newLowerTestFunc(pkg, "parent")
	parent.Body = ir.Nodes{spawn, newLowerTestReturn()}
	function := &Function{
		Func: parent,
		Edges: []Edge{{
			Kind: GoCall, Callee: child,
			CalleeName: symbolName(child.Nname), Node: call,
		}},
		Sites: []Site{{
			ID: 1, Kind: SiteSpawn, Node: call,
		}},
	}
	plan := &Plan{Functions: map[*ir.Func]*Function{parent: function}}
	if _, err := newLowerCandidate(plan, function); err == nil ||
		!strings.Contains(err.Error(), "spawn target returns values") {
		t.Fatalf("newLowerCandidate error = %v, want result rejection", err)
	}
}

func TestLowerRejectsAwaitResults(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/awaitresults", "awaitresults")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	results := []*types.Field{
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
	}
	pair := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("pair"),
		types.NewSignature(nil, nil, results))
	pair.DeclareParams(true)
	single := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("single"),
		types.NewSignature(nil, nil, results[:1]))
	single.DeclareParams(true)

	for _, test := range []struct {
		name string
		body func(*ir.Func) (*ir.CallExpr, ir.Nodes)
		want string
	}{
		{
			name: "complex-normalized",
			body: func(caller *ir.Func) (*ir.CallExpr, ir.Nodes) {
				call := newLowerTestCall(pair)
				call.SetType(pair.Type().ResultsTuple())
				first := caller.NewLocal(src.NoXPos, pkg.Lookup("first"),
					types.Types[types.TINT])
				second := caller.NewLocal(src.NoXPos, pkg.Lookup("second"),
					types.Types[types.TINT])
				inner := ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC,
					ir.Nodes{first, second}, ir.Nodes{call})
				projection := ir.NewConvExpr(src.NoXPos, ir.OCONVNOP,
					types.Types[types.TINT], first)
				projection.SetInit(ir.Nodes{inner})
				target := caller.NewLocal(src.NoXPos, pkg.Lookup("target"),
					types.Types[types.TINT])
				outer := ir.NewAssignListStmt(src.NoXPos, ir.OAS2,
					ir.Nodes{ir.NewStarExpr(src.NoXPos, target), second},
					ir.Nodes{projection, second})
				return call, ir.Nodes{outer, newLowerTestReturn()}
			},
			want: "without matching assignment",
		},
		{
			name: "direct-complex",
			body: func(caller *ir.Func) (*ir.CallExpr, ir.Nodes) {
				call := newLowerTestCall(single)
				call.SetType(types.Types[types.TINT])
				target := caller.NewLocal(src.NoXPos, pkg.Lookup("pointer"),
					types.NewPtr(types.Types[types.TINT]))
				assign := ir.NewAssignStmt(src.NoXPos,
					ir.NewStarExpr(src.NoXPos, target), call)
				return call, ir.Nodes{assign, newLowerTestReturn()}
			},
			want: "result 0 is not a variable",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			caller := newLowerTestFunc(pkg, test.name)
			call, body := test.body(caller)
			caller.Body = body
			callee := pair
			if test.name == "direct-complex" {
				callee = single
			}
			function := &Function{
				Func:    caller,
				Effect:  MaySuspend,
				Primary: CoroPrimary,
				Edges: []Edge{{
					Kind: DirectCall, Callee: callee,
					CalleeName: symbolName(callee.Nname), Node: call,
				}},
				Sites: []Site{{
					ID: 1, Kind: SiteAwait, Node: call,
				}},
			}
			plan := &Plan{Functions: map[*ir.Func]*Function{
				caller: function,
			}}
			if _, err := newLowerCandidate(plan, function); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("newLowerCandidate error = %v, want %q",
					err, test.want)
			}
		})
	}
}

func TestCallResultTargets(t *testing.T) {
	prepareLowerTest(t)

	pkg := types.NewPkg("example.com/coro/resulttargets", "resulttargets")
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("call"),
		types.NewSignature(nil, nil, nil))
	call := newLowerTestCall(fn)
	if got, err := callResultTargets(call, call, 0); err != nil ||
		len(got) != 0 {
		t.Fatalf("no-result call targets = %v, %v, want none", got, err)
	}
	if got, err := callResultTargets(call, nil, 0); err != nil ||
		len(got) != 0 {
		t.Fatalf("spawn targets = %v, %v, want none", got, err)
	}
	nestedNoResult := newLowerTestCall(fn)
	if _, err := callResultTargets(call, nestedNoResult, 0); err == nil ||
		!strings.Contains(err.Error(), "nested in another statement") {
		t.Fatalf("nested no-result error = %v", err)
	}
	if got, err := callResultTargets(call, call, 1); err != nil ||
		len(got) != 1 || got[0] != nil {
		t.Fatalf("discarded call targets = %v, %v, want one empty target",
			got, err)
	}

	target := ir.NewNameAt(src.NoXPos, pkg.Lookup("target"),
		types.Types[types.TINT])
	assign := ir.NewAssignStmt(src.NoXPos, target, call)
	if got, err := callResultTargets(call, assign, 1); err != nil ||
		len(got) != 1 || got[0] != target {
		t.Fatalf("assignment targets = %v, %v, want %v", got, err, target)
	}

	list := ir.NewAssignListStmt(src.NoXPos, ir.OAS2,
		ir.Nodes{target, ir.BlankNode}, ir.Nodes{call})
	if got, err := callResultTargets(call, list, 2); err != nil ||
		len(got) != 2 || got[0] != target || got[1] != nil {
		t.Fatalf("list targets = %v, %v, want [%v <nil>]",
			got, err, target)
	}

	if _, err := callResultTargets(call, assign, 2); err == nil ||
		!strings.Contains(err.Error(), "without matching assignment") {
		t.Fatalf("mismatched target error = %v", err)
	}

	outer := newLowerTestCall(fn)
	outer.SetInit(ir.Nodes{list})
	if _, err := callResultTargets(call, outer, 2); err == nil ||
		!strings.Contains(err.Error(), "without matching assignment") {
		t.Fatalf("nested target error = %v", err)
	}

	multiCall := newLowerTestCall(fn)
	second := ir.NewNameAt(src.NoXPos, pkg.Lookup("second"),
		types.Types[types.TINT])
	inner := ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC,
		ir.Nodes{target, second}, ir.Nodes{multiCall})
	projection := ir.NewConvExpr(src.NoXPos, ir.OCONVNOP,
		types.Types[types.TINT], target)
	projection.SetInit(ir.Nodes{inner})
	outerTarget := ir.NewNameAt(src.NoXPos, pkg.Lookup("outer"),
		types.Types[types.TINT])
	normalized := ir.NewAssignListStmt(src.NoXPos, ir.OAS2,
		ir.Nodes{outerTarget, ir.BlankNode}, ir.Nodes{projection, second})
	if got, err := callResultTargets(multiCall, normalized, 2); err != nil ||
		len(got) != 2 || got[0] != target || got[1] != second {
		t.Fatalf("normalized targets = %v, %v, want [%v %v]",
			got, err, target, second)
	}

	inner.Lhs[1] = ir.BlankNode
	normalized.Rhs[1] = ir.BlankNode
	if got, err := callResultTargets(multiCall, normalized, 2); err != nil ||
		len(got) != 2 || got[0] != target || got[1] != nil {
		t.Fatalf("normalized blank targets = %v, %v, want [%v <nil>]",
			got, err, target)
	}
	inner.Lhs[1] = second
	normalized.Rhs[1] = second

	missing := ir.NewAssignListStmt(src.NoXPos, ir.OAS2,
		ir.Nodes{outerTarget, ir.BlankNode}, ir.Nodes{target, second})
	if _, err := callResultTargets(multiCall, missing, 2); err == nil ||
		!strings.Contains(err.Error(), "without matching assignment") {
		t.Fatalf("missing normalized assignment error = %v", err)
	}
	notInitialized := ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC,
		ir.Nodes{outerTarget}, ir.Nodes{multiCall})
	if _, ok := normalizedResultAssignment(multiCall, notInitialized, 1); ok {
		t.Fatal("direct assignment is a normalized result assignment")
	}

	normalized.Rhs[1] = target
	if _, err := callResultTargets(multiCall, normalized, 2); err == nil ||
		!strings.Contains(err.Error(), "without matching assignment") {
		t.Fatalf("mismatched normalized projection error = %v", err)
	}
	normalized.Rhs[1] = second

	normalized.Lhs[0] = ir.NewStarExpr(src.NoXPos, outerTarget)
	if _, err := callResultTargets(multiCall, normalized, 2); err == nil ||
		!strings.Contains(err.Error(), "without matching assignment") {
		t.Fatalf("complex normalized target error = %v", err)
	}
	normalized.Lhs[0] = outerTarget

	unsupported := ir.NewConvExpr(src.NoXPos, ir.OCONVIFACE,
		types.Types[types.TINT], target)
	if isResultProjection(unsupported, target) {
		t.Fatal("interface conversion is a result projection")
	}

	if got := takeCallInit(normalized, multiCall); len(got) != 1 ||
		got[0] != inner {
		t.Fatalf("normalized call init = %v, want [%v]", got, inner)
	}
	if got := takeCallInit(normalized, multiCall); len(got) != 0 {
		t.Fatalf("normalized call init after take = %v, want none", got)
	}
}

func TestSupportsAwaitInit(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/awaitinit", "awaitinit")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	fn := newLowerTestFunc(pkg, "call")
	target := ir.NewNameAt(src.NoXPos, pkg.Lookup("target"),
		types.Types[types.TINT])
	complex := ir.NewStarExpr(src.NoXPos, target)

	tests := []struct {
		name string
		stmt ir.Node
		want bool
	}{
		{"call", newLowerTestCall(fn), true},
		{"return", newLowerTestReturn(), true},
		{"assignment", ir.NewAssignStmt(src.NoXPos, target, ir.NewInt(src.NoXPos, 1)), true},
		{"complex-assignment", ir.NewAssignStmt(src.NoXPos, complex, ir.NewInt(src.NoXPos, 1)), false},
		{"assignment-list", ir.NewAssignListStmt(src.NoXPos, ir.OAS2,
			ir.Nodes{target, ir.BlankNode}, ir.Nodes{ir.NewInt(src.NoXPos, 1)}), true},
		{"complex-assignment-list", ir.NewAssignListStmt(src.NoXPos, ir.OAS2,
			ir.Nodes{complex}, ir.Nodes{ir.NewInt(src.NoXPos, 1)}), false},
		{"other", ir.NewIfStmt(src.NoXPos, ir.NewBool(src.NoXPos, true), nil, nil), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := supportsAwaitInit(test.stmt); got != test.want {
				t.Fatalf("supportsAwaitInit(%v) = %t, want %t",
					test.stmt.Op(), got, test.want)
			}
		})
	}
}

func TestLowerRejectsUnsupportedControl(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/lowercontrol", "lowercontrol")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	yield := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("yield"),
		types.NewSignature(nil, nil, nil))
	yield.DeclareParams(true)
	branch := func() ir.Node {
		return ir.NewBranchStmt(src.NoXPos, ir.OBREAK, nil)
	}
	cases := []struct {
		name string
		body func(*ir.CallExpr) ir.Nodes
		want string
	}{
		{
			name: "init",
			body: func(call *ir.CallExpr) ir.Nodes {
				call.SetInit(ir.Nodes{branch()})
				return ir.Nodes{call}
			},
			want: "control flow break",
		},
		{
			name: "defer",
			body: func(call *ir.CallExpr) ir.Nodes {
				return ir.Nodes{ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, call)}
			},
			want: "suspending defer",
		},
		{
			name: "non-call-defer",
			body: func(call *ir.CallExpr) ir.Nodes {
				return ir.Nodes{ir.NewGoDeferStmt(src.NoXPos,
					ir.ODEFER, call.Fun)}
			},
			want: "non-call go or defer statement",
		},
		{
			name: "block",
			body: func(*ir.CallExpr) ir.Nodes {
				return ir.Nodes{ir.NewBlockStmt(src.NoXPos, ir.Nodes{branch()})}
			},
			want: "control flow break",
		},
		{
			name: "if-body",
			body: func(*ir.CallExpr) ir.Nodes {
				return ir.Nodes{ir.NewIfStmt(src.NoXPos, nil,
					ir.Nodes{branch()}, nil)}
			},
			want: "control flow break",
		},
		{
			name: "if-else",
			body: func(*ir.CallExpr) ir.Nodes {
				return ir.Nodes{ir.NewIfStmt(src.NoXPos, nil,
					nil, ir.Nodes{branch()})}
			},
			want: "control flow break",
		},
		{
			name: "labeled-for",
			body: func(*ir.CallExpr) ir.Nodes {
				loop := ir.NewForStmt(src.NoXPos, nil, nil, nil, nil, false)
				loop.Label = pkg.Lookup("loop")
				return ir.Nodes{loop}
			},
			want: "labeled for loop",
		},
		{
			name: "for-post",
			body: func(*ir.CallExpr) ir.Nodes {
				return ir.Nodes{ir.NewForStmt(src.NoXPos, nil, nil,
					branch(), nil, false)}
			},
			want: "control flow break",
		},
		{
			name: "for-body",
			body: func(*ir.CallExpr) ir.Nodes {
				return ir.Nodes{ir.NewForStmt(src.NoXPos, nil, nil,
					nil, ir.Nodes{branch()}, false)}
			},
			want: "control flow break",
		},
		{
			name: "non-normalized-defer",
			body: func(call *ir.CallExpr) ir.Nodes {
				call.Args = ir.Nodes{ir.NewBasicLit(src.NoXPos,
					types.Types[types.TINT], constant.MakeInt64(1))}
				return ir.Nodes{ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, call)}
			},
			want: "non-normalized defer call",
		},
		{
			name: "defer-in-switch",
			body: func(call *ir.CallExpr) ir.Nodes {
				deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, call)
				clause := ir.NewCaseStmt(src.NoXPos, nil, ir.Nodes{deferStmt})
				return ir.Nodes{ir.NewSwitchStmt(src.NoXPos, nil,
					[]*ir.CaseClause{clause})}
			},
			want: "defer in unsupported control flow switch",
		},
		{
			name: "return",
			body: func(*ir.CallExpr) ir.Nodes {
				return ir.Nodes{ir.NewReturnStmt(src.NoXPos, ir.Nodes{
					ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
						constant.MakeInt64(1)),
				})}
			},
			want: "return has 1 results, want 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := newLowerTestFunc(pkg, tc.name)
			call := newLowerTestCall(yield)
			fn.Body = tc.body(call)
			function := &Function{
				Func: fn,
				Sites: []Site{{
					ID: 1, Kind: SiteYield, Node: call,
				}},
			}
			plan := &Plan{Functions: map[*ir.Func]*Function{fn: function}}
			_, err := newLowerCandidate(plan, function)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("newLowerCandidate error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLowerRejectsUnsupportedDefers(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/lowerdefer", "lowerdefer")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	tests := []struct {
		name    string
		edge    func(*ir.Func, *ir.CallExpr) []Edge
		closure bool
		loop    bool
		want    string
	}{
		{
			name: "missing-plan",
			edge: func(*ir.Func, *ir.CallExpr) []Edge {
				return nil
			},
			want: "defer has no static call plan",
		},
		{
			name: "wrong-edge-kind",
			edge: func(callee *ir.Func, call *ir.CallExpr) []Edge {
				return []Edge{{
					Kind: DirectCall, Callee: callee,
					CalleeName: symbolName(callee.Nname), Node: call,
				}}
			},
			want: "defer has no static call plan",
		},
		{
			name: "unknown-effect",
			edge: func(callee *ir.Func, call *ir.CallExpr) []Edge {
				return []Edge{{
					Kind: DeferCall, Callee: callee,
					CalleeName: symbolName(callee.Nname), Unknown: true,
					Node: call,
				}}
			},
			want: "defer target has unknown effects",
		},
		{
			name: "suspending",
			edge: func(callee *ir.Func, call *ir.CallExpr) []Edge {
				return []Edge{{
					Kind: DeferCall, Callee: callee,
					CalleeName: symbolName(callee.Nname),
					Imported:   FuncSummary{Effect: MaySuspend}, Node: call,
				}}
			},
			want: "suspending defer",
		},
		{
			name: "execution-constraint",
			edge: func(callee *ir.Func, call *ir.CallExpr) []Edge {
				return []Edge{{
					Kind: DeferCall, Callee: callee,
					CalleeName: symbolName(callee.Nname),
					Imported:   FuncSummary{Exec: NeedsPreempt}, Node: call,
				}}
			},
			want: "defer target has execution constraints preempt",
		},
		{
			name: "terminal-control",
			edge: func(callee *ir.Func, call *ir.CallExpr) []Edge {
				return []Edge{{
					Kind: DeferCall, Callee: callee,
					CalleeName: symbolName(callee.Nname),
					Imported:   FuncSummary{Terminal: MayPanic}, Node: call,
				}}
			},
			want: "defer target has terminal control panic",
		},
		{
			name:    "terminal-literal-without-local-plan",
			closure: true,
			edge: func(callee *ir.Func, call *ir.CallExpr) []Edge {
				return []Edge{{
					Kind: DeferCall, Callee: callee,
					CalleeName: symbolName(callee.Nname),
					Imported:   FuncSummary{Terminal: UsesRecover}, Node: call,
				}}
			},
			want: "defer target has terminal control recover",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			yield := newLowerTestFunc(pkg, tc.name+"Yield")
			callee := newLowerTestFunc(pkg, tc.name+"Cleanup")
			var fun ir.Node = callee.Nname
			if tc.closure {
				callee = ir.NewClosureFunc(src.NoXPos, src.NoXPos,
					ir.OCLOSURE, types.NewSignature(nil, nil, nil),
					newLowerTestFunc(pkg, tc.name+"Outer"),
					typecheck.Target, 0)
				callee.DeclareParams(true)
				fun = callee.OClosure
			}
			deferCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, fun, nil)
			deferCall.SetTypecheck(1)
			deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER, deferCall)
			deferStmt.SetTypecheck(1)
			yieldCall := newLowerTestCall(yield)
			fn := newLowerTestFunc(pkg, tc.name)
			deferBody := ir.Node(deferStmt)
			if tc.loop {
				deferBody = ir.NewForStmt(src.NoXPos, nil,
					ir.NewBool(src.NoXPos, false), nil,
					ir.Nodes{deferStmt}, false)
			}
			fn.Body = ir.Nodes{deferBody, yieldCall, newLowerTestReturn()}
			function := &Function{
				Func:    fn,
				Local:   MaySuspend,
				Effect:  MaySuspend,
				Primary: CoroPrimary,
				Edges:   tc.edge(callee, deferCall),
				Sites: []Site{{
					ID: 1, Kind: SiteYield, Node: yieldCall,
				}},
			}
			plan := &Plan{Functions: map[*ir.Func]*Function{fn: function}}
			_, err := newLowerCandidate(plan, function)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("newLowerCandidate error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLowerRepeatedSourceLiteral(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/repeatedliteral",
		"repeatedliteral")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	newCandidate := func(nested, missingType bool) (*lowerCandidate, error) {
		fn := newLowerTestFunc(pkg, "caller")
		sourceType := types.Types[types.TINT]
		if missingType {
			sourceType = nil
		}
		source := fn.NewLocal(src.NoXPos, pkg.Lookup("source"),
			sourceType)
		literal := ir.NewClosureFunc(src.NoXPos, src.NoXPos,
			ir.OCLOSURE, types.NewSignature(nil, nil, nil),
			fn, typecheck.Target, 0)
		literal.DeclareParams(true)
		captured := ir.NewClosureVar(src.NoXPos, literal, source)

		target := ir.NewNameAt(src.NoXPos, pkg.Lookup("target"),
			types.Types[types.TINT])
		target.Class = ir.PEXTERN
		body := ir.Node(ir.NewAssignStmt(src.NoXPos, target, captured))
		if nested {
			closure := ir.NewClosureFunc(src.NoXPos, src.NoXPos,
				ir.OCLOSURE, types.NewSignature(nil, nil, nil),
				literal, typecheck.Target, 0)
			closure.DeclareParams(true)
			nestedCapture := ir.NewClosureVar(src.NoXPos, closure, captured)
			closure.Body = ir.Nodes{
				ir.NewAssignStmt(src.NoXPos, target, nestedCapture),
			}
			call := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
				closure.OClosure, nil)
			call.SetTypecheck(1)
			body = call
		}
		literal.Body = ir.Nodes{body}

		deferCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC,
			literal.OClosure, nil)
		deferCall.SetTypecheck(1)
		deferStmt := ir.NewGoDeferStmt(src.NoXPos, ir.ODEFER,
			deferCall)
		deferStmt.SetTypecheck(1)
		loop := ir.NewForStmt(src.NoXPos, nil,
			ir.NewBool(src.NoXPos, false), nil,
			ir.Nodes{deferStmt}, false)
		yield := newLowerTestFunc(pkg, "yield")
		yieldCall := newLowerTestCall(yield)
		fn.Body = ir.Nodes{loop, yieldCall, newLowerTestReturn()}

		function := &Function{
			Func:    fn,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DeferCall, Callee: literal,
				CalleeName: symbolName(literal.Nname), Node: deferCall,
			}},
			Sites: []Site{{
				ID: 1, Kind: SiteYield, Node: yieldCall,
			}},
		}
		plan := &Plan{Functions: map[*ir.Func]*Function{fn: function}}
		return newLowerCandidate(plan, function)
	}

	candidate, err := newCandidate(false, false)
	if err != nil {
		t.Fatalf("newLowerCandidate rejected repeated literal: %v", err)
	}
	if len(candidate.defers) != 1 ||
		!candidate.defers[0].sourceLiteral ||
		len(candidate.defers[0].literalCaptures) != 1 {
		t.Fatalf("repeated literal candidate = %+v", candidate.defers)
	}

	if _, err := newCandidate(true, false); err == nil ||
		!strings.Contains(err.Error(), "nested repeated source defer capture") {
		t.Fatalf("nested repeated literal error = %v", err)
	}
	if _, err := newCandidate(false, true); err == nil ||
		!strings.Contains(err.Error(),
			"repeated source defer capture has no type") {
		t.Fatalf("untyped repeated literal error = %v", err)
	}
}

func TestLowerParametersAndResults(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/lowerresult", "lowerresult")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	yield := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("yield"),
		types.NewSignature(nil, nil, nil))
	yield.DeclareParams(true)

	param := types.NewField(src.NoXPos, pkg.Lookup("value"), types.Types[types.TINT])
	resultField := types.NewField(src.NoXPos, nil, types.Types[types.TINT])
	leaf := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("leaf"),
		types.NewSignature(nil, []*types.Field{param}, []*types.Field{resultField}))
	leaf.DeclareParams(true)
	yieldCall := newLowerTestCall(yield)
	returnValue, _ := param.Nname.(ir.Node)
	ret := ir.NewReturnStmt(src.NoXPos, []ir.Node{returnValue})
	ret.SetTypecheck(1)
	leaf.Body = []ir.Node{yieldCall, ret}

	caller := newLowerTestFunc(pkg, "caller")
	leafCall := ir.NewCallExpr(src.NoXPos, ir.OCALLFUNC, leaf.Nname,
		[]ir.Node{ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(42))})
	leafCall.SetType(types.Types[types.TINT])
	leafCall.SetTypecheck(1)
	output := ir.NewNameAt(src.NoXPos, pkg.Lookup("output"), types.Types[types.TINT])
	output.Class = ir.PEXTERN
	assign := ir.NewAssignStmt(src.NoXPos, output, leafCall)
	assign.SetTypecheck(1)
	caller.Body = []ir.Node{assign, newLowerTestReturn()}

	plan := &Plan{Functions: map[*ir.Func]*Function{
		leaf: {
			Func:    leaf,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteYield, Node: yieldCall,
			}},
		},
		caller: {
			Func:    caller,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: leaf, CalleeName: symbolName(leaf.Nname),
				Node: leafCall,
			}},
			Sites: []Site{{
				ID: 1, Kind: SiteAwait, Node: leafCall,
			}},
		},
	}}
	result, err := Lower(plan)
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if result.Lowered != 2 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want 2 lowered and 0 skipped", result)
	}
}

func TestLowerAcceptsAwaitInStructuredBody(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/structuredawait",
		"structuredawait")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	resultField := types.NewField(src.NoXPos, nil,
		types.Types[types.TINT])
	leaf := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("leaf"),
		types.NewSignature(nil, nil, []*types.Field{resultField}))
	leaf.DeclareParams(true)
	caller := newLowerTestFunc(pkg, "caller")
	target := caller.NewLocal(src.NoXPos, pkg.Lookup("target"),
		types.Types[types.TINT])
	decl := ir.NewDecl(src.NoXPos, ir.ODCL, target)
	call := newLowerTestCall(leaf)
	call.SetType(types.Types[types.TINT])
	assign := ir.NewAssignStmt(src.NoXPos, target, call)
	assign.SetTypecheck(1)
	ifStmt := ir.NewIfStmt(src.NoXPos,
		ir.NewBool(src.NoXPos, true), ir.Nodes{decl, assign}, nil)
	ifStmt.SetTypecheck(1)
	block := ir.NewBlockStmt(src.NoXPos, ir.Nodes{ifStmt})
	block.SetTypecheck(1)
	loop := ir.NewForStmt(src.NoXPos, nil,
		ir.NewBool(src.NoXPos, false), nil, ir.Nodes{block}, false)
	loop.SetTypecheck(1)
	caller.Body = ir.Nodes{loop, newLowerTestReturn()}

	function := &Function{
		Func:    caller,
		Effect:  MaySuspend,
		Primary: CoroPrimary,
		Edges: []Edge{{
			Kind: DirectCall, Callee: leaf,
			CalleeName: symbolName(leaf.Nname), Node: call,
		}},
		Sites: []Site{{
			ID: 1, Kind: SiteAwait, Node: call,
		}},
	}
	plan := &Plan{Functions: map[*ir.Func]*Function{
		caller: function,
		leaf: {
			Func:    leaf,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
		},
	}}
	candidate, err := newLowerCandidate(plan, function)
	if err != nil {
		t.Fatalf("newLowerCandidate rejected structured await: %v", err)
	}
	if candidate.transitions[call] != SiteAwait {
		t.Fatalf("structured await transition = %v, want await",
			candidate.transitions[call])
	}
}

func TestLowerNormalizedResults(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/normalizedresult",
		"normalizedresult")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	yield := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("yield"),
		types.NewSignature(nil, nil, nil))
	yield.DeclareParams(true)

	resultFields := []*types.Field{
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
		types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
	}
	leaf := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("leaf"),
		types.NewSignature(nil, nil, resultFields))
	leaf.DeclareParams(true)
	typecheck.Target.Funcs = append(typecheck.Target.Funcs, leaf)
	yieldCall := newLowerTestCall(yield)
	ret := ir.NewReturnStmt(src.NoXPos, ir.Nodes{
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(1)),
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(2)),
	})
	ret.SetTypecheck(1)
	leaf.Body = ir.Nodes{yieldCall, ret}

	caller := newLowerTestFunc(pkg, "caller")
	first := caller.NewLocal(src.NoXPos, pkg.Lookup("first"),
		types.Types[types.TINT])
	second := caller.NewLocal(src.NoXPos, pkg.Lookup("second"),
		types.Types[types.TINT])
	resultFirst := caller.NewLocal(src.NoXPos, pkg.Lookup("resultFirst"),
		types.Types[types.TINT])
	resultSecond := caller.NewLocal(src.NoXPos, pkg.Lookup("resultSecond"),
		types.Types[types.TINT])
	directFirst := caller.NewLocal(src.NoXPos, pkg.Lookup("directFirst"),
		types.Types[types.TINT])
	directSecond := caller.NewLocal(src.NoXPos, pkg.Lookup("directSecond"),
		types.Types[types.TINT])
	initTemp := caller.NewLocal(src.NoXPos, pkg.Lookup("initTemp"),
		types.Types[types.TINT])

	leafCall := newLowerTestCall(leaf)
	leafCall.SetType(leaf.Type().ResultsTuple())
	inner := ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC,
		ir.Nodes{first, second}, ir.Nodes{leafCall})
	inner.SetTypecheck(1)
	firstProjection := ir.NewConvExpr(src.NoXPos, ir.OCONVNOP,
		types.Types[types.TINT], first)
	firstProjection.SetTypecheck(1)
	firstProjection.SetInit(ir.Nodes{inner})
	outer := ir.NewAssignListStmt(src.NoXPos, ir.OAS2,
		ir.Nodes{resultFirst, resultSecond},
		ir.Nodes{firstProjection, second})
	outer.SetTypecheck(1)
	outer.SetInit(ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, initTemp)})
	discardCall := newLowerTestCall(leaf)
	discardCall.SetType(leaf.Type().ResultsTuple())
	prelude := ir.NewAssignStmt(src.NoXPos, resultFirst,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(0)))
	prelude.SetTypecheck(1)
	prelude.SetInit(ir.Nodes{discardCall})
	directCall := newLowerTestCall(leaf)
	directCall.SetType(leaf.Type().ResultsTuple())
	direct := ir.NewAssignListStmt(src.NoXPos, ir.OAS2FUNC,
		ir.Nodes{directFirst, directSecond}, ir.Nodes{directCall})
	direct.SetTypecheck(1)
	caller.Body = ir.Nodes{prelude, direct, outer, newLowerTestReturn()}

	plan := &Plan{Functions: map[*ir.Func]*Function{
		leaf: {
			Func:    leaf,
			Local:   MaySuspend,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Sites: []Site{{
				ID: 1, Kind: SiteYield, Node: yieldCall,
			}},
		},
		caller: {
			Func:    caller,
			Effect:  MaySuspend,
			Primary: CoroPrimary,
			Edges: []Edge{{
				Kind: DirectCall, Callee: leaf,
				CalleeName: symbolName(leaf.Nname), Node: leafCall,
			}, {
				Kind: DirectCall, Callee: leaf,
				CalleeName: symbolName(leaf.Nname), Node: discardCall,
			}, {
				Kind: DirectCall, Callee: leaf,
				CalleeName: symbolName(leaf.Nname), Node: directCall,
			}},
			Sites: []Site{
				{ID: 1, Kind: SiteAwait, Node: discardCall},
				{ID: 2, Kind: SiteAwait, Node: directCall},
				{ID: 3, Kind: SiteAwait, Node: leafCall},
			},
		},
	}}
	result, err := Lower(plan)
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if result.Lowered != 2 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want 2 lowered and 0 skipped", result)
	}

	wantFactory := symbolName(leaf.Nname) + ".coro"
	var factoryCalls, wrapperCalls int
	for _, fn := range typecheck.Target.Funcs {
		ir.Visit(fn, func(node ir.Node) {
			call, ok := node.(*ir.CallExpr)
			if !ok {
				return
			}
			switch symbolName(ir.StaticCalleeName(ir.StaticValue(call.Fun))) {
			case wantFactory:
				factoryCalls++
			case symbolName(leaf.Nname):
				wrapperCalls++
			}
		})
	}
	if factoryCalls != 4 {
		t.Fatalf("generated IR has %d calls to %s, want 4",
			factoryCalls, wantFactory)
	}
	if wrapperCalls != 0 {
		t.Fatalf("generated IR has %d calls to ordinary wrapper, want 0",
			wrapperCalls)
	}
}

func TestLowerChannelOperations(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/channel", "channel")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	fn := newLowerTestFunc(pkg, "operations")
	channelType := types.NewChan(types.Types[types.TINT], types.Cboth)
	channel := fn.NewLocal(src.NoXPos, pkg.Lookup("channel"), channelType)
	channelDecl := ir.NewDecl(src.NoXPos, ir.ODCL, channel)
	boolName := ir.NewDeclNameAt(src.NoXPos, ir.OTYPE,
		pkg.Lookup("definedBool"))
	definedBool := types.NewNamed(boolName)
	boolName.SetType(definedBool)
	boolName.SetTypecheck(1)
	definedBool.SetUnderlying(types.Types[types.TBOOL])

	send := ir.NewSendStmt(src.NoXPos, channel,
		ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
			constant.MakeInt64(1)))
	send.SetTypecheck(1)

	directTarget := fn.NewLocal(src.NoXPos, pkg.Lookup("directTarget"),
		types.Types[types.TINT])
	directRecv := ir.NewUnaryExpr(src.NoXPos, ir.ORECV, channel)
	directRecv.SetType(types.Types[types.TINT])
	directRecv.SetTypecheck(1)
	direct := ir.NewAssignStmt(src.NoXPos, directTarget, directRecv)
	direct.SetTypecheck(1)

	singleTemp := fn.NewLocal(src.NoXPos, pkg.Lookup("singleTemp"),
		types.Types[types.TINT])
	singleTarget := fn.NewLocal(src.NoXPos, pkg.Lookup("singleTarget"),
		types.Types[types.TINT])
	singleRecv := ir.NewUnaryExpr(src.NoXPos, ir.ORECV, channel)
	singleRecv.SetType(types.Types[types.TINT])
	singleRecv.SetTypecheck(1)
	singleInner := ir.NewAssignStmt(src.NoXPos, singleTemp, singleRecv)
	singleInner.SetTypecheck(1)
	singleProjection := ir.NewConvExpr(src.NoXPos, ir.OCONVNOP,
		types.Types[types.TINT], singleTemp)
	singleProjection.SetTypecheck(1)
	singleProjection.SetInit(ir.Nodes{singleInner})
	singleOuter := ir.NewAssignStmt(src.NoXPos, singleTarget,
		singleProjection)
	singleOuter.SetTypecheck(1)

	pairValueTemp := fn.NewLocal(src.NoXPos, pkg.Lookup("pairValueTemp"),
		types.Types[types.TINT])
	pairOKTemp := fn.NewLocal(src.NoXPos, pkg.Lookup("pairOKTemp"),
		definedBool)
	pairValue := fn.NewLocal(src.NoXPos, pkg.Lookup("pairValue"),
		types.Types[types.TINT])
	pairOK := fn.NewLocal(src.NoXPos, pkg.Lookup("pairOK"),
		definedBool)
	pairRecv := ir.NewUnaryExpr(src.NoXPos, ir.ORECV, channel)
	pairRecv.SetType(types.Types[types.TINT])
	pairRecv.SetTypecheck(1)
	pairInner := ir.NewAssignListStmt(src.NoXPos, ir.OAS2RECV,
		ir.Nodes{pairValueTemp, pairOKTemp}, ir.Nodes{pairRecv})
	pairInner.SetTypecheck(1)
	pairProjection := ir.NewConvExpr(src.NoXPos, ir.OCONVNOP,
		types.Types[types.TINT], pairValueTemp)
	pairProjection.SetTypecheck(1)
	pairProjection.SetInit(ir.Nodes{pairInner})
	pairOuter := ir.NewAssignListStmt(src.NoXPos, ir.OAS2,
		ir.Nodes{pairValue, pairOK},
		ir.Nodes{pairProjection, pairOKTemp})
	pairOuter.SetTypecheck(1)

	discard := ir.NewUnaryExpr(src.NoXPos, ir.ORECV, channel)
	discard.SetType(types.Types[types.TINT])
	discard.SetTypecheck(1)

	fn.Body = ir.Nodes{
		channelDecl, send, direct, singleOuter, pairOuter, discard,
		newLowerTestReturn(),
	}
	function := &Function{
		Func:    fn,
		Local:   MaySuspend,
		Effect:  MaySuspend,
		Primary: CoroPrimary,
		Sites: []Site{
			{ID: 1, Kind: SiteChannel, Node: send},
			{ID: 2, Kind: SiteChannel, Node: directRecv},
			{ID: 3, Kind: SiteChannel, Node: singleRecv},
			{ID: 4, Kind: SiteChannel, Node: pairRecv},
			{ID: 5, Kind: SiteChannel, Node: discard},
		},
	}
	result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{
		fn: function,
	}})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if result.Lowered != 1 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want one lowered function", result)
	}

	var sends, receives, nativeChannels int
	for _, generated := range typecheck.Target.Funcs {
		ir.Visit(generated, func(node ir.Node) {
			switch node.Op() {
			case ir.OSEND, ir.ORECV:
				nativeChannels++
			}
			call, ok := node.(*ir.CallExpr)
			if !ok {
				return
			}
			name := ir.StaticCalleeName(call.Fun)
			if name == nil || name.Sym() == nil {
				return
			}
			switch name.Sym().Name {
			case "coroChanSend":
				sends++
			case "coroChanRecv":
				receives++
			}
		})
	}
	if sends != 1 || receives != 4 {
		t.Fatalf("generated channel helpers = (%d send, %d receive), want (1, 4)",
			sends, receives)
	}
	if nativeChannels != 0 {
		t.Fatalf("generated IR retains %d native channel operations",
			nativeChannels)
	}
}

func TestLowerChannelRange(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/channelrange", "channelrange")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	fn := newLowerTestFunc(pkg, "rangeChannel")
	elementType := types.NewPtr(types.Types[types.TINT])
	channelType := types.NewChan(elementType, types.Cboth)
	channel := fn.NewLocal(src.NoXPos, pkg.Lookup("channel"), channelType)
	key := fn.NewLocal(src.NoXPos, pkg.Lookup("key"),
		elementType)
	rangeKey := fn.NewLocal(src.NoXPos, pkg.Lookup("rangeKey"),
		elementType)
	copyKey := ir.NewAssignStmt(src.NoXPos, key, rangeKey)
	copyKey.Def = true
	copyKey.SetTypecheck(1)
	copyKey.SetInit(ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, key)})
	loop := ir.NewRangeStmt(src.NoXPos, rangeKey, nil, channel,
		ir.Nodes{copyKey}, false)
	loop.SetTypecheck(1)

	interfaceKey := fn.NewLocal(src.NoXPos, pkg.Lookup("interfaceKey"),
		types.Types[types.TINTER])
	convertedLoop := ir.NewRangeStmt(src.NoXPos, interfaceKey, nil, channel,
		nil, false)
	convertedLoop.SetTypecheck(1)
	fn.Body = ir.Nodes{
		ir.NewDecl(src.NoXPos, ir.ODCL, channel),
		loop,
		convertedLoop,
		newLowerTestReturn(),
	}
	function := &Function{
		Func:    fn,
		Local:   MaySuspend,
		Effect:  MaySuspend,
		Primary: CoroPrimary,
		Sites: []Site{
			{ID: 1, Kind: SiteChannel, Node: loop},
			{ID: 2, Kind: SiteChannel, Node: convertedLoop},
		},
	}
	result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{
		fn: function,
	}})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if result.Lowered != 1 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want one lowered function", result)
	}

	var receives, nativeRanges, nativeReceives int
	for _, generated := range typecheck.Target.Funcs {
		ir.Visit(generated, func(node ir.Node) {
			switch node.Op() {
			case ir.ORANGE:
				nativeRanges++
			case ir.ORECV:
				nativeReceives++
			}
			call, ok := node.(*ir.CallExpr)
			if !ok {
				return
			}
			name := ir.StaticCalleeName(call.Fun)
			if name != nil && name.Sym() != nil &&
				name.Sym().Name == "coroChanRecv" {
				receives++
			}
		})
	}
	if receives != 2 {
		t.Fatalf("generated IR has %d channel receives, want 2", receives)
	}
	if nativeRanges != 0 || nativeReceives != 0 {
		t.Fatalf("generated IR retains %d ranges and %d receives",
			nativeRanges, nativeReceives)
	}
}

func TestLowerChannelSelect(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	oldCurFunc := ir.CurFunc
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
		ir.CurFunc = oldCurFunc
	}()

	pkg := types.NewPkg("example.com/coro/channelselect", "channelselect")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)

	fn := newLowerTestFunc(pkg, "selectChannel")
	channelType := types.NewChan(types.Types[types.TINT], types.Cboth)
	sendChannel := fn.NewLocal(src.NoXPos, pkg.Lookup("sendChannel"),
		channelType)
	recvChannel := fn.NewLocal(src.NoXPos, pkg.Lookup("recvChannel"),
		channelType)
	sendValue := fn.NewLocal(src.NoXPos, pkg.Lookup("sendValue"),
		types.Types[types.TINT])
	recvValue := fn.NewLocal(src.NoXPos, pkg.Lookup("recvValue"),
		types.Types[types.TINT])
	recvOK := fn.NewLocal(src.NoXPos, pkg.Lookup("recvOK"),
		types.Types[types.TBOOL])

	send := ir.NewSendStmt(src.NoXPos, sendChannel, sendValue)
	recv := ir.NewUnaryExpr(src.NoXPos, ir.ORECV, recvChannel)
	recv.SetType(types.Types[types.TINT])
	recvAssign := ir.NewAssignListStmt(src.NoXPos, ir.OSELRECV2,
		ir.Nodes{recvValue, recvOK}, ir.Nodes{recv})
	sendCase := ir.NewCommStmt(src.NoXPos, send, nil)
	recvCase := ir.NewCommStmt(src.NoXPos, recvAssign, nil)
	defaultCase := ir.NewCommStmt(src.NoXPos, nil, nil)
	selectStmt := ir.NewSelectStmt(src.NoXPos,
		[]*ir.CommClause{recvCase, defaultCase, sendCase})
	fn.Body = ir.Nodes{selectStmt, newLowerTestReturn()}

	function := &Function{
		Func:    fn,
		Local:   MaySuspend,
		Effect:  MaySuspend,
		Primary: CoroPrimary,
		Sites: []Site{
			{ID: 1, Kind: SiteChannel, Node: selectStmt},
			{ID: 2, Kind: SiteChannel, Node: recv},
			{ID: 3, Kind: SiteChannel, Node: send},
		},
	}
	result, err := Lower(&Plan{Functions: map[*ir.Func]*Function{
		fn: function,
	}})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if result.Lowered != 1 || result.Skipped != 0 {
		t.Fatalf("Lower result = %+v, want one lowered function", result)
	}

	var selects, nativeSelects, nativeChannels int
	for _, generated := range typecheck.Target.Funcs {
		ir.Visit(generated, func(node ir.Node) {
			switch node.Op() {
			case ir.OSELECT:
				nativeSelects++
			case ir.OSEND, ir.ORECV:
				nativeChannels++
			}
			call, ok := node.(*ir.CallExpr)
			if !ok {
				return
			}
			name := ir.StaticCalleeName(call.Fun)
			if name != nil && name.Sym() != nil &&
				name.Sym().Name == "coroSelect" {
				selects++
				if len(call.Args) != 7 {
					t.Errorf("coroSelect has %d arguments, want 7",
						len(call.Args))
				}
			}
		})
	}
	if selects != 1 {
		t.Fatalf("generated IR has %d coroutine selects, want 1", selects)
	}
	if nativeSelects != 0 || nativeChannels != 0 {
		t.Fatalf("generated IR retains %d selects and %d channel operations",
			nativeSelects, nativeChannels)
	}
}

func TestNewLowerSelectRejectsMalformedIR(t *testing.T) {
	prepareLowerTest(t)

	pkg := types.NewPkg("example.com/coro/selectmalformed",
		"selectmalformed")
	channel := ir.NewNameAt(src.NoXPos, pkg.Lookup("channel"),
		types.NewChan(types.Types[types.TINT], types.Cboth))
	newRecv := func() *ir.UnaryExpr {
		recv := ir.NewUnaryExpr(src.NoXPos, ir.ORECV, channel)
		recv.SetType(types.Types[types.TINT])
		return recv
	}
	newTarget := func(name string, typ *types.Type) *ir.Name {
		return ir.NewNameAt(src.NoXPos, pkg.Lookup(name), typ)
	}
	newSelect := func(comm ir.Node) *ir.SelectStmt {
		return ir.NewSelectStmt(src.NoXPos, []*ir.CommClause{
			ir.NewCommStmt(src.NoXPos, comm, nil),
		})
	}

	tests := []struct {
		name string
		make func() *ir.SelectStmt
		want string
	}{
		{
			name: "labeled",
			make: func() *ir.SelectStmt {
				stmt := newSelect(newRecv())
				stmt.Label = pkg.Lookup("label")
				return stmt
			},
			want: "labeled select",
		},
		{
			name: "walked",
			make: func() *ir.SelectStmt {
				stmt := newSelect(newRecv())
				stmt.SetWalked(true)
				return stmt
			},
			want: "already lowered select",
		},
		{
			name: "compiled",
			make: func() *ir.SelectStmt {
				stmt := newSelect(newRecv())
				stmt.Compiled = ir.Nodes{ir.NewBlockStmt(src.NoXPos, nil)}
				return stmt
			},
			want: "already lowered select",
		},
		{
			name: "too-many-cases",
			make: func() *ir.SelectStmt {
				return ir.NewSelectStmt(src.NoXPos,
					make([]*ir.CommClause, 1<<16+1))
			},
			want: "select has 65537 cases",
		},
		{
			name: "nil-case",
			make: func() *ir.SelectStmt {
				return ir.NewSelectStmt(src.NoXPos,
					[]*ir.CommClause{nil})
			},
			want: "nil select case",
		},
		{
			name: "multiple-defaults",
			make: func() *ir.SelectStmt {
				return ir.NewSelectStmt(src.NoXPos, []*ir.CommClause{
					ir.NewCommStmt(src.NoXPos, nil, nil),
					ir.NewCommStmt(src.NoXPos, nil, nil),
				})
			},
			want: "multiple select defaults",
		},
		{
			name: "invalid-unary",
			make: func() *ir.SelectStmt {
				operation := ir.NewUnaryExpr(src.NoXPos, ir.ONEG, channel)
				return newSelect(operation)
			},
			want: "unsupported select operation",
		},
		{
			name: "invalid-assignment",
			make: func() *ir.SelectStmt {
				assignment := ir.NewAssignStmt(src.NoXPos,
					newTarget("assignTarget", types.Types[types.TINT]),
					ir.NewInt(src.NoXPos, 1))
				return newSelect(assignment)
			},
			want: "unsupported select assignment",
		},
		{
			name: "complex-result",
			make: func() *ir.SelectStmt {
				values := newTarget("values",
					types.NewSlice(types.Types[types.TINT]))
				target := ir.NewIndexExpr(src.NoXPos, values,
					ir.NewInt(src.NoXPos, 0))
				target.SetType(types.Types[types.TINT])
				return newSelect(ir.NewAssignStmt(src.NoXPos,
					target, newRecv()))
			},
			want: "receive result 0 is not a variable",
		},
		{
			name: "untyped-result",
			make: func() *ir.SelectStmt {
				return newSelect(ir.NewAssignStmt(src.NoXPos,
					newTarget("untyped", nil), newRecv()))
			},
			want: "receive result 0 has no type",
		},
		{
			name: "invalid-receive-list",
			make: func() *ir.SelectStmt {
				assignment := ir.NewAssignListStmt(src.NoXPos, ir.OAS2RECV,
					ir.Nodes{newTarget("result",
						types.Types[types.TINT])}, ir.Nodes{newRecv()})
				return newSelect(assignment)
			},
			want: "unsupported select receive assignment",
		},
		{
			name: "receive-list-without-receive",
			make: func() *ir.SelectStmt {
				assignment := ir.NewAssignListStmt(src.NoXPos, ir.OAS2RECV,
					ir.Nodes{
						newTarget("value", types.Types[types.TINT]),
						newTarget("ok", types.Types[types.TBOOL]),
					}, ir.Nodes{ir.NewInt(src.NoXPos, 1)})
				return newSelect(assignment)
			},
			want: "select receive has no receive operation",
		},
		{
			name: "unsupported-operation",
			make: func() *ir.SelectStmt {
				return newSelect(ir.NewBlockStmt(src.NoXPos, nil))
			},
			want: "unsupported select operation",
		},
		{
			name: "untyped-channel",
			make: func() *ir.SelectStmt {
				untyped := ir.NewNameAt(src.NoXPos,
					pkg.Lookup("untypedChannel"), nil)
				recv := ir.NewUnaryExpr(src.NoXPos, ir.ORECV, untyped)
				recv.SetType(types.Types[types.TINT])
				return newSelect(recv)
			},
			want: "select operation has no channel type",
		},
	}

	if _, err := newLowerSelect(nil); err == nil ||
		!strings.Contains(err.Error(), "nil select") {
		t.Fatalf("newLowerSelect(nil) error = %v, want nil select", err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newLowerSelect(test.make())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("newLowerSelect error = %v, want %q", err, test.want)
			}
		})
	}

	target := newTarget("validTarget", types.Types[types.TINT])
	for name, stmt := range map[string]*ir.SelectStmt{
		"empty": ir.NewSelectStmt(src.NoXPos, nil),
		"default": ir.NewSelectStmt(src.NoXPos, []*ir.CommClause{
			ir.NewCommStmt(src.NoXPos, nil, nil),
		}),
		"bare-receive": newSelect(newRecv()),
		"assigned-receive": newSelect(ir.NewAssignStmt(
			src.NoXPos, target, newRecv())),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newLowerSelect(stmt); err != nil {
				t.Fatalf("newLowerSelect failed: %v", err)
			}
		})
	}
}

func TestTransformedRangeVariable(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/rangevar", "rangevar")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)
	fn := newLowerTestFunc(pkg, "rangeVariable")
	key := fn.NewLocal(src.NoXPos, pkg.Lookup("key"),
		types.Types[types.TINT])
	variable := fn.NewLocal(src.NoXPos, pkg.Lookup("variable"),
		types.Types[types.TINT])

	if got := transformedRangeVariable(nil); got != nil {
		t.Fatalf("transformedRangeVariable(nil) = %v, want nil", got)
	}
	loop := ir.NewRangeStmt(src.NoXPos, key, nil, nil, nil, false)
	if got := transformedRangeVariable(loop); got != nil {
		t.Fatalf("empty range variable = %v, want nil", got)
	}
	loop.Body = ir.Nodes{ir.NewInt(src.NoXPos, 1)}
	if got := transformedRangeVariable(loop); got != nil {
		t.Fatalf("non-assignment range variable = %v, want nil", got)
	}

	assignment := ir.NewAssignStmt(src.NoXPos, variable, key)
	loop.Body = ir.Nodes{assignment}
	if got := transformedRangeVariable(loop); got != nil {
		t.Fatalf("non-defining range variable = %v, want nil", got)
	}
	assignment.Def = true
	assignment.Y = ir.NewInt(src.NoXPos, 1)
	if got := transformedRangeVariable(loop); got != nil {
		t.Fatalf("unrelated range variable = %v, want nil", got)
	}
	assignment.Y = key
	assignment.X = ir.NewIndexExpr(src.NoXPos, variable,
		ir.NewInt(src.NoXPos, 0))
	if got := transformedRangeVariable(loop); got != nil {
		t.Fatalf("non-name range variable = %v, want nil", got)
	}
	assignment.X = variable
	if got := transformedRangeVariable(loop); got != nil {
		t.Fatalf("undeclared range variable = %v, want nil", got)
	}
	assignment.SetInit(ir.Nodes{ir.NewBlockStmt(src.NoXPos, nil)})
	if got := transformedRangeVariable(loop); got != nil {
		t.Fatalf("unmatched range declaration = %v, want nil", got)
	}
	assignment.SetInit(ir.Nodes{
		ir.NewDecl(src.NoXPos, ir.ODCL, variable),
	})
	if got := transformedRangeVariable(loop); got != variable {
		t.Fatalf("transformed range variable = %v, want %v", got, variable)
	}
}

func TestReparentClosureCaptures(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/closurecapture", "closurecapture")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)
	fn := newLowerTestFunc(pkg, "source")
	resume := newLowerTestFunc(pkg, "resume")
	source := fn.NewLocal(src.NoXPos, pkg.Lookup("sourceValue"),
		types.Types[types.TINT])
	resumeSource := resume.NewLocal(src.NoXPos, pkg.Lookup("resumeValue"),
		types.Types[types.TINT])

	closure := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
		types.NewSignature(nil, nil, nil), fn, typecheck.Target, 0)
	closure.DeclareParams(true)
	captured := ir.NewClosureVar(src.NoXPos, closure, source)
	nested := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
		types.NewSignature(nil, nil, nil), closure, typecheck.Target, 0)
	nested.DeclareParams(true)
	nestedCapture := ir.NewClosureVar(src.NoXPos, nested, captured)
	nestedTarget := closure.NewLocal(src.NoXPos, pkg.Lookup("nested"),
		nested.Type())
	closure.Body = ir.Nodes{
		ir.NewAssignStmt(src.NoXPos, nestedTarget, nested.OClosure),
	}

	excluded := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
		types.NewSignature(nil, nil, nil), fn, typecheck.Target, 0)
	excluded.DeclareParams(true)
	excludedCapture := ir.NewClosureVar(src.NoXPos, excluded, source)
	closureTarget := fn.NewLocal(src.NoXPos, pkg.Lookup("closure"),
		closure.Type())
	excludedTarget := fn.NewLocal(src.NoXPos, pkg.Lookup("excluded"),
		excluded.Type())
	fn.Body = ir.Nodes{
		ir.NewAssignStmt(src.NoXPos, closureTarget, closure.OClosure),
		ir.NewAssignStmt(src.NoXPos, excludedTarget, excluded.OClosure),
	}

	edit := func(node ir.Node) ir.Node {
		if node == source {
			return resumeSource
		}
		return node
	}
	if err := reparentClosureCaptures(fn, resume,
		map[*ir.Func]bool{excluded: true}, edit); err != nil {
		t.Fatal(err)
	}
	if closure.ClosureParent != resume {
		t.Fatalf("closure parent = %v, want resume", closure.ClosureParent)
	}
	if captured.Outer != resumeSource ||
		captured.Defn != resumeSource.Canonical() {
		t.Fatalf("capture = (outer %v, definition %v), want %v",
			captured.Outer, captured.Defn, resumeSource)
	}
	if nested.ClosureParent != closure ||
		nestedCapture.Defn != captured.Canonical() {
		t.Fatalf("nested capture = (parent %v, definition %v), want (%v, %v)",
			nested.ClosureParent, nestedCapture.Defn,
			closure, captured.Canonical())
	}
	if excluded.ClosureParent != fn ||
		excludedCapture.Outer != source {
		t.Fatalf("excluded capture = (parent %v, outer %v), want (%v, %v)",
			excluded.ClosureParent, excludedCapture.Outer, fn, source)
	}

	badFn := newLowerTestFunc(pkg, "badSource")
	badResume := newLowerTestFunc(pkg, "badResume")
	badSource := badFn.NewLocal(src.NoXPos, pkg.Lookup("badValue"),
		types.Types[types.TINT])
	badClosure := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
		types.NewSignature(nil, nil, nil), badFn, typecheck.Target, 0)
	badClosure.DeclareParams(true)
	ir.NewClosureVar(src.NoXPos, badClosure, badSource)
	badTarget := badFn.NewLocal(src.NoXPos, pkg.Lookup("badClosure"),
		badClosure.Type())
	laterClosure := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
		types.NewSignature(nil, nil, nil), badFn, typecheck.Target, 0)
	laterClosure.DeclareParams(true)
	laterTarget := badFn.NewLocal(src.NoXPos, pkg.Lookup("laterClosure"),
		laterClosure.Type())
	badFn.Body = ir.Nodes{
		ir.NewAssignStmt(src.NoXPos, badTarget, badClosure.OClosure),
		ir.NewAssignStmt(src.NoXPos, laterTarget, laterClosure.OClosure),
	}
	err := reparentClosureCaptures(badFn, badResume, nil,
		func(ir.Node) ir.Node {
			return ir.NewBasicLit(src.NoXPos, types.Types[types.TINT],
				constant.MakeInt64(0))
		})
	if err == nil || !strings.Contains(err.Error(),
		"badSource: closure capture is not a variable") {
		t.Fatalf("reparentClosureCaptures error = %v", err)
	}
}

func TestLowerRejectsRangeVariableClosure(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/rangeclosure", "rangeclosure")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)
	fn := newLowerTestFunc(pkg, "rangeClosure")
	channelType := types.NewChan(types.Types[types.TINT], types.Cboth)
	channel := fn.NewLocal(src.NoXPos, pkg.Lookup("channel"), channelType)
	rangeKey := fn.NewLocal(src.NoXPos, pkg.Lookup("rangeKey"),
		types.Types[types.TINT])
	variable := fn.NewLocal(src.NoXPos, pkg.Lookup("variable"),
		types.Types[types.TINT])
	copyKey := ir.NewAssignStmt(src.NoXPos, variable, rangeKey)
	copyKey.Def = true
	copyKey.SetInit(ir.Nodes{
		ir.NewDecl(src.NoXPos, ir.ODCL, variable),
	})

	closure := ir.NewClosureFunc(src.NoXPos, src.NoXPos, ir.OCLOSURE,
		types.NewSignature(nil, nil, nil), fn, typecheck.Target, 0)
	closure.DeclareParams(true)
	captured := ir.NewClosureVar(src.NoXPos, closure, variable)
	sink := ir.NewNameAt(src.NoXPos, pkg.Lookup("sink"),
		types.Types[types.TINT])
	sink.Class = ir.PEXTERN
	closure.Body = ir.Nodes{
		ir.NewAssignStmt(src.NoXPos, sink, captured),
	}
	closureTarget := fn.NewLocal(src.NoXPos, pkg.Lookup("closure"),
		closure.Type())
	saveClosure := ir.NewAssignStmt(src.NoXPos, closureTarget,
		closure.OClosure)
	loop := ir.NewRangeStmt(src.NoXPos, rangeKey, nil, channel,
		ir.Nodes{copyKey, saveClosure}, false)
	fn.Body = ir.Nodes{loop}
	function := &Function{
		Func: fn,
		Sites: []Site{{
			ID: 1, Kind: SiteChannel, Node: loop,
		}},
	}
	_, err := newLowerCandidate(
		&Plan{Functions: map[*ir.Func]*Function{fn: function}},
		function)
	if err == nil || !strings.Contains(err.Error(),
		"range variable captured by closure") {
		t.Fatalf("newLowerCandidate error = %v, want range closure error", err)
	}
}

func TestLowerRejectsUnsupportedChannels(t *testing.T) {
	prepareLowerTest(t)

	oldTarget := typecheck.Target
	oldLocalPkg := types.LocalPkg
	defer func() {
		typecheck.Target = oldTarget
		types.LocalPkg = oldLocalPkg
	}()

	pkg := types.NewPkg("example.com/coro/channelreject", "channelreject")
	types.LocalPkg = pkg
	typecheck.Target = new(ir.Package)
	channelType := types.NewChan(types.Types[types.TINT], types.Cboth)

	tests := []struct {
		name string
		make func(*ir.Func, *ir.Name) (ir.Nodes, ir.Node)
		want string
	}{
		{
			name: "nested-expression",
			make: func(fn *ir.Func, channel *ir.Name) (ir.Nodes, ir.Node) {
				recv := ir.NewUnaryExpr(src.NoXPos, ir.ORECV, channel)
				recv.SetType(types.Types[types.TINT])
				target := fn.NewLocal(src.NoXPos, pkg.Lookup("nestedTarget"),
					types.Types[types.TINT])
				expression := ir.NewBinaryExpr(src.NoXPos, ir.OADD, recv,
					ir.NewInt(src.NoXPos, 1))
				expression.SetType(types.Types[types.TINT])
				assign := ir.NewAssignStmt(src.NoXPos, target, expression)
				return ir.Nodes{assign}, recv
			},
			want: "nested receive assignment",
		},
		{
			name: "complex-target",
			make: func(fn *ir.Func, channel *ir.Name) (ir.Nodes, ir.Node) {
				recv := ir.NewUnaryExpr(src.NoXPos, ir.ORECV, channel)
				recv.SetType(types.Types[types.TINT])
				values := fn.NewLocal(src.NoXPos, pkg.Lookup("values"),
					types.NewSlice(types.Types[types.TINT]))
				target := ir.NewIndexExpr(src.NoXPos, values,
					ir.NewInt(src.NoXPos, 0))
				target.SetType(types.Types[types.TINT])
				assign := ir.NewAssignStmt(src.NoXPos, target, recv)
				return ir.Nodes{assign}, recv
			},
			want: "receive result 0 is not a variable",
		},
		{
			name: "range-value-variable",
			make: func(fn *ir.Func, channel *ir.Name) (ir.Nodes, ir.Node) {
				value := fn.NewLocal(src.NoXPos,
					pkg.Lookup("rangeValue"), types.Types[types.TINT])
				loop := ir.NewRangeStmt(src.NoXPos, nil, value, channel,
					nil, false)
				return ir.Nodes{loop}, loop
			},
			want: "channel range has value variable",
		},
		{
			name: "labeled-range",
			make: func(_ *ir.Func, channel *ir.Name) (ir.Nodes, ir.Node) {
				loop := ir.NewRangeStmt(src.NoXPos, nil, nil, channel,
					nil, false)
				loop.Label = pkg.Lookup("rangeLabel")
				return ir.Nodes{loop}, loop
			},
			want: "labeled range loop",
		},
		{
			name: "non-channel-range",
			make: func(_ *ir.Func, _ *ir.Name) (ir.Nodes, ir.Node) {
				loop := ir.NewRangeStmt(src.NoXPos, nil, nil,
					ir.NewInt(src.NoXPos, 1), nil, false)
				return ir.Nodes{loop}, loop
			},
			want: "unsupported range loop",
		},
		{
			name: "unprocessed-range-variable",
			make: func(_ *ir.Func, channel *ir.Name) (ir.Nodes, ir.Node) {
				loop := ir.NewRangeStmt(src.NoXPos, nil, nil, channel,
					nil, true)
				return ir.Nodes{loop}, loop
			},
			want: "unprocessed range variables",
		},
		{
			name: "complex-range-result",
			make: func(fn *ir.Func, channel *ir.Name) (ir.Nodes, ir.Node) {
				values := fn.NewLocal(src.NoXPos,
					pkg.Lookup("rangeValues"),
					types.NewSlice(types.Types[types.TINT]))
				key := ir.NewIndexExpr(src.NoXPos, values,
					ir.NewInt(src.NoXPos, 0))
				key.SetType(types.Types[types.TINT])
				loop := ir.NewRangeStmt(src.NoXPos, key, nil, channel,
					nil, false)
				return ir.Nodes{loop}, loop
			},
			want: "range result is not a variable",
		},
		{
			name: "untyped-range-result",
			make: func(fn *ir.Func, channel *ir.Name) (ir.Nodes, ir.Node) {
				key := fn.NewLocal(src.NoXPos,
					pkg.Lookup("untypedRangeResult"), nil)
				loop := ir.NewRangeStmt(src.NoXPos, key, nil, channel,
					nil, false)
				return ir.Nodes{loop}, loop
			},
			want: "range result has no type",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fn := newLowerTestFunc(pkg, test.name)
			channel := fn.NewLocal(src.NoXPos,
				pkg.Lookup(test.name+"Channel"), channelType)
			body, site := test.make(fn, channel)
			fn.Body = body
			function := &Function{
				Func: fn,
				Sites: []Site{{
					ID: 1, Kind: SiteChannel, Node: site,
				}},
			}
			_, err := newLowerCandidate(
				&Plan{Functions: map[*ir.Func]*Function{fn: function}},
				function)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("newLowerCandidate error = %v, want %q",
					err, test.want)
			}
		})
	}
}

func TestNewLowerChannelRejectsMalformedIR(t *testing.T) {
	prepareLowerTest(t)

	pkg := types.NewPkg("example.com/coro/channelmalformed",
		"channelmalformed")
	channel := ir.NewNameAt(src.NoXPos, pkg.Lookup("channel"),
		types.NewChan(types.Types[types.TINT], types.Cboth))
	newRecv := func() *ir.UnaryExpr {
		recv := ir.NewUnaryExpr(src.NoXPos, ir.ORECV, channel)
		recv.SetType(types.Types[types.TINT])
		return recv
	}
	newTarget := func(name string, typ *types.Type) *ir.Name {
		return ir.NewNameAt(src.NoXPos, pkg.Lookup(name), typ)
	}

	tests := []struct {
		name string
		make func() (ir.Node, ir.Node, ir.Node)
		want string
	}{
		{
			name: "nested-send",
			make: func() (ir.Node, ir.Node, ir.Node) {
				send := ir.NewSendStmt(src.NoXPos, channel,
					ir.NewInt(src.NoXPos, 1))
				return send, send, ir.NewBlockStmt(src.NoXPos, nil)
			},
			want: "nested send",
		},
		{
			name: "invalid-receive",
			make: func() (ir.Node, ir.Node, ir.Node) {
				node := ir.NewUnaryExpr(src.NoXPos, ir.ONEG, channel)
				return node, node, node
			},
			want: "invalid receive operation",
		},
		{
			name: "nested-receive",
			make: func() (ir.Node, ir.Node, ir.Node) {
				recv := newRecv()
				owner := newRecv()
				return recv, owner, owner
			},
			want: "nested receive",
		},
		{
			name: "bad-receive-list",
			make: func() (ir.Node, ir.Node, ir.Node) {
				recv := newRecv()
				owner := ir.NewAssignListStmt(src.NoXPos, ir.OAS2RECV,
					ir.Nodes{newTarget("listTarget",
						types.Types[types.TINT])}, ir.Nodes{recv})
				return recv, owner, owner
			},
			want: "unsupported receive assignment",
		},
		{
			name: "bad-owner",
			make: func() (ir.Node, ir.Node, ir.Node) {
				recv := newRecv()
				owner := ir.NewBlockStmt(src.NoXPos, nil)
				return recv, owner, owner
			},
			want: "nested receive in BLOCK",
		},
		{
			name: "untyped-result",
			make: func() (ir.Node, ir.Node, ir.Node) {
				recv := newRecv()
				owner := ir.NewAssignStmt(src.NoXPos,
					newTarget("untypedTarget", nil), recv)
				return recv, owner, owner
			},
			want: "receive result 0 has no type",
		},
		{
			name: "bad-normalization",
			make: func() (ir.Node, ir.Node, ir.Node) {
				recv := newRecv()
				temp := newTarget("normalTemp", types.Types[types.TINT])
				owner := ir.NewAssignStmt(src.NoXPos, temp, recv)
				other := newTarget("normalOther", types.Types[types.TINT])
				projection := ir.NewConvExpr(src.NoXPos, ir.OCONVNOP,
					types.Types[types.TINT], other)
				projection.SetInit(ir.Nodes{owner})
				container := ir.NewAssignStmt(src.NoXPos,
					newTarget("normalTarget", types.Types[types.TINT]),
					projection)
				return recv, owner, container
			},
			want: "nested receive assignment",
		},
		{
			name: "bad-channel",
			make: func() (ir.Node, ir.Node, ir.Node) {
				notChannel := newTarget("notChannel",
					types.Types[types.TINT])
				recv := ir.NewUnaryExpr(src.NoXPos, ir.ORECV, notChannel)
				recv.SetType(types.Types[types.TINT])
				return recv, recv, recv
			},
			want: "operation has no channel type",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			node, statement, container := test.make()
			_, err := newLowerChannel(node, statement, container)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("newLowerChannel error = %v, want %q",
					err, test.want)
			}
		})
	}
}
