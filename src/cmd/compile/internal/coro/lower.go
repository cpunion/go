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
	"slices"
	"strings"
)

const (
	coroActComplete = 0
	coroActYield    = 1
	coroActWait     = 2
	coroActPanic    = 3
	coroActGoexit   = 4
)

func (p *Plan) lowerAll() (int, int, []string) {
	// Deterministic ordering for reproducible symbols and diagnostics.
	var lowered []*FuncInfo
	var skippedDiags []string
	skippedCount := 0
	sorted := slices.Clone(p.Funcs)
	slices.SortFunc(sorted, func(a, b *ir.Func) int {
		return strings.Compare(ir.PkgFuncName(a), ir.PkgFuncName(b))
	})
	for _, fn := range sorted {
		if info := p.Lowered[fn]; info != nil {
			lowered = append(lowered, info)
		} else if reason := p.SkipReason[fn]; reason != "" && reason != "empty body" && reason != "wrapper" && reason != "package init" && reason != "cgo/init helper" {
			skippedCount++
			skippedDiags = append(skippedDiags, fmt.Sprintf("%s: %s", ir.PkgFuncName(fn), reason))
		}
	}
	if len(lowered) == 0 {
		return 0, skippedCount, skippedDiags
	}

	// Stage 2 & 3: build CFG + frame struct for every lowered function.
	cfgs := make(map[*FuncInfo]*funcCFG, len(lowered))
	for _, info := range lowered {
		g := buildCFG(p, info)
		buildFrame(info, g)
		cfgs[info] = g
	}

	// Stage 4a: create F.coro and F.coro.func1 shells so mutual/self recursion resolves cleanly.
	for _, info := range lowered {
		p.createFuncShells(info)
	}

	// Stage 4b: emit F.coro.func1, F.coro, public wrapper F, and dynamic dispatch adapters.
	deadClo := make(map[*ir.Func]bool)
	for _, info := range lowered {
		g := cfgs[info]
		p.emitResumeBody(info, g, deadClo)
		p.emitFactoryBody(info)
		p.rewritePublicWrapper(info)
		p.emitCallableAdapter(info)
	}

	// Stage 4c: emit synthetic package init registering dynamic callables.
	p.emitCallableInit(lowered)

	// Prune dead .gowrap/.deferwrap wrappers and cloned plain defer closures whose
	// parent function was lowered, so escape analysis never walks stale closure vars.
	kept := p.Pkg.Funcs[:0]
	for _, fn := range p.Pkg.Funcs {
		if fn != nil && (deadClo[fn] || (isGoDeferWrapper(fn) && p.Lowered[fn.ClosureParent] != nil)) {
			continue
		}
		kept = append(kept, fn)
	}
	p.Pkg.Funcs = kept

	return len(lowered), skippedCount, skippedDiags
}

func (p *Plan) createFuncShells(info *FuncInfo) {
	fn := info.Orig
	pos := fn.Pos()
	baseName := fn.Sym().Name

	// 1. State machine: func F.coro.func1(ctx unsafe.Pointer) uint8
	resumeSym := types.LocalPkg.Lookup(baseName + ".coro.func1")
	ctxField := types.NewField(pos, types.LocalPkg.Lookup("ctx"), types.Types[types.TUNSAFEPTR])
	retField := types.NewField(pos, nil, types.Types[types.TUINT8])
	resumeSig := types.NewSignature(nil, []*types.Field{ctxField}, []*types.Field{retField})
	resumeFn := ir.NewFunc(pos, pos, resumeSym, resumeSig)
	typecheck.DeclFunc(resumeFn)
	typecheck.FinishFuncBody()
	info.ResumeFn = resumeFn

	// 2. Frame factory: func F.coro(caps..., recv?, params...) (unsafe.Pointer, func(unsafe.Pointer) uint8)
	facSym := types.LocalPkg.Lookup(baseName + ".coro")
	var facParams []*types.Field
	for i, cf := range info.CapFields {
		facParams = append(facParams, types.NewField(pos, types.LocalPkg.Lookup(fmt.Sprintf("c%d", i)), cf.Type))
	}
	for i, pf := range info.ParamFields {
		facParams = append(facParams, types.NewField(pos, types.LocalPkg.Lookup(fmt.Sprintf("p%d", i)), pf.Type))
	}
	resumeFuncType := types.NewSignature(nil,
		[]*types.Field{types.NewField(pos, nil, types.Types[types.TUNSAFEPTR])},
		[]*types.Field{types.NewField(pos, nil, types.Types[types.TUINT8])},
	)
	facResults := []*types.Field{
		types.NewField(pos, nil, types.Types[types.TUNSAFEPTR]),
		types.NewField(pos, nil, resumeFuncType),
	}
	facSig := types.NewSignature(nil, facParams, facResults)
	facFn := ir.NewFunc(pos, pos, facSym, facSig)
	typecheck.DeclFunc(facFn)
	typecheck.FinishFuncBody()
	info.FactoryFn = facFn
}

func (p *Plan) emitFactoryBody(info *FuncInfo) {
	fac := info.FactoryFn
	pos := info.Orig.Pos()
	ir.WithFunc(fac, func() {
		// frame := runtime.coroAlloc(typePtr(FrameType))
		allocCall := typecheck.Call(pos, typecheck.LookupRuntime("coroAlloc"), []ir.Node{
			reflectdata.TypePtrAt(pos, info.FrameType),
		}, false)
		frameVar := newLocal(fac, pos, ".frame", types.Types[types.TUNSAFEPTR])
		fac.Body.Append(assignNode(pos, frameVar, allocCall))

		// f := (*FrameType)(frame)
		fptrType := types.NewPtr(info.FrameType)
		fVar := newLocal(fac, pos, ".f", fptrType)
		fac.Body.Append(assignNode(pos, fVar, typecheck.ConvNop(frameVar, fptrType)))

		// Populate captured vars and parameters.
		params := fac.Type().Params()
		pi := 0
		for _, cf := range info.CapFields {
			arg := params[pi].Nname.(*ir.Name)
			fac.Body.Append(assignNode(pos, fieldSel(pos, fVar, cf), arg))
			pi++
		}
		for _, pf := range info.ParamFields {
			arg := params[pi].Nname.(*ir.Name)
			fac.Body.Append(assignNode(pos, fieldSel(pos, fVar, pf), arg))
			pi++
		}

		// return frame, F.coro.func1
		ret := ir.NewReturnStmt(pos, []ir.Node{frameVar, info.ResumeFn.Nname})
		fac.Body.Append(typecheck.Stmt(ret))
		typecheck.Stmts(fac.Body)
	})
}

func (p *Plan) rewritePublicWrapper(info *FuncInfo) {
	fn := info.Orig
	pos := fn.Pos()
	fn.DiscardInlineBody()

	var body ir.Nodes
	ir.WithFunc(fn, func() {
		// Gather captures (if closure), receiver (if method), and regular params.
		var args []ir.Node
		for i, cv := range fn.ClosureVars {
			var a ir.Node = cv
			if i < len(info.CapByRef) && info.CapByRef[i] {
				a = typecheck.NodAddrAt(pos, cv)
			}
			args = append(args, a)
		}
		sig := fn.Type()
		if recv := sig.Recv(); recv != nil {
			args = append(args, ensureParamName(fn, pos, recv, ".recv"))
		}
		for i, param := range sig.Params() {
			args = append(args, ensureParamName(fn, pos, param, fmt.Sprintf(".p%d", i)))
		}

		// frame, resume := F.coro(args...)
		frameVar := newLocal(fn, pos, ".frame", types.Types[types.TUNSAFEPTR])
		resumeVar := newLocal(fn, pos, ".resume", info.FactoryFn.Type().Results()[1].Type)
		facCall := typecheck.Call(pos, info.FactoryFn.Nname, args, false)
		al := ir.NewAssignListStmt(pos, ir.OAS2, []ir.Node{frameVar, resumeVar}, []ir.Node{facCall})
		body.Append(typecheck.Stmt(al))

		// runtime.coroRunFrame(frame, resume)
		runCall := typecheck.Call(pos, typecheck.LookupRuntime("coroRunFrame"), []ir.Node{frameVar, resumeVar}, false)
		body.Append(typecheck.Stmt(runCall))

		// Copy return values out before coroReleaseFrame(frame).
		var retVals []ir.Node
		if len(info.ResFields) > 0 {
			fptrType := types.NewPtr(info.FrameType)
			fVar := newLocal(fn, pos, ".f", fptrType)
			body.Append(assignNode(pos, fVar, typecheck.ConvNop(frameVar, fptrType)))
			for i, rf := range info.ResFields {
				rv := newLocal(fn, pos, fmt.Sprintf(".r%d", i), rf.Type)
				body.Append(assignNode(pos, rv, fieldSel(pos, fVar, rf)))
				retVals = append(retVals, rv)
			}
		}

		// runtime.coroReleaseFrame(frame)
		relCall := typecheck.Call(pos, typecheck.LookupRuntime("coroReleaseFrame"), []ir.Node{frameVar}, false)
		body.Append(typecheck.Stmt(relCall))

		// return r0, r1, ...
		ret := ir.NewReturnStmt(pos, retVals)
		body.Append(typecheck.Stmt(ret))

		// Reset fn.Dcl to params/results + wrapper locals only (original locals now live in ResumeFn/Frame).
		var newDcl []*ir.Name
		for _, d := range fn.Dcl {
			if d.Class == ir.PPARAM || d.Class == ir.PPARAMOUT {
				newDcl = append(newDcl, d)
			}
		}
		for _, d := range fn.Dcl {
			if d.Class == ir.PAUTO && (strings.HasPrefix(d.Sym().Name, ".frame") ||
				strings.HasPrefix(d.Sym().Name, ".resume") ||
				strings.HasPrefix(d.Sym().Name, ".f") ||
				strings.HasPrefix(d.Sym().Name, ".r")) {
				newDcl = append(newDcl, d)
			}
		}
		fn.Dcl = newDcl
		fn.Body = body
		fn.SetHasDefer(false)
		typecheck.Stmts(fn.Body)
	})
}

func (p *Plan) emitResumeBody(info *FuncInfo, g *funcCFG, deadClo map[*ir.Func]bool) {
	rf := info.ResumeFn
	pos := info.Orig.Pos()
	ir.WithFunc(rf, func() {
		ctxVar := rf.Type().Params()[0].Nname.(*ir.Name)
		fptrType := types.NewPtr(info.FrameType)
		fVar := newLocal(rf, pos, ".f", fptrType)
		rf.Body.Append(typecheck.Stmt(ir.NewDecl(pos, ir.ODCL, fVar)))
		rf.Body.Append(assignNode(pos, fVar, typecheck.ConvNop(ctxVar, fptrType)))

		rw := &bodyRewriter{
			plan:      p,
			info:      info,
			g:         g,
			rf:        rf,
			ctxVar:    ctxVar,
			fVar:      fVar,
			stackLocs: make(map[*ir.Name]*ir.Name),
			byRefCaps: make(map[*ir.Name]*types.Field),
			deadClo:   deadClo,
		}
		for i, cv := range info.Orig.ClosureVars {
			if i < len(info.CapByRef) && info.CapByRef[i] {
				rw.byRefCaps[cv.Canonical()] = info.CapFields[i]
			}
		}

		// Create block labels.
		bbLabels := make([]*types.Sym, len(g.blocks))
		for i := range g.blocks {
			bbLabels[i] = types.LocalPkg.Lookup(fmt.Sprintf(".coro_bb%d", i))
		}

		// Dispatch switch on f._state.
		var cases []*ir.CaseClause
		for i, bb := range g.blocks {
			if i == 0 || bb.resumeState > 0 {
				stVal := uint64(bb.resumeState)
				br := ir.NewBranchStmt(pos, ir.OGOTO, bbLabels[i])
				cc := ir.NewCaseStmt(pos, []ir.Node{typecheck.Conv(ir.NewInt(pos, int64(stVal)), types.Types[types.TUINT32])}, []ir.Node{br})
				cases = append(cases, cc)
			}
		}
		cases = append(cases, ir.NewCaseStmt(pos, nil, []ir.Node{
			ir.NewReturnStmt(pos, []ir.Node{typecheck.Conv(ir.NewInt(pos, coroActComplete), types.Types[types.TUINT8])}),
		}))
		sw := ir.NewSwitchStmt(pos, fieldSel(pos, fVar, info.StateField), cases)
		rf.Body.Append(typecheck.Stmt(sw))

		// Emit each basic block.
		for i, bb := range g.blocks {
			rf.Body.Append(typecheck.Stmt(ir.NewLabelStmt(pos, bbLabels[i])))

			// Emit resume epilogue for suspension points that land on this block.
			if bb.resumeState > 0 {
				rw.emitResumeEpilogue(bb, g, bbLabels)
			}

			// Emit straight-line statements.
			for _, stmt := range bb.stmts {
				rw.emitStmt(stmt)
			}

			// Emit block terminator.
			rw.emitTerm(&bb.term, bbLabels)
		}

		// Trailing return complete (unreachable safety).
		rf.Body.Append(typecheck.Stmt(ir.NewReturnStmt(pos, []ir.Node{typecheck.Conv(ir.NewInt(pos, coroActComplete), types.Types[types.TUINT8])})))

		typecheck.Stmts(rf.Body)
	})
}

type bodyRewriter struct {
	plan      *Plan
	info      *FuncInfo
	g         *funcCFG
	rf        *ir.Func
	ctxVar    *ir.Name
	fVar      *ir.Name
	stackLocs map[*ir.Name]*ir.Name
	byRefCaps map[*ir.Name]*types.Field
	deadClo   map[*ir.Func]bool
	tmpSeq    int
}

func (rw *bodyRewriter) emitResumeEpilogue(bb *basicBlock, g *funcCFG, bbLabels []*types.Sym) {
	// Find the predecessor block whose suspension terminator targets bb.
	for _, pred := range g.blocks {
		t := &pred.term
		if t.target != bb {
			continue
		}
		pos := t.pos
		switch t.kind {
		case termChanRecv:
			if t.dst != nil && t.elemTmp != nil {
				rw.rf.Body.Append(assignNode(pos, rw.rewriteNode(t.dst), rw.rewriteNode(t.elemTmp)))
			}
			if t.okDst != nil && g.chanOKField != nil {
				rw.rf.Body.Append(assignNode(pos, rw.rewriteNode(t.okDst), fieldSel(pos, rw.fVar, g.chanOKField)))
			}
		case termIO:
			// Call coroReadFinish / coroWriteFinish, then assign n and err.
			recv := rw.rewriteNode(t.ioRecv)
			finMethod := "coroReadFinish"
			if t.ioKind == ioFileWrite || t.ioKind == ioConnWrite {
				finMethod = "coroWriteFinish"
			}
			finCall := callStdlibIOMethod(pos, recv, t.ioKind, finMethod, []ir.Node{
				typecheck.NodAddrAt(pos, fieldSel(pos, rw.fVar, g.ioNField)),
				typecheck.NodAddrAt(pos, fieldSel(pos, rw.fVar, g.ioErrField)),
				fieldSel(pos, rw.fVar, g.ioDirectField),
				fieldSel(pos, rw.fVar, g.ioStatusField),
			})
			rw.rf.Body.Append(typecheck.Stmt(finCall))
			if t.ioNDst != nil {
				rw.rf.Body.Append(assignNode(pos, rw.rewriteNode(t.ioNDst), fieldSel(pos, rw.fVar, g.ioNField)))
			}
			if t.ioErrDst != nil {
				rw.rf.Body.Append(assignNode(pos, rw.rewriteNode(t.ioErrDst), fieldSel(pos, rw.fVar, g.ioErrField)))
			}
		case termAwait:
			// Copy callee return values from childFrame, then release childFrame.
			calleeInfo := rw.plan.Lowered[t.awaitCallee]
			childPtr := fieldSel(pos, rw.fVar, g.awaitChildField)
			if len(calleeInfo.ResFields) > 0 && len(t.awaitDsts) > 0 {
				cfType := types.NewPtr(calleeInfo.FrameType)
				cfVar := newLocal(rw.rf, pos, fmt.Sprintf(".cf%d", rw.tmpSeq), cfType)
				rw.tmpSeq++
				rw.rf.Body.Append(assignNode(pos, cfVar, typecheck.ConvNop(childPtr, cfType)))
				for idx, dst := range t.awaitDsts {
					if dst != nil && !ir.IsBlank(dst) && idx < len(calleeInfo.ResFields) {
						rw.rf.Body.Append(assignNode(pos, rw.rewriteNode(dst), fieldSel(pos, cfVar, calleeInfo.ResFields[idx])))
					}
				}
			}
			relCall := typecheck.Call(pos, typecheck.LookupRuntime("coroReleaseFrame"), []ir.Node{childPtr}, false)
			rw.rf.Body.Append(typecheck.Stmt(relCall))
			rw.rf.Body.Append(assignNode(pos, fieldSel(pos, rw.fVar, g.awaitChildField), ir.NewNilExpr(pos, types.Types[types.TUNSAFEPTR])))
		case termSelect:
			// Switch on f._selI to copy recv/ok and jump to chosen case body.
			var cases []*ir.CaseClause
			for idx, c := range t.selCases {
				var cBody ir.Nodes
				if !c.isSend {
					if c.dst != nil && c.elemTmp != nil {
						cBody.Append(assignNode(pos, rw.rewriteNode(c.dst), rw.rewriteNode(c.elemTmp)))
					}
					if c.okDst != nil {
						cBody.Append(assignNode(pos, rw.rewriteNode(c.okDst), fieldSel(pos, rw.fVar, g.selOKField)))
					}
				}
				cBody.Append(ir.NewBranchStmt(pos, ir.OGOTO, bbLabels[c.bodyBB.id]))
				cases = append(cases, ir.NewCaseStmt(pos, []ir.Node{ir.NewInt(pos, int64(idx))}, cBody))
			}
			if t.hasDefault && t.defaultBB != nil {
				cases = append(cases, ir.NewCaseStmt(pos, nil, []ir.Node{
					ir.NewBranchStmt(pos, ir.OGOTO, bbLabels[t.defaultBB.id]),
				}))
			} else {
				cases = append(cases, ir.NewCaseStmt(pos, nil, []ir.Node{
					rw.returnAction(pos, coroActComplete),
				}))
			}
			sw := ir.NewSwitchStmt(pos, fieldSel(pos, rw.fVar, g.selChosenField), cases)
			rw.rf.Body.Append(typecheck.Stmt(sw))
		}
		return
	}
}

func (rw *bodyRewriter) emitStmt(stmt ir.Node) {
	if stmt == nil {
		return
	}
	switch stmt.Op() {
	case ir.ODCL:
		dcl := stmt.(*ir.Decl)
		if rw.info.VarFields[dcl.X.Canonical()] != nil {
			return // frame-resident; no stack ODCL needed
		}
		dcl.X = rw.mapStackLocal(dcl.X.Canonical())
		rw.rf.Body.Append(dcl)
		return
	case ir.OGO:
		rw.emitGoStmt(stmt.(*ir.GoDeferStmt))
		return
	case ir.ODEFER:
		rw.emitDeferStmt(stmt.(*ir.GoDeferStmt))
		return
	}
	rw.rf.Body.Append(typecheck.Stmt(rw.rewriteNode(stmt)))
}

func isGoDeferWrapper(fn *ir.Func) bool {
	if fn == nil || !fn.Wrapper() || fn.Sym() == nil {
		return false
	}
	name := fn.Sym().Name
	return strings.Contains(name, ".gowrap") || strings.Contains(name, ".deferwrap")
}

// unwrapGoDeferCall peels off the synthetic zero-arg .gowrap/.deferwrap closure
// produced by typecheck.tcGoDefer, restoring the original call with outer vars
// already evaluated in the statement's Init list.
func unwrapGoDeferCall(call *ir.CallExpr) *ir.CallExpr {
	if call == nil || call.Op() != ir.OCALLFUNC {
		return call
	}
	cloExpr, ok := call.Fun.(*ir.ClosureExpr)
	if !ok || !isGoDeferWrapper(cloExpr.Func) {
		return call
	}
	wfn := cloExpr.Func
	if len(wfn.Body) != 1 {
		return call
	}
	inner, ok := wfn.Body[0].(*ir.CallExpr)
	if !ok {
		return call
	}
	capMap := make(map[*ir.Name]ir.Node, len(wfn.ClosureVars))
	for _, cv := range wfn.ClosureVars {
		if cv.Outer != nil {
			capMap[cv.Canonical()] = cv.Outer
		}
	}
	clone := ir.Copy(inner).(*ir.CallExpr)
	var subst func(ir.Node) ir.Node
	subst = func(n ir.Node) ir.Node {
		if n == nil {
			return nil
		}
		if nm, ok := n.(*ir.Name); ok {
			if outer, found := capMap[nm.Canonical()]; found {
				return outer
			}
			return nm
		}
		cp := ir.Copy(n)
		ir.EditChildren(cp, subst)
		return cp
	}
	ir.EditChildren(clone, subst)
	return clone
}

func (rw *bodyRewriter) emitGoStmt(goSt *ir.GoDeferStmt) {
	pos := goSt.Pos()
	call, ok := goSt.Call.(*ir.CallExpr)
	if !ok {
		// Unlikely non-CallExpr; fall back to ordinary go.
		rw.rf.Body.Append(typecheck.Stmt(rw.rewriteNode(goSt)))
		return
	}
	call = unwrapGoDeferCall(call)

	// 1. Direct static lowered callee (function, method, or direct closure literal).
	if callee := rw.g.isLoweredCallee(call); callee != nil {
		calleeInfo := rw.plan.Lowered[callee]
		args := rw.rewriteCalleeArgs(call, callee)
		frameVar := newLocal(rw.rf, pos, fmt.Sprintf(".sf%d", rw.tmpSeq), types.Types[types.TUNSAFEPTR])
		resVar := newLocal(rw.rf, pos, fmt.Sprintf(".sr%d", rw.tmpSeq), calleeInfo.FactoryFn.Type().Results()[1].Type)
		rw.tmpSeq++
		facCall := typecheck.Call(pos, calleeInfo.FactoryFn.Nname, args, false)
		al := ir.NewAssignListStmt(pos, ir.OAS2, []ir.Node{frameVar, resVar}, []ir.Node{facCall})
		rw.rf.Body.Append(typecheck.Stmt(al))
		spawnCall := typecheck.Call(pos, typecheck.LookupRuntime("coroSpawnFrame"), []ir.Node{rw.ctxVar, frameVar, resVar}, false)
		rw.rf.Body.Append(typecheck.Stmt(spawnCall))
		return
	}

	// 2. Dynamic interface method call: `go iface.M(args...)`.
	if call.Op() == ir.OCALLINTER {
		rw.emitDynamicInterfaceGo(pos, call)
		return
	}

	// 3. Dynamic function value call: `go fnVar(args...)`.
	if call.Op() == ir.OCALLFUNC {
		if _, isStaticName := call.Fun.(*ir.Name); !isStaticName {
			if _, isCloLit := call.Fun.(*ir.ClosureExpr); !isCloLit {
				rw.emitDynamicFuncGo(pos, call)
				return
			}
		}
	}

	// 4. Unlowered static callee (e.g. skipped cgo worker or plain helper): spawn via coroSpawnOpaque.
	rw.emitOpaqueGo(pos, call)
}

func (rw *bodyRewriter) emitOpaqueGo(pos src.XPos, call *ir.CallExpr) {
	// Evaluate receiver / function / arguments now (Go spec), capture in a zero-arg closure, call coroSpawnOpaque.
	var callExpr *ir.CallExpr
	rwCall := rw.rewriteNode(call).(*ir.CallExpr)
	var evalArgs []ir.Node
	for i, a := range rwCall.Args {
		tmp := newLocal(rw.rf, pos, fmt.Sprintf(".ga%d_%d", rw.tmpSeq, i), a.Type())
		rw.rf.Body.Append(typecheck.Stmt(ir.NewDecl(pos, ir.ODCL, tmp)))
		rw.rf.Body.Append(assignNode(pos, tmp, a))
		evalArgs = append(evalArgs, tmp)
	}
	var fnNode ir.Node = rwCall.Fun
	if sel, ok := rwCall.Fun.(*ir.SelectorExpr); ok && (sel.Op() == ir.ODOTMETH || sel.Op() == ir.ODOTINTER) {
		rtmp := newLocal(rw.rf, pos, fmt.Sprintf(".gr%d", rw.tmpSeq), sel.X.Type())
		rw.rf.Body.Append(typecheck.Stmt(ir.NewDecl(pos, ir.ODCL, rtmp)))
		rw.rf.Body.Append(assignNode(pos, rtmp, sel.X))
		selCopy := ir.NewSelectorExpr(pos, sel.Op(), rtmp, sel.Sel)
		selCopy.Selection = sel.Selection
		selCopy.SetType(sel.Type())
		selCopy.SetTypecheck(1)
		fnNode = selCopy
	} else if _, isStatic := rwCall.Fun.(*ir.Name); !isStatic {
		ftmp := newLocal(rw.rf, pos, fmt.Sprintf(".gf%d", rw.tmpSeq), rwCall.Fun.Type())
		rw.rf.Body.Append(typecheck.Stmt(ir.NewDecl(pos, ir.ODCL, ftmp)))
		rw.rf.Body.Append(assignNode(pos, ftmp, rwCall.Fun))
		fnNode = ftmp
	}
	rw.tmpSeq++

	thunk := rw.makeZeroArgThunk(pos, func(clo *ir.Func) {
		var cArgs []ir.Node
		for _, a := range evalArgs {
			cArgs = append(cArgs, ir.NewClosureVar(pos, clo, a.(*ir.Name)))
		}
		var cFun ir.Node = fnNode
		if nm, ok := fnNode.(*ir.Name); ok && nm.Class == ir.PAUTO {
			cFun = ir.NewClosureVar(pos, clo, nm)
		} else if sel, ok := fnNode.(*ir.SelectorExpr); ok {
			if rnm, ok := sel.X.(*ir.Name); ok && rnm.Class == ir.PAUTO {
				sel2 := ir.NewSelectorExpr(pos, sel.Op(), ir.NewClosureVar(pos, clo, rnm), sel.Sel)
				sel2.Selection = sel.Selection
				sel2.SetType(sel.Type())
				sel2.SetTypecheck(1)
				cFun = sel2
			}
		}
		callExpr = typecheck.Call(pos, cFun, cArgs, rwCall.IsDDD).(*ir.CallExpr)
		clo.Body.Append(typecheck.Stmt(callExpr))
	})
	spawnCall := typecheck.Call(pos, typecheck.LookupRuntime("coroSpawnOpaque"), []ir.Node{rw.ctxVar, thunk}, false)
	rw.rf.Body.Append(typecheck.Stmt(spawnCall))
}

func (rw *bodyRewriter) emitDeferStmt(defSt *ir.GoDeferStmt) {
	pos := defSt.Pos()
	call, ok := defSt.Call.(*ir.CallExpr)
	if !ok {
		return
	}
	call = unwrapGoDeferCall(call)

	// 1. Lowered coroutine callee (including suspending deferred closure literal!).
	if callee := rw.g.isLoweredCallee(call); callee != nil {
		calleeInfo := rw.plan.Lowered[callee]
		args := rw.rewriteCalleeArgs(call, callee)
		frameVar := newLocal(rw.rf, pos, fmt.Sprintf(".df%d", rw.tmpSeq), types.Types[types.TUNSAFEPTR])
		resVar := newLocal(rw.rf, pos, fmt.Sprintf(".dr%d", rw.tmpSeq), calleeInfo.FactoryFn.Type().Results()[1].Type)
		rw.tmpSeq++
		facCall := typecheck.Call(pos, calleeInfo.FactoryFn.Nname, args, false)
		al := ir.NewAssignListStmt(pos, ir.OAS2, []ir.Node{frameVar, resVar}, []ir.Node{facCall})
		rw.rf.Body.Append(typecheck.Stmt(al))
		dc := typecheck.Call(pos, typecheck.LookupRuntime("coroDeferCoro"), []ir.Node{rw.ctxVar, frameVar, resVar}, false)
		rw.rf.Body.Append(typecheck.Stmt(dc))
		return
	}

	// 2. Direct plain deferred closure literal `defer func(params...) { ... }(args...)`
	//    Rewrite direct recover() inside it to runtime.coroRecover(ctx)!
	if cloExpr, ok := call.Fun.(*ir.ClosureExpr); ok && cloExpr.Func != nil {
		origClo := cloExpr.Func
		rw.deadClo[origClo] = true
		// Evaluate arguments at defer site per Go spec.
		var evalArgs []*ir.Name
		for i, a := range call.Args {
			ra := rw.rewriteNode(a)
			tmp := newLocal(rw.rf, pos, fmt.Sprintf(".da%d_%d", rw.tmpSeq, i), a.Type())
			rw.rf.Body.Append(typecheck.Stmt(ir.NewDecl(pos, ir.ODCL, tmp)))
			rw.rf.Body.Append(assignNode(pos, tmp, ra))
			evalArgs = append(evalArgs, tmp)
		}
		rw.tmpSeq++
		thunk := rw.makeZeroArgThunk(pos, func(clo *ir.Func) {
			ctxCap := ir.NewClosureVar(pos, clo, rw.ctxVar)
			fCap := ir.NewClosureVar(pos, clo, rw.fVar)
			// Bind original closure parameters from captured evalArgs.
			paramMap := make(map[*ir.Name]ir.Node)
			for i, p := range origClo.Type().Params() {
				if p.Nname != nil && i < len(evalArgs) {
					paramMap[p.Nname.(*ir.Name).Canonical()] = ir.NewClosureVar(pos, clo, evalArgs[i])
				}
			}
			// Rebind captures from outer lowered func to fCap.<field>.
			cloRW := &nestedClosureRewriter{
				parentRW:    rw,
				clo:         clo,
				ctxCap:      ctxCap,
				fCap:        fCap,
				paramMap:    paramMap,
				localMap:    make(map[*ir.Name]*ir.Name),
				rewriteRecv: true,
			}
			for _, s := range origClo.Body {
				clo.Body.Append(typecheck.Stmt(cloRW.rewrite(s)))
			}
		})
		dc := typecheck.Call(pos, typecheck.LookupRuntime("coroDefer"), []ir.Node{rw.ctxVar, thunk}, false)
		rw.rf.Body.Append(typecheck.Stmt(dc))
		return
	}

	// 3. Plain function/method/interface call: evaluate receiver & args now, wrap in thunk, push coroDefer.
	rwCall := rw.rewriteNode(call).(*ir.CallExpr)
	var evalArgs []*ir.Name
	for i, a := range rwCall.Args {
		tmp := newLocal(rw.rf, pos, fmt.Sprintf(".da%d_%d", rw.tmpSeq, i), a.Type())
		rw.rf.Body.Append(typecheck.Stmt(ir.NewDecl(pos, ir.ODCL, tmp)))
		rw.rf.Body.Append(assignNode(pos, tmp, a))
		evalArgs = append(evalArgs, tmp)
	}
	var fnNode ir.Node = rwCall.Fun
	if sel, ok := rwCall.Fun.(*ir.SelectorExpr); ok && (sel.Op() == ir.ODOTMETH || sel.Op() == ir.ODOTINTER) {
		rtmp := newLocal(rw.rf, pos, fmt.Sprintf(".dr%d", rw.tmpSeq), sel.X.Type())
		rw.rf.Body.Append(typecheck.Stmt(ir.NewDecl(pos, ir.ODCL, rtmp)))
		rw.rf.Body.Append(assignNode(pos, rtmp, sel.X))
		selCopy := ir.NewSelectorExpr(pos, sel.Op(), rtmp, sel.Sel)
		selCopy.Selection = sel.Selection
		selCopy.SetType(sel.Type())
		selCopy.SetTypecheck(1)
		fnNode = selCopy
	} else if _, isStatic := rwCall.Fun.(*ir.Name); !isStatic {
		ftmp := newLocal(rw.rf, pos, fmt.Sprintf(".df%d", rw.tmpSeq), rwCall.Fun.Type())
		rw.rf.Body.Append(typecheck.Stmt(ir.NewDecl(pos, ir.ODCL, ftmp)))
		rw.rf.Body.Append(assignNode(pos, ftmp, rwCall.Fun))
		fnNode = ftmp
	}
	rw.tmpSeq++

	thunk := rw.makeZeroArgThunk(pos, func(clo *ir.Func) {
		var cArgs []ir.Node
		for _, a := range evalArgs {
			cArgs = append(cArgs, ir.NewClosureVar(pos, clo, a))
		}
		var cFun ir.Node = fnNode
		if nm, ok := fnNode.(*ir.Name); ok && nm.Class == ir.PAUTO {
			cFun = ir.NewClosureVar(pos, clo, nm)
		} else if sel, ok := fnNode.(*ir.SelectorExpr); ok {
			if rnm, ok := sel.X.(*ir.Name); ok && rnm.Class == ir.PAUTO {
				sel2 := ir.NewSelectorExpr(pos, sel.Op(), ir.NewClosureVar(pos, clo, rnm), sel.Sel)
				sel2.Selection = sel.Selection
				sel2.SetType(sel.Type())
				sel2.SetTypecheck(1)
				cFun = sel2
			}
		}
		ce := typecheck.Call(pos, cFun, cArgs, rwCall.IsDDD)
		clo.Body.Append(typecheck.Stmt(ce))
	})
	dc := typecheck.Call(pos, typecheck.LookupRuntime("coroDefer"), []ir.Node{rw.ctxVar, thunk}, false)
	rw.rf.Body.Append(typecheck.Stmt(dc))
}

func (rw *bodyRewriter) makeZeroArgThunk(pos src.XPos, build func(clo *ir.Func)) *ir.ClosureExpr {
	sig := types.NewSignature(nil, nil, nil)
	clo := ir.NewClosureFunc(pos, pos, ir.OCLOSURE, sig, rw.info.Orig, rw.plan.Pkg, 1000+rw.tmpSeq)
	clo.ClosureParent = rw.rf
	clo.DeclareParams(true)
	rw.tmpSeq++
	ir.WithFunc(clo, func() {
		build(clo)
		typecheck.Stmts(clo.Body)
	})
	return clo.OClosure
}

func (rw *bodyRewriter) rewriteCalleeArgs(call *ir.CallExpr, callee *ir.Func) []ir.Node {
	var args []ir.Node
	calleeInfo := rw.plan.Lowered[callee]
	if clo, ok := call.Fun.(*ir.ClosureExpr); ok && clo.Func == callee {
		for i, cv := range callee.ClosureVars {
			outer := cv.Outer
			var arg ir.Node = rw.rewriteNode(outer)
			if calleeInfo != nil && i < len(calleeInfo.CapByRef) && calleeInfo.CapByRef[i] {
				arg = typecheck.NodAddrAt(call.Pos(), arg)
			}
			args = append(args, arg)
		}
	}
	if sel, ok := call.Fun.(*ir.SelectorExpr); ok && sel.Op() == ir.ODOTMETH {
		args = append(args, rw.rewriteNode(sel.X))
	}
	for _, a := range call.Args {
		args = append(args, rw.rewriteNode(a))
	}
	return args
}

func (rw *bodyRewriter) setState(pos src.XPos, st uint32) {
	rw.rf.Body.Append(assignNode(pos, fieldSel(pos, rw.fVar, rw.info.StateField),
		typecheck.Conv(ir.NewInt(pos, int64(st)), types.Types[types.TUINT32])))
}

func (rw *bodyRewriter) returnAction(pos src.XPos, act uint8) ir.Node {
	return typecheck.Stmt(ir.NewReturnStmt(pos, []ir.Node{
		typecheck.Conv(ir.NewInt(pos, int64(act)), types.Types[types.TUINT8]),
	}))
}

func (rw *bodyRewriter) emitTerm(t *blockTerm, bbLabels []*types.Sym) {
	pos := t.pos
	switch t.kind {
	case termGoto:
		rw.rf.Body.Append(typecheck.Stmt(ir.NewBranchStmt(pos, ir.OGOTO, bbLabels[t.target.id])))

	case termBranch:
		ifn := ir.NewIfStmt(pos, rw.rewriteNode(t.cond),
			[]ir.Node{ir.NewBranchStmt(pos, ir.OGOTO, bbLabels[t.thenBB.id])},
			[]ir.Node{ir.NewBranchStmt(pos, ir.OGOTO, bbLabels[t.elseBB.id])},
		)
		rw.rf.Body.Append(typecheck.Stmt(ifn))

	case termReturn:
		rw.rf.Body.Append(rw.returnAction(pos, coroActComplete))

	case termYield:
		rw.setState(pos, t.target.resumeState)
		rw.rf.Body.Append(rw.returnAction(pos, coroActYield))

	case termGC:
		rw.setState(pos, t.target.resumeState)
		gcCall := typecheck.Call(pos, typecheck.LookupRuntime("coroGC"), []ir.Node{rw.ctxVar}, false)
		rw.rf.Body.Append(typecheck.Stmt(gcCall))
		rw.rf.Body.Append(rw.returnAction(pos, coroActWait))

	case termSleep:
		rw.setState(pos, t.target.resumeState)
		dur := typecheck.Conv(rw.rewriteNode(t.arg), types.Types[types.TINT64])
		slCall := typecheck.Call(pos, typecheck.LookupRuntime("coroSleep"), []ir.Node{rw.ctxVar, dur}, false)
		ifn := ir.NewIfStmt(pos, slCall,
			[]ir.Node{rw.returnAction(pos, coroActWait)},
			[]ir.Node{ir.NewBranchStmt(pos, ir.OGOTO, bbLabels[t.target.id])},
		)
		rw.rf.Body.Append(typecheck.Stmt(ifn))

	case termChanSend:
		rw.setState(pos, t.target.resumeState)
		ch := rw.rewriteNode(t.ch)
		elemT := ch.Type().Elem()
		elemPtr := typecheck.NodAddrAt(pos, rw.rewriteNode(t.elemTmp))
		fn := typecheck.LookupRuntime("coroChanSend", elemT, elemT)
		sc := typecheck.Call(pos, fn, []ir.Node{rw.ctxVar, ch, elemPtr}, false)
		ifn := ir.NewIfStmt(pos, sc,
			[]ir.Node{rw.returnAction(pos, coroActWait)},
			[]ir.Node{ir.NewBranchStmt(pos, ir.OGOTO, bbLabels[t.target.id])},
		)
		rw.rf.Body.Append(typecheck.Stmt(ifn))

	case termChanRecv:
		rw.setState(pos, t.target.resumeState)
		ch := rw.rewriteNode(t.ch)
		elemT := ch.Type().Elem()
		var elemPtr ir.Node
		if t.elemTmp != nil {
			elemPtr = typecheck.NodAddrAt(pos, rw.rewriteNode(t.elemTmp))
		} else {
			elemPtr = ir.NewNilExpr(pos, types.NewPtr(elemT))
		}
		var okPtr ir.Node
		if t.okDst != nil && rw.g.chanOKField != nil {
			okPtr = typecheck.NodAddrAt(pos, fieldSel(pos, rw.fVar, rw.g.chanOKField))
		} else {
			okPtr = ir.NewNilExpr(pos, types.NewPtr(types.Types[types.TBOOL]))
		}
		fn := typecheck.LookupRuntime("coroChanRecv", elemT, elemT)
		rc := typecheck.Call(pos, fn, []ir.Node{rw.ctxVar, ch, elemPtr, okPtr}, false)
		ifn := ir.NewIfStmt(pos, rc,
			[]ir.Node{rw.returnAction(pos, coroActWait)},
			[]ir.Node{ir.NewBranchStmt(pos, ir.OGOTO, bbLabels[t.target.id])},
		)
		rw.rf.Body.Append(typecheck.Stmt(ifn))

	case termSelect:
		rw.setState(pos, t.target.resumeState)
		nsends, nrecvs := 0, 0
		arr := fieldSel(pos, rw.fVar, rw.g.selCasesField)
		scaseType := arr.Type().Elem()
		for i, c := range t.selCases {
			if c.isSend {
				nsends++
			} else {
				nrecvs++
			}
			slot := typecheck.Expr(ir.NewIndexExpr(pos, arr, ir.NewInt(pos, int64(i))))
			cField := fieldSelExpr(pos, slot, scaseType.Field(0))
			eField := fieldSelExpr(pos, slot, scaseType.Field(1))
			chNode := rw.rewriteNode(c.ch)
			rw.rf.Body.Append(assignNode(pos, cField, typecheck.ConvNop(chNode, types.Types[types.TUNSAFEPTR])))
			if c.elemTmp != nil {
				ep := typecheck.ConvNop(typecheck.NodAddrAt(pos, rw.rewriteNode(c.elemTmp)), types.Types[types.TUNSAFEPTR])
				rw.rf.Body.Append(assignNode(pos, eField, ep))
			} else {
				rw.rf.Body.Append(assignNode(pos, eField, ir.NewNilExpr(pos, types.Types[types.TUNSAFEPTR])))
			}
		}
		var cas0 ir.Node
		if len(t.selCases) > 0 {
			idx0 := typecheck.Expr(ir.NewIndexExpr(pos, arr, ir.NewInt(pos, 0)))
			cas0 = typecheck.ConvNop(typecheck.NodAddrAt(pos, idx0), types.NewPtr(types.Types[types.TUINT8]))
		} else {
			cas0 = ir.NewNilExpr(pos, types.NewPtr(types.Types[types.TUINT8]))
		}
		selCall := typecheck.Call(pos, typecheck.LookupRuntime("coroSelect"), []ir.Node{
			rw.ctxVar,
			cas0,
			ir.NewInt(pos, int64(nsends)),
			ir.NewInt(pos, int64(nrecvs)),
			ir.NewBool(pos, !t.hasDefault),
			typecheck.NodAddrAt(pos, fieldSel(pos, rw.fVar, rw.g.selChosenField)),
			typecheck.NodAddrAt(pos, fieldSel(pos, rw.fVar, rw.g.selOKField)),
		}, false)
		ifn := ir.NewIfStmt(pos, selCall,
			[]ir.Node{rw.returnAction(pos, coroActWait)},
			[]ir.Node{ir.NewBranchStmt(pos, ir.OGOTO, bbLabels[t.target.id])},
		)
		rw.rf.Body.Append(typecheck.Stmt(ifn))

	case termIO:
		rw.setState(pos, t.target.resumeState)
		recv := rw.rewriteNode(t.ioRecv)
		buf := rw.rewriteNode(t.ioBuf)
		startMethod := "coroReadStart"
		if t.ioKind == ioFileWrite || t.ioKind == ioConnWrite {
			startMethod = "coroWriteStart"
		}
		startCall := callStdlibIOMethod(pos, recv, t.ioKind, startMethod, []ir.Node{
			rw.ctxVar,
			buf,
			typecheck.NodAddrAt(pos, fieldSel(pos, rw.fVar, rw.g.ioNField)),
			typecheck.NodAddrAt(pos, fieldSel(pos, rw.fVar, rw.g.ioErrField)),
			typecheck.NodAddrAt(pos, fieldSel(pos, rw.fVar, rw.g.ioDirectField)),
			typecheck.NodAddrAt(pos, fieldSel(pos, rw.fVar, rw.g.ioStatusField)),
		})
		ifn := ir.NewIfStmt(pos, startCall,
			[]ir.Node{rw.returnAction(pos, coroActWait)},
			[]ir.Node{ir.NewBranchStmt(pos, ir.OGOTO, bbLabels[t.target.id])},
		)
		rw.rf.Body.Append(typecheck.Stmt(ifn))

	case termAwait:
		rw.setState(pos, t.target.resumeState)
		calleeInfo := rw.plan.Lowered[t.awaitCallee]
		var args []ir.Node
		for _, a := range t.awaitArgs {
			args = append(args, rw.rewriteNode(a))
		}
		frameVar := newLocal(rw.rf, pos, fmt.Sprintf(".af%d", rw.tmpSeq), types.Types[types.TUNSAFEPTR])
		resVar := newLocal(rw.rf, pos, fmt.Sprintf(".ar%d", rw.tmpSeq), calleeInfo.FactoryFn.Type().Results()[1].Type)
		rw.tmpSeq++
		facCall := typecheck.Call(pos, calleeInfo.FactoryFn.Nname, args, false)
		al := ir.NewAssignListStmt(pos, ir.OAS2, []ir.Node{frameVar, resVar}, []ir.Node{facCall})
		rw.rf.Body.Append(typecheck.Stmt(al))
		rw.rf.Body.Append(assignNode(pos, fieldSel(pos, rw.fVar, rw.g.awaitChildField), frameVar))
		awaitCall := typecheck.Call(pos, typecheck.LookupRuntime("coroAwaitFrame"), []ir.Node{rw.ctxVar, frameVar, resVar}, false)
		rw.rf.Body.Append(typecheck.Stmt(ir.NewReturnStmt(pos, []ir.Node{awaitCall})))

	case termPanic:
		val := rw.rewriteNode(t.arg)
		if !val.Type().IsEmptyInterface() {
			val = typecheck.AssignConv(val, types.Types[types.TINTER], "panic")
		}
		pc := typecheck.Call(pos, typecheck.LookupRuntime("coroPanic"), []ir.Node{rw.ctxVar, val}, false)
		rw.rf.Body.Append(typecheck.Stmt(pc))
		rw.rf.Body.Append(rw.returnAction(pos, coroActPanic))

	case termGoexit:
		gc := typecheck.Call(pos, typecheck.LookupRuntime("coroGoexit"), []ir.Node{rw.ctxVar}, false)
		rw.rf.Body.Append(typecheck.Stmt(gc))
		rw.rf.Body.Append(rw.returnAction(pos, coroActGoexit))
	}
}

func (rw *bodyRewriter) mapStackLocal(orig *ir.Name) *ir.Name {
	if mapped, ok := rw.stackLocs[orig]; ok {
		return mapped
	}
	nm := newLocal(rw.rf, orig.Pos(), orig.Sym().Name, orig.Type())
	rw.stackLocs[orig] = nm
	return nm
}

func (rw *bodyRewriter) rewriteNode(n ir.Node) ir.Node {
	if n == nil {
		return nil
	}
	var edit func(ir.Node) ir.Node
	edit = func(x ir.Node) ir.Node {
		if x == nil {
			return nil
		}
		// Clean up ODCL inside Init of statements for frame-resident locals.
		if inits := x.Init(); len(inits) > 0 {
			var kept ir.Nodes
			for _, st := range inits {
				if d, ok := st.(*ir.Decl); ok && rw.info.VarFields[d.X.Canonical()] != nil {
					continue
				}
				kept.Append(edit(st))
			}
			if in, ok := x.(ir.InitNode); ok {
				*in.PtrInit() = kept
			}
		}

		switch v := x.(type) {
		case *ir.Name:
			c := v.Canonical()
			if fld, ok := rw.byRefCaps[c]; ok {
				return typecheck.Expr(ir.NewStarExpr(v.Pos(), fieldSel(v.Pos(), rw.fVar, fld)))
			}
			if fld := rw.info.VarFields[c]; fld != nil {
				return fieldSel(v.Pos(), rw.fVar, fld)
			}
			if c.Class == ir.PAUTO && c.Curfn == rw.info.Orig {
				return rw.mapStackLocal(c)
			}
			return v

		case *ir.CallExpr:
			if v.Op() == ir.ORECOVER {
				return typecheck.Call(v.Pos(), typecheck.LookupRuntime("coroRecover"), []ir.Node{rw.ctxVar}, false)
			}
			ir.EditChildren(v, edit)
			return v

		case *ir.ClosureExpr:
			// Nested closure literal appearing as a value (not directly invoked in go/defer):
			// rebind any captures of Orig's locals to capture fVar instead!
			if v.Func != nil && rw.plan.Lowered[v.Func] == nil {
				rw.rebindNestedClosure(v.Func)
			}
			return v

		case *ir.AssignStmt:
			v.Def = false
			ir.EditChildren(v, edit)
			return v

		case *ir.AssignListStmt:
			v.Def = false
			ir.EditChildren(v, edit)
			return v

		case *ir.Decl:
			if rw.info.VarFields[v.X.Canonical()] != nil {
				return ir.NewBlockStmt(v.Pos(), nil)
			}
			if v.X.Canonical().Class == ir.PAUTO && v.X.Canonical().Curfn == rw.info.Orig {
				v.X = rw.mapStackLocal(v.X.Canonical())
			}
			return v

		default:
			ir.EditChildren(x, edit)
			return x
		}
	}
	return edit(n)
}

func (rw *bodyRewriter) rebindNestedClosure(clo *ir.Func) {
	if clo.ClosureParent != rw.info.Orig {
		return
	}
	var newCaps []*ir.Name
	var fCap *ir.Name
	remap := make(map[*ir.Name]*types.Field)
	byRefRemap := make(map[*ir.Name]*types.Field)

	for _, cv := range clo.ClosureVars {
		if cv.Outer != nil && cv.Outer.Curfn == rw.info.Orig {
			c := cv.Outer.Canonical()
			if fld := rw.info.VarFields[c]; fld != nil {
				if fCap == nil {
					fCap = ir.NewClosureVar(clo.Pos(), clo, rw.fVar)
					newCaps = append(newCaps, fCap)
				}
				if bFld, ok := rw.byRefCaps[c]; ok {
					byRefRemap[cv.Canonical()] = bFld
				} else {
					remap[cv.Canonical()] = fld
				}
				continue
			}
		}
		newCaps = append(newCaps, cv)
	}
	if fCap == nil {
		clo.ClosureParent = rw.rf
		return
	}
	clo.ClosureParent = rw.rf
	clo.ClosureVars = newCaps
	ir.WithFunc(clo, func() {
		var edit func(ir.Node) ir.Node
		edit = func(n ir.Node) ir.Node {
			if nm, ok := n.(*ir.Name); ok {
				c := nm.Canonical()
				if fld, ok := byRefRemap[c]; ok {
					return typecheck.Expr(ir.NewStarExpr(nm.Pos(), fieldSel(nm.Pos(), fCap, fld)))
				}
				if fld, ok := remap[c]; ok {
					return fieldSel(nm.Pos(), fCap, fld)
				}
			}
			ir.EditChildren(n, edit)
			return n
		}
		for i, s := range clo.Body {
			clo.Body[i] = edit(s)
		}
	})
}

type nestedClosureRewriter struct {
	parentRW    *bodyRewriter
	clo         *ir.Func
	ctxCap      *ir.Name
	fCap        *ir.Name
	paramMap    map[*ir.Name]ir.Node
	localMap    map[*ir.Name]*ir.Name
	rewriteRecv bool
}

func (cr *nestedClosureRewriter) rewrite(n ir.Node) ir.Node {
	var edit func(ir.Node) ir.Node
	edit = func(x ir.Node) ir.Node {
		if x == nil {
			return nil
		}
		if inits := x.Init(); len(inits) > 0 {
			var kept ir.Nodes
			for _, st := range inits {
				kept.Append(edit(st))
			}
			if in, ok := x.(ir.InitNode); ok {
				*in.PtrInit() = kept
			}
		}
		switch v := x.(type) {
		case *ir.Name:
			c := v.Canonical()
			if repl, ok := cr.paramMap[c]; ok {
				return repl
			}
			// Check if this is a closure var from the original deferred closure.
			// Note: check v.IsClosureVar() on v itself, because v.Canonical() unwraps to Outer!
			if v.IsClosureVar() && v.Outer != nil {
				oc := v.Outer.Canonical()
				if fld, ok := cr.parentRW.byRefCaps[oc]; ok {
					return typecheck.Expr(ir.NewStarExpr(v.Pos(), fieldSel(v.Pos(), cr.fCap, fld)))
				}
				if fld := cr.parentRW.info.VarFields[oc]; fld != nil {
					return fieldSel(v.Pos(), cr.fCap, fld)
				}
				if oc.Class == ir.PAUTO && oc.Curfn == cr.parentRW.info.Orig {
					return ir.NewClosureVar(v.Pos(), cr.clo, cr.parentRW.mapStackLocal(oc))
				}
			}
			if c.Class == ir.PAUTO && c.Curfn != cr.clo {
				if mapped, ok := cr.localMap[c]; ok {
					return mapped
				}
				nm := newLocal(cr.clo, v.Pos(), v.Sym().Name, v.Type())
				cr.localMap[c] = nm
				return nm
			}
			return v
		case *ir.CallExpr:
			if cr.rewriteRecv && v.Op() == ir.ORECOVER {
				return typecheck.Call(v.Pos(), typecheck.LookupRuntime("coroRecover"), []ir.Node{cr.ctxCap}, false)
			}
			ir.EditChildren(v, edit)
			return v
		case *ir.ClosureExpr:
			// Do NOT rewrite recover() inside deeper nested closures (per Go spec).
			return v
		default:
			ir.EditChildren(x, edit)
			return x
		}
	}
	return edit(n)
}

func callStdlibIOMethod(pos src.XPos, recv ir.Node, kind ioKind, methodName string, args []ir.Node) ir.Node {
	pkg := recv.Type().Elem().Sym().Pkg
	if (kind == ioConnRead || kind == ioConnWrite) && recv.Type().Elem().Sym().Name != "conn" {
		// Unwrap embedded net.conn so the call directly targets net.(*conn).coroReadStart/Finish.
		connSym := pkg.Lookup("conn")
		connField := typecheck.Expr(ir.NewSelectorExpr(pos, ir.OXDOT, recv, connSym))
		recv = typecheck.NodAddrAt(pos, connField)
	}
	sym := pkg.Lookup(methodName)
	mexpr := typecheck.NewMethodExpr(pos, recv.Type(), sym)
	fullArgs := make([]ir.Node, 0, 1+len(args))
	fullArgs = append(fullArgs, recv)
	fullArgs = append(fullArgs, args...)
	return typecheck.Call(pos, mexpr, fullArgs, false)
}

func newLocal(fn *ir.Func, pos src.XPos, name string, t *types.Type) *ir.Name {
	nm := ir.NewNameAt(pos, types.LocalPkg.Lookup(name), t)
	nm.Class = ir.PAUTO
	nm.Curfn = fn
	nm.SetTypecheck(1)
	fn.Dcl = append(fn.Dcl, nm)
	return nm
}

func ensureParamName(fn *ir.Func, pos src.XPos, f *types.Field, fallback string) *ir.Name {
	if f.Nname != nil {
		return f.Nname.(*ir.Name)
	}
	nm := ir.NewNameAt(pos, types.LocalPkg.Lookup(fallback), f.Type)
	nm.Class = ir.PPARAM
	nm.Curfn = fn
	nm.SetTypecheck(1)
	f.Nname = nm
	fn.Dcl = append(fn.Dcl, nm)
	return nm
}

func assignNode(pos src.XPos, lhs, rhs ir.Node) ir.Node {
	if rhs.Type() != nil && lhs.Type() != nil && !types.Identical(rhs.Type(), lhs.Type()) {
		rhs = typecheck.AssignConv(rhs, lhs.Type(), "coro")
	}
	return typecheck.Stmt(ir.NewAssignStmt(pos, lhs, rhs))
}

func fieldSel(pos src.XPos, fptr *ir.Name, fld *types.Field) *ir.SelectorExpr {
	sel := ir.NewSelectorExpr(pos, ir.ODOTPTR, fptr, fld.Sym)
	sel.Selection = fld
	sel.SetType(fld.Type)
	sel.SetTypecheck(1)
	return sel
}

func fieldSelExpr(pos src.XPos, baseExpr ir.Node, fld *types.Field) *ir.SelectorExpr {
	sel := ir.NewSelectorExpr(pos, ir.ODOT, baseExpr, fld.Sym)
	sel.Selection = fld
	sel.SetType(fld.Type)
	sel.SetTypecheck(1)
	return sel
}
