// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/reflectdata"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"fmt"
)

type termKind uint8

const (
	termNone termKind = iota
	termGoto
	termBranch
	termReturn
	termYield
	termGC
	termSleep
	termChanSend
	termChanRecv
	termSelect
	termIO
	termAwait
	termPanic
	termGoexit
)

type selCaseCFG struct {
	isSend  bool
	ch      ir.Node
	val     ir.Node  // for send: temp holding value to send
	dst     ir.Node  // for recv: LHS to assign received value (may be nil/blank)
	okDst   ir.Node  // for recv: LHS for comma-ok bool (may be nil/blank)
	elemTmp *ir.Name // pinned frame temp for send/recv element address
	bodyBB  *basicBlock
}

type blockTerm struct {
	kind   termKind
	pos    src.XPos
	target *basicBlock // next / goto / resume block
	thenBB *basicBlock // termBranch true target
	elseBB *basicBlock // termBranch false target
	cond   ir.Node     // termBranch condition
	arg    ir.Node     // termSleep duration or termPanic payload

	// termChanSend / termChanRecv
	ch      ir.Node
	val     ir.Node
	dst     ir.Node
	okDst   ir.Node
	elemTmp *ir.Name

	// termSelect
	selCases   []selCaseCFG
	hasDefault bool
	defaultBB  *basicBlock

	// termIO
	ioKind   ioKind
	ioRecv   ir.Node
	ioBuf    ir.Node
	ioNDst   ir.Node
	ioErrDst ir.Node

	// termAwait
	awaitCallee *ir.Func
	awaitArgs   []ir.Node // captures (if closure) + receiver (if method) + regular args
	awaitDsts   []ir.Node
}

type basicBlock struct {
	id          int
	resumeState uint32 // >0 if this block is a suspension resume entry point
	stmts       []ir.Node
	term        blockTerm
	succs       []*basicBlock
}

type funcCFG struct {
	plan         *Plan
	info         *FuncInfo
	fn           *ir.Func
	blocks       []*basicBlock
	cur          *basicBlock
	temps        []*ir.Name
	pinnedLocals map[*ir.Name]bool
	tempSeq      int
	resumeSeq    uint32

	// Labels and loop/switch break/continue targets.
	labels        map[*types.Sym]*basicBlock
	breakStack    []*basicBlock
	continueStack []*basicBlock
	labelBreak    map[*types.Sym]*basicBlock
	labelContinue map[*types.Sym]*basicBlock

	// Frame scratch requirements (filled during CFG construction, bound in frame.go).
	needAwaitChild bool
	needIO         bool
	needChanOK     bool
	maxSelectCases int
	hasEscapingClo bool

	awaitChildField *types.Field
	ioNField        *types.Field
	ioErrField      *types.Field
	ioDirectField   *types.Field
	ioStatusField   *types.Field
	chanOKField     *types.Field
	selCasesField   *types.Field
	selChosenField  *types.Field
	selOKField      *types.Field
}

func buildCFG(plan *Plan, info *FuncInfo) *funcCFG {
	g := &funcCFG{
		plan:          plan,
		info:          info,
		fn:            info.Orig,
		pinnedLocals:  make(map[*ir.Name]bool),
		labels:        make(map[*types.Sym]*basicBlock),
		labelBreak:    make(map[*types.Sym]*basicBlock),
		labelContinue: make(map[*types.Sym]*basicBlock),
	}

	// Pin any local captured by a nested closure (plain defer or escaping closure),
	// excluding synthetic .gowrap/.deferwrap wrappers which we unwrap directly.
	ir.VisitList(info.Orig.Body, func(n ir.Node) {
		clo, ok := n.(*ir.ClosureExpr)
		if !ok || clo.Func == nil || isGoDeferWrapper(clo.Func) {
			return
		}
		if plan.Lowered[clo.Func] == nil {
			for _, cv := range clo.Func.ClosureVars {
				if cv.Outer != nil && cv.Outer.Curfn == info.Orig {
					g.pinnedLocals[cv.Outer.Canonical()] = true
				}
			}
		}
	})

	entry := g.newBlock()
	g.cur = entry
	ir.WithFunc(info.Orig, func() {
		g.lowerStmts(info.Orig.Body)
	})
	if g.cur != nil && g.cur.term.kind == termNone {
		g.cur.term = blockTerm{kind: termReturn, pos: info.Orig.Endlineno}
	}

	// Compute CFG successor lists for liveness analysis.
	for _, bb := range g.blocks {
		bb.succs = termSuccs(&bb.term)
	}
	// Connect termSelect's resume dispatch block to its case/default target blocks.
	for _, bb := range g.blocks {
		if bb.term.kind == termSelect && bb.term.target != nil {
			rb := bb.term.target
			for _, c := range bb.term.selCases {
				if c.bodyBB != nil {
					rb.succs = append(rb.succs, c.bodyBB)
				}
			}
			if bb.term.hasDefault && bb.term.defaultBB != nil {
				rb.succs = append(rb.succs, bb.term.defaultBB)
			}
		}
	}
	return g
}

func termSuccs(t *blockTerm) []*basicBlock {
	switch t.kind {
	case termGoto, termYield, termGC, termSleep, termChanSend, termChanRecv, termSelect, termIO, termAwait:
		if t.target != nil {
			return []*basicBlock{t.target}
		}
	case termBranch:
		var out []*basicBlock
		if t.thenBB != nil {
			out = append(out, t.thenBB)
		}
		if t.elseBB != nil {
			out = append(out, t.elseBB)
		}
		return out
	}
	return nil
}

func (g *funcCFG) newBlock() *basicBlock {
	bb := &basicBlock{id: len(g.blocks)}
	g.blocks = append(g.blocks, bb)
	return bb
}

func (g *funcCFG) newResumeBlock() *basicBlock {
	bb := g.newBlock()
	g.resumeSeq++
	bb.resumeState = g.resumeSeq
	return bb
}

func (g *funcCFG) getLabelBlock(sym *types.Sym) *basicBlock {
	if bb, ok := g.labels[sym]; ok {
		return bb
	}
	bb := g.newBlock()
	g.labels[sym] = bb
	return bb
}

func (g *funcCFG) newTemp(pos src.XPos, t *types.Type, pinned bool) *ir.Name {
	sym := types.LocalPkg.Lookup(fmt.Sprintf(".coro_t%d", g.tempSeq))
	g.tempSeq++
	nm := ir.NewNameAt(pos, sym, t)
	nm.Class = ir.PAUTO
	nm.Curfn = g.fn
	nm.SetTypecheck(1)
	g.temps = append(g.temps, nm)
	if pinned {
		g.pinnedLocals[nm] = true
	}
	return nm
}

func (g *funcCFG) emit(stmt ir.Node) {
	if g.cur == nil || stmt == nil {
		return
	}
	g.cur.stmts = append(g.cur.stmts, stmt)
}

func (g *funcCFG) terminate(term blockTerm) {
	if g.cur == nil {
		return
	}
	g.cur.term = term
	g.cur = nil
}

func (g *funcCFG) ensureCur() {
	if g.cur == nil {
		g.cur = g.newBlock()
	}
}

func (g *funcCFG) lowerStmts(stmts ir.Nodes) {
	for _, s := range stmts {
		g.lowerStmt(s, nil)
	}
}

func (g *funcCFG) nodeHasSuspend(n ir.Node) bool {
	if n == nil {
		return false
	}
	found := false
	ir.Visit(n, func(x ir.Node) {
		if found {
			return
		}
		switch x.Op() {
		case ir.OSEND, ir.ORECV, ir.OAS2RECV, ir.OSELECT, ir.OGOTO, ir.OBREAK, ir.OCONTINUE, ir.OFALL, ir.OLABEL, ir.ORETURN, ir.OPANIC, ir.OGO, ir.ODEFER:
			found = true
		case ir.ORANGE:
			rn := x.(*ir.RangeStmt)
			if rn.X != nil && rn.X.Type() != nil && rn.X.Type().IsChan() {
				found = true
			}
		case ir.OCALLFUNC, ir.OCALLMETH:
			call := x.(*ir.CallExpr)
			if isSuspendingBuiltinCall(call) || matchStdlibIO(call) != ioNone || isRuntimeGoexitCall(call) {
				found = true
				return
			}
			if callee := staticLocalCallee(call, g.loweredSet()); callee != nil {
				found = true
			}
		}
	})
	return found
}

func (g *funcCFG) exprHasSuspend(n ir.Node) bool {
	if n == nil {
		return false
	}
	found := false
	ir.Visit(n, func(x ir.Node) {
		if found {
			return
		}
		switch x.Op() {
		case ir.ORECV:
			found = true
		case ir.OCALLFUNC, ir.OCALLMETH:
			call := x.(*ir.CallExpr)
			if isSuspendingBuiltinCall(call) || matchStdlibIO(call) != ioNone || isRuntimeGoexitCall(call) {
				found = true
				return
			}
			if callee := staticLocalCallee(call, g.loweredSet()); callee != nil {
				found = true
			}
		}
	})
	return found
}

func (g *funcCFG) loweredSet() map[*ir.Func]bool {
	m := make(map[*ir.Func]bool, len(g.plan.Lowered))
	for fn := range g.plan.Lowered {
		m[fn] = true
	}
	return m
}

func (g *funcCFG) isLoweredCallee(call *ir.CallExpr) *ir.Func {
	if call == nil {
		return nil
	}
	switch fn := call.Fun.(type) {
	case *ir.Name:
		if fn.Func != nil && g.plan.Lowered[fn.Func] != nil {
			return fn.Func
		}
	case *ir.ClosureExpr:
		if fn.Func != nil && g.plan.Lowered[fn.Func] != nil {
			return fn.Func
		}
	case *ir.SelectorExpr:
		if fn.Op() == ir.ODOTMETH || fn.Op() == ir.OMETHEXPR {
			if mn := ir.MethodExprName(fn); mn != nil && mn.Func != nil && g.plan.Lowered[mn.Func] != nil {
				return mn.Func
			}
		}
	}
	return nil
}

func isRuntimeGoexitCall(call *ir.CallExpr) bool {
	pkgPath, name := callTargetPkgName(call)
	return pkgPath == "runtime" && name == "Goexit"
}

func (g *funcCFG) lowerInit(n ir.Node) {
	if n == nil {
		return
	}
	in, ok := n.(ir.InitNode)
	if !ok {
		return
	}
	inits := in.Init()
	if len(inits) == 0 {
		return
	}
	*in.PtrInit() = nil
	g.lowerStmts(inits)
}

func (g *funcCFG) lowerStmt(n ir.Node, label *types.Sym) {
	if n == nil {
		return
	}
	if n.Op() == ir.OLABEL {
		lbl := n.(*ir.LabelStmt)
		bb := g.getLabelBlock(lbl.Label)
		if g.cur != nil {
			g.terminate(blockTerm{kind: termGoto, pos: lbl.Pos(), target: bb})
		}
		g.cur = bb
		return
	}
	// If dead code follows a return/goto/panic, start an unreachable block for labels inside.
	if g.cur == nil {
		g.cur = g.newBlock()
	}

	g.lowerInit(n)

	switch n.Op() {
	case ir.OBLOCK:
		g.lowerStmts(n.(*ir.BlockStmt).List)
		return

	case ir.ODCL, ir.OINLMARK:
		// Frame-resident locals have ODCL dropped during rewriting; stack locals keep theirs.
		if n.Op() == ir.ODCL {
			g.emit(n)
		}
		return

	case ir.OGOTO:
		br := n.(*ir.BranchStmt)
		g.terminate(blockTerm{kind: termGoto, pos: br.Pos(), target: g.getLabelBlock(br.Label)})
		return

	case ir.OBREAK:
		br := n.(*ir.BranchStmt)
		var target *basicBlock
		if br.Label != nil {
			target = g.labelBreak[br.Label]
		} else if len(g.breakStack) > 0 {
			target = g.breakStack[len(g.breakStack)-1]
		}
		if target == nil {
			base.FatalfAt(br.Pos(), "coro: unresolved break target")
		}
		g.terminate(blockTerm{kind: termGoto, pos: br.Pos(), target: target})
		return

	case ir.OCONTINUE:
		br := n.(*ir.BranchStmt)
		var target *basicBlock
		if br.Label != nil {
			target = g.labelContinue[br.Label]
		} else if len(g.continueStack) > 0 {
			target = g.continueStack[len(g.continueStack)-1]
		}
		if target == nil {
			base.FatalfAt(br.Pos(), "coro: unresolved continue target")
		}
		g.terminate(blockTerm{kind: termGoto, pos: br.Pos(), target: target})
		return

	case ir.ORETURN:
		ret := n.(*ir.ReturnStmt)
		sig := g.fn.Type()
		results := sig.Results()
		if len(ret.Results) > 0 {
			// Evaluate all return expressions before writing to result fields.
			if len(ret.Results) == 1 && len(results) > 1 {
				// Multi-value return: `return f()`
				call, ok := ret.Results[0].(*ir.CallExpr)
				if ok && (matchStdlibIO(call) != ioNone || g.isLoweredCallee(call) != nil) {
					dsts := make([]ir.Node, len(results))
					for i, r := range results {
						dsts[i] = g.resultName(ret.Pos(), i, r)
					}
					g.lowerMultiCall(ret.Pos(), call, dsts)
					g.terminate(blockTerm{kind: termReturn, pos: ret.Pos()})
					return
				}
				vals := make([]ir.Node, len(results))
				al := ir.NewAssignListStmt(ret.Pos(), ir.OAS2FUNC, nil, []ir.Node{g.flattenExpr(ret.Results[0])})
				for i, r := range results {
					tmp := g.newTemp(ret.Pos(), r.Type, false)
					al.Lhs = append(al.Lhs, tmp)
					vals[i] = tmp
				}
				al.SetTypecheck(1)
				g.emit(al)
				for i, r := range results {
					g.emitAssign(ret.Pos(), g.resultName(ret.Pos(), i, r), vals[i])
				}
			} else {
				tmps := make([]ir.Node, len(ret.Results))
				for i, expr := range ret.Results {
					fe := g.flattenExpr(expr)
					tmp := g.newTemp(ret.Pos(), results[i].Type, false)
					g.emitAssign(ret.Pos(), tmp, fe)
					tmps[i] = tmp
				}
				for i, r := range results {
					g.emitAssign(ret.Pos(), g.resultName(ret.Pos(), i, r), tmps[i])
				}
			}
		}
		g.terminate(blockTerm{kind: termReturn, pos: ret.Pos()})
		return

	case ir.OPANIC:
		pn := n.(*ir.UnaryExpr)
		val := g.flattenExpr(pn.X)
		g.terminate(blockTerm{kind: termPanic, pos: pn.Pos(), arg: val})
		return

	case ir.OSEND:
		sn := n.(*ir.SendStmt)
		ch := g.flattenExpr(sn.Chan)
		val := g.flattenExpr(sn.Value)
		g.emitChanSend(sn.Pos(), ch, val)
		return

	case ir.OIF:
		ifn := n.(*ir.IfStmt)
		if !g.nodeHasSuspend(ifn) {
			g.emit(ifn)
			return
		}
		cond := g.flattenExpr(ifn.Cond)
		thenBB := g.newBlock()
		elseBB := g.newBlock()
		joinBB := g.newBlock()
		g.terminate(blockTerm{kind: termBranch, pos: ifn.Pos(), cond: cond, thenBB: thenBB, elseBB: elseBB})
		g.cur = thenBB
		g.lowerStmts(ifn.Body)
		if g.cur != nil {
			g.terminate(blockTerm{kind: termGoto, pos: ifn.Pos(), target: joinBB})
		}
		g.cur = elseBB
		g.lowerStmts(ifn.Else)
		if g.cur != nil {
			g.terminate(blockTerm{kind: termGoto, pos: ifn.Pos(), target: joinBB})
		}
		g.cur = joinBB
		return

	case ir.OFOR:
		fn := n.(*ir.ForStmt)
		if !g.nodeHasSuspend(fn) {
			g.emit(fn)
			return
		}
		g.lowerFor(fn)
		return

	case ir.ORANGE:
		rn := n.(*ir.RangeStmt)
		if (rn.X == nil || rn.X.Type() == nil || !rn.X.Type().IsChan()) && !g.nodeHasSuspend(rn) {
			g.emit(rn)
			return
		}
		g.lowerRange(rn)
		return

	case ir.OSWITCH:
		sw := n.(*ir.SwitchStmt)
		if !g.nodeHasSuspend(sw) {
			g.emit(sw)
			return
		}
		g.lowerSwitch(sw)
		return

	case ir.OSELECT:
		g.lowerSelect(n.(*ir.SelectStmt))
		return

	case ir.OAS:
		as := n.(*ir.AssignStmt)
		if as.Y != nil && as.Y.Op() == ir.ORECV {
			ch := g.flattenExpr(as.Y.(*ir.UnaryExpr).X)
			var dst ir.Node
			if !ir.IsBlank(as.X) {
				dst = g.flattenLValue(as.X)
			}
			g.emitChanRecv(as.Pos(), ch, dst, nil)
			return
		}
		if as.Y != nil && (as.Y.Op() == ir.OCALLFUNC || as.Y.Op() == ir.OCALLMETH) {
			call := as.Y.(*ir.CallExpr)
			if matchStdlibIO(call) != ioNone || g.isLoweredCallee(call) != nil {
				var dst ir.Node
				if !ir.IsBlank(as.X) {
					dst = g.flattenLValue(as.X)
				}
				g.lowerMultiCall(as.Pos(), call, []ir.Node{dst})
				return
			}
		}
		if g.exprHasSuspend(as.X) || g.exprHasSuspend(as.Y) {
			lhs := g.flattenLValue(as.X)
			rhs := g.flattenExpr(as.Y)
			g.emitAssign(as.Pos(), lhs, rhs)
			return
		}
		g.emit(as)
		return

	case ir.OASOP:
		ao := n.(*ir.AssignOpStmt)
		if g.exprHasSuspend(ao.X) || g.exprHasSuspend(ao.Y) {
			lhs := g.flattenLValue(ao.X)
			rhs := g.flattenExpr(ao.Y)
			ao2 := ir.NewAssignOpStmt(ao.Pos(), ao.AsOp, lhs, rhs)
			ao2.IncDec = ao.IncDec
			ao2.SetTypecheck(1)
			g.emit(ao2)
			return
		}
		g.emit(ao)
		return

	case ir.OAS2RECV:
		al := n.(*ir.AssignListStmt)
		recv := al.Rhs[0].(*ir.UnaryExpr)
		ch := g.flattenExpr(recv.X)
		var dst, okDst ir.Node
		if len(al.Lhs) > 0 && !ir.IsBlank(al.Lhs[0]) {
			dst = g.flattenLValue(al.Lhs[0])
		}
		if len(al.Lhs) > 1 && !ir.IsBlank(al.Lhs[1]) {
			okDst = g.flattenLValue(al.Lhs[1])
		}
		g.emitChanRecv(al.Pos(), ch, dst, okDst)
		return

	case ir.OAS2FUNC:
		al := n.(*ir.AssignListStmt)
		call, ok := al.Rhs[0].(*ir.CallExpr)
		if ok && (matchStdlibIO(call) != ioNone || g.isLoweredCallee(call) != nil) {
			dsts := make([]ir.Node, len(al.Lhs))
			for i, l := range al.Lhs {
				if !ir.IsBlank(l) {
					dsts[i] = g.flattenLValue(l)
				}
			}
			g.lowerMultiCall(al.Pos(), call, dsts)
			return
		}
		if g.exprHasSuspend(al.Rhs[0]) {
			al.Rhs[0] = g.flattenExpr(al.Rhs[0])
		}
		g.emit(al)
		return

	case ir.OAS2, ir.OAS2DOTTYPE, ir.OAS2MAPR:
		al := n.(*ir.AssignListStmt)
		need := false
		for _, r := range al.Rhs {
			if g.exprHasSuspend(r) {
				need = true
				break
			}
		}
		if !need {
			for _, l := range al.Lhs {
				if g.exprHasSuspend(l) {
					need = true
					break
				}
			}
		}
		if !need {
			g.emit(al)
			return
		}
		// Evaluate RHS expressions left-to-right into temps, then assign to LHS.
		tmps := make([]ir.Node, len(al.Rhs))
		for i, r := range al.Rhs {
			fe := g.flattenExpr(r)
			tmp := g.newTemp(al.Pos(), fe.Type(), false)
			g.emitAssign(al.Pos(), tmp, fe)
			tmps[i] = tmp
		}
		for i, l := range al.Lhs {
			if !ir.IsBlank(l) && i < len(tmps) {
				g.emitAssign(al.Pos(), g.flattenLValue(l), tmps[i])
			}
		}
		return

	case ir.ORECV:
		// Standalone `<-ch` statement.
		ch := g.flattenExpr(n.(*ir.UnaryExpr).X)
		g.emitChanRecv(n.Pos(), ch, nil, nil)
		return

	case ir.OCALLFUNC, ir.OCALLMETH:
		call := n.(*ir.CallExpr)
		if g.lowerStandaloneCall(call) {
			return
		}
		if g.exprHasSuspend(call) {
			g.emit(g.flattenCallArgs(call))
			return
		}
		g.emit(call)
		return

	case ir.OGO, ir.ODEFER:
		goSt := n.(*ir.GoDeferStmt)
		if call, ok := goSt.Call.(*ir.CallExpr); ok && g.exprHasSuspend(call) {
			goSt.Call = g.flattenCallArgs(call)
		}
		g.emit(goSt)
		return

	default:
		if g.exprHasSuspend(n) {
			g.emit(g.flattenExpr(n))
			return
		}
		g.emit(n)
	}
}

func (g *funcCFG) resultName(pos src.XPos, idx int, fld *types.Field) *ir.Name {
	if fld.Nname != nil {
		return fld.Nname.(*ir.Name).Canonical()
	}
	// Unnamed result: create a synthetic placeholder PAUTO mapped to info.ResFields[idx].
	// We reuse fn.Type().Results()[idx].Nname if present; otherwise create and attach one.
	sym := types.LocalPkg.Lookup(fmt.Sprintf("~r%d", idx))
	nm := ir.NewNameAt(pos, sym, fld.Type)
	nm.Class = ir.PPARAMOUT
	nm.Curfn = g.fn
	nm.SetTypecheck(1)
	fld.Nname = nm
	return nm
}

func (g *funcCFG) emitAssign(pos src.XPos, lhs, rhs ir.Node) {
	if lhs == nil || ir.IsBlank(lhs) {
		return
	}
	if rhs.Type() != nil && lhs.Type() != nil && !types.Identical(rhs.Type(), lhs.Type()) {
		rhs = typecheck.AssignConv(rhs, lhs.Type(), "coro assign")
	}
	as := ir.NewAssignStmt(pos, lhs, rhs)
	as.SetTypecheck(1)
	g.emit(as)
}

func (g *funcCFG) emitChanSend(pos src.XPos, ch, val ir.Node) {
	elemT := ch.Type().Elem()
	tmp := g.newTemp(pos, elemT, true)
	g.emitAssign(pos, tmp, val)
	nextBB := g.newResumeBlock()
	g.terminate(blockTerm{
		kind:    termChanSend,
		pos:     pos,
		target:  nextBB,
		ch:      ch,
		val:     tmp,
		elemTmp: tmp,
	})
	g.cur = nextBB
}

func (g *funcCFG) emitChanRecv(pos src.XPos, ch, dst, okDst ir.Node) {
	var elemTmp *ir.Name
	if dst != nil && !ir.IsBlank(dst) {
		elemTmp = g.newTemp(pos, ch.Type().Elem(), true)
	}
	if okDst != nil && !ir.IsBlank(okDst) {
		g.needChanOK = true
	}
	nextBB := g.newResumeBlock()
	g.terminate(blockTerm{
		kind:    termChanRecv,
		pos:     pos,
		target:  nextBB,
		ch:      ch,
		dst:     dst,
		okDst:   okDst,
		elemTmp: elemTmp,
	})
	g.cur = nextBB
}

func (g *funcCFG) lowerStandaloneCall(call *ir.CallExpr) bool {
	pos := call.Pos()
	pkgPath, name := callTargetPkgName(call)
	if pkgPath == "runtime" && name == "Gosched" {
		nextBB := g.newResumeBlock()
		g.terminate(blockTerm{kind: termYield, pos: pos, target: nextBB})
		g.cur = nextBB
		return true
	}
	if pkgPath == "runtime" && name == "GC" {
		nextBB := g.newResumeBlock()
		g.terminate(blockTerm{kind: termGC, pos: pos, target: nextBB})
		g.cur = nextBB
		return true
	}
	if pkgPath == "runtime" && name == "Goexit" {
		g.terminate(blockTerm{kind: termGoexit, pos: pos})
		return true
	}
	if pkgPath == "time" && name == "Sleep" && len(call.Args) == 1 {
		dur := g.flattenExpr(call.Args[0])
		nextBB := g.newResumeBlock()
		g.terminate(blockTerm{kind: termSleep, pos: pos, target: nextBB, arg: dur})
		g.cur = nextBB
		return true
	}
	if matchStdlibIO(call) != ioNone || g.isLoweredCallee(call) != nil {
		g.lowerMultiCall(pos, call, nil)
		return true
	}
	return false
}

func (g *funcCFG) lowerMultiCall(pos src.XPos, call *ir.CallExpr, dsts []ir.Node) {
	if kind := matchStdlibIO(call); kind != ioNone {
		sel := call.Fun.(*ir.SelectorExpr)
		var recv, buf ir.Node
		if sel.Op() == ir.OMETHEXPR {
			recv = g.flattenExpr(call.Args[0])
			buf = g.flattenExpr(call.Args[1])
		} else {
			recv = g.flattenExpr(sel.X)
			buf = g.flattenExpr(call.Args[0])
		}
		g.needIO = true
		var nDst, errDst ir.Node
		if len(dsts) > 0 {
			nDst = dsts[0]
		}
		if len(dsts) > 1 {
			errDst = dsts[1]
		}
		nextBB := g.newResumeBlock()
		g.terminate(blockTerm{
			kind:     termIO,
			pos:      pos,
			target:   nextBB,
			ioKind:   kind,
			ioRecv:   recv,
			ioBuf:    buf,
			ioNDst:   nDst,
			ioErrDst: errDst,
		})
		g.cur = nextBB
		return
	}

	callee := g.isLoweredCallee(call)
	if callee == nil {
		base.FatalfAt(pos, "coro: lowerMultiCall on non-lowered callee")
	}
	g.needAwaitChild = true
	args := g.collectCalleeArgs(call, callee)
	nextBB := g.newResumeBlock()
	g.terminate(blockTerm{
		kind:        termAwait,
		pos:         pos,
		target:      nextBB,
		awaitCallee: callee,
		awaitArgs:   args,
		awaitDsts:   dsts,
	})
	g.cur = nextBB
}

func (g *funcCFG) collectCalleeArgs(call *ir.CallExpr, callee *ir.Func) []ir.Node {
	var args []ir.Node
	calleeInfo := g.plan.Lowered[callee]
	// If callee is a closure literal directly invoked here, pass captured outer vars first.
	if clo, ok := call.Fun.(*ir.ClosureExpr); ok && clo.Func == callee {
		for i, cv := range callee.ClosureVars {
			outer := cv.Outer
			if outer == nil {
				base.FatalfAt(call.Pos(), "coro: missing outer var for closure %v", callee)
			}
			var arg ir.Node = outer
			if calleeInfo != nil && i < len(calleeInfo.CapByRef) && calleeInfo.CapByRef[i] {
				arg = typecheck.NodAddrAt(call.Pos(), outer)
			}
			args = append(args, arg)
		}
	}
	// If method call (ODOTMETH), pass receiver next.
	if sel, ok := call.Fun.(*ir.SelectorExpr); ok && sel.Op() == ir.ODOTMETH {
		args = append(args, g.flattenExpr(sel.X))
	}
	for _, a := range call.Args {
		args = append(args, g.flattenExpr(a))
	}
	return args
}

func (g *funcCFG) lowerFor(fn *ir.ForStmt) {
	pos := fn.Pos()
	condBB := g.newBlock()
	bodyBB := g.newBlock()
	postBB := g.newBlock()
	afterBB := g.newBlock()

	g.terminate(blockTerm{kind: termGoto, pos: pos, target: condBB})

	g.cur = condBB
	if fn.Cond != nil {
		cond := g.flattenExpr(fn.Cond)
		g.terminate(blockTerm{kind: termBranch, pos: pos, cond: cond, thenBB: bodyBB, elseBB: afterBB})
	} else {
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: bodyBB})
	}

	g.breakStack = append(g.breakStack, afterBB)
	g.continueStack = append(g.continueStack, postBB)
	if fn.Label != nil {
		g.labelBreak[fn.Label] = afterBB
		g.labelContinue[fn.Label] = postBB
	}

	g.cur = bodyBB
	g.lowerStmts(fn.Body)
	if g.cur != nil {
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: postBB})
	}

	g.cur = postBB
	if fn.Post != nil {
		g.lowerStmt(fn.Post, nil)
	}
	if g.cur != nil {
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: condBB})
	}

	g.breakStack = g.breakStack[:len(g.breakStack)-1]
	g.continueStack = g.continueStack[:len(g.continueStack)-1]
	g.cur = afterBB
}

func (g *funcCFG) lowerRange(rn *ir.RangeStmt) {
	pos := rn.Pos()
	xt := rn.X.Type()
	condBB := g.newBlock()
	bodyBB := g.newBlock()
	postBB := g.newBlock()
	afterBB := g.newBlock()

	g.breakStack = append(g.breakStack, afterBB)
	g.continueStack = append(g.continueStack, postBB)
	if rn.Label != nil {
		g.labelBreak[rn.Label] = afterBB
		g.labelContinue[rn.Label] = postBB
	}
	defer func() {
		g.breakStack = g.breakStack[:len(g.breakStack)-1]
		g.continueStack = g.continueStack[:len(g.continueStack)-1]
	}()

	xExpr := g.flattenExpr(rn.X)

	switch {
	case xt.IsChan():
		chTmp := g.newTemp(pos, xt, false)
		g.emitAssign(pos, chTmp, xExpr)
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: condBB})

		g.cur = condBB
		okTmp := g.newTemp(pos, types.Types[types.TBOOL], false)
		var valDst ir.Node
		if rn.Key != nil && !ir.IsBlank(rn.Key) {
			valDst = rn.Key
		}
		g.emitChanRecv(pos, chTmp, valDst, okTmp)
		g.terminate(blockTerm{kind: termBranch, pos: pos, cond: okTmp, thenBB: bodyBB, elseBB: afterBB})

		g.cur = bodyBB
		g.lowerStmts(rn.Body)
		if g.cur != nil {
			g.terminate(blockTerm{kind: termGoto, pos: pos, target: postBB})
		}
		g.cur = postBB
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: condBB})

	case xt.IsSlice() || xt.IsArray() || (xt.IsPtr() && xt.Elem().IsArray()):
		seqTmp := g.newTemp(pos, xt, false)
		g.emitAssign(pos, seqTmp, xExpr)
		idxTmp := g.newTemp(pos, types.Types[types.TINT], false)
		g.emitAssign(pos, idxTmp, ir.NewInt(pos, 0))
		lenTmp := g.newTemp(pos, types.Types[types.TINT], false)
		lenExpr := typecheck.Expr(ir.NewUnaryExpr(pos, ir.OLEN, seqTmp))
		g.emitAssign(pos, lenTmp, lenExpr)
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: condBB})

		g.cur = condBB
		cond := typecheck.Expr(ir.NewBinaryExpr(pos, ir.OLT, idxTmp, lenTmp))
		g.terminate(blockTerm{kind: termBranch, pos: pos, cond: cond, thenBB: bodyBB, elseBB: afterBB})

		g.cur = bodyBB
		if rn.Key != nil && !ir.IsBlank(rn.Key) {
			g.emitAssign(pos, rn.Key, idxTmp)
		}
		if rn.Value != nil && !ir.IsBlank(rn.Value) {
			elem := typecheck.Expr(ir.NewIndexExpr(pos, seqTmp, idxTmp))
			g.emitAssign(pos, rn.Value, elem)
		}
		g.lowerStmts(rn.Body)
		if g.cur != nil {
			g.terminate(blockTerm{kind: termGoto, pos: pos, target: postBB})
		}

		g.cur = postBB
		inc := typecheck.Expr(ir.NewBinaryExpr(pos, ir.OADD, idxTmp, ir.NewInt(pos, 1)))
		g.emitAssign(pos, idxTmp, inc)
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: condBB})

	case xt.IsInteger():
		limTmp := g.newTemp(pos, xt, false)
		g.emitAssign(pos, limTmp, xExpr)
		idxTmp := g.newTemp(pos, xt, false)
		g.emitAssign(pos, idxTmp, typecheck.Conv(ir.NewInt(pos, 0), xt))
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: condBB})

		g.cur = condBB
		cond := typecheck.Expr(ir.NewBinaryExpr(pos, ir.OLT, idxTmp, limTmp))
		g.terminate(blockTerm{kind: termBranch, pos: pos, cond: cond, thenBB: bodyBB, elseBB: afterBB})

		g.cur = bodyBB
		if rn.Key != nil && !ir.IsBlank(rn.Key) {
			g.emitAssign(pos, rn.Key, idxTmp)
		}
		g.lowerStmts(rn.Body)
		if g.cur != nil {
			g.terminate(blockTerm{kind: termGoto, pos: pos, target: postBB})
		}

		g.cur = postBB
		inc := typecheck.Expr(ir.NewBinaryExpr(pos, ir.OADD, idxTmp, typecheck.Conv(ir.NewInt(pos, 1), xt)))
		g.emitAssign(pos, idxTmp, inc)
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: condBB})

	case xt.IsString():
		strTmp := g.newTemp(pos, xt, false)
		g.emitAssign(pos, strTmp, xExpr)
		idxTmp := g.newTemp(pos, types.Types[types.TINT], false)
		g.emitAssign(pos, idxTmp, ir.NewInt(pos, 0))
		nextIdx := g.newTemp(pos, types.Types[types.TINT], false)
		lenTmp := g.newTemp(pos, types.Types[types.TINT], false)
		g.emitAssign(pos, lenTmp, typecheck.Expr(ir.NewUnaryExpr(pos, ir.OLEN, strTmp)))
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: condBB})

		g.cur = condBB
		cond := typecheck.Expr(ir.NewBinaryExpr(pos, ir.OLT, idxTmp, lenTmp))
		g.terminate(blockTerm{kind: termBranch, pos: pos, cond: cond, thenBB: bodyBB, elseBB: afterBB})

		g.cur = bodyBB
		runeTmp := g.newTemp(pos, types.RuneType, false)
		byteTmp := g.newTemp(pos, types.Types[types.TUINT8], false)
		g.emitAssign(pos, byteTmp, typecheck.Expr(ir.NewIndexExpr(pos, strTmp, idxTmp)))
		asciiBB := g.newBlock()
		utf8BB := g.newBlock()
		runBodyBB := g.newBlock()
		isASCII := typecheck.Expr(ir.NewBinaryExpr(pos, ir.OLT, byteTmp, typecheck.Conv(ir.NewInt(pos, 0x80), types.Types[types.TUINT8])))
		g.terminate(blockTerm{kind: termBranch, pos: pos, cond: isASCII, thenBB: asciiBB, elseBB: utf8BB})

		g.cur = asciiBB
		g.emitAssign(pos, runeTmp, typecheck.Conv(byteTmp, types.RuneType))
		g.emitAssign(pos, nextIdx, typecheck.Expr(ir.NewBinaryExpr(pos, ir.OADD, idxTmp, ir.NewInt(pos, 1))))
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: runBodyBB})

		g.cur = utf8BB
		decCall := typecheck.Call(pos, typecheck.LookupRuntime("decoderune"), []ir.Node{strTmp, idxTmp}, false)
		al := ir.NewAssignListStmt(pos, ir.OAS2FUNC, []ir.Node{runeTmp, nextIdx}, []ir.Node{decCall})
		al.SetTypecheck(1)
		g.emit(al)
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: runBodyBB})

		g.cur = runBodyBB
		if rn.Key != nil && !ir.IsBlank(rn.Key) {
			g.emitAssign(pos, rn.Key, idxTmp)
		}
		if rn.Value != nil && !ir.IsBlank(rn.Value) {
			g.emitAssign(pos, rn.Value, runeTmp)
		}
		g.lowerStmts(rn.Body)
		if g.cur != nil {
			g.terminate(blockTerm{kind: termGoto, pos: pos, target: postBB})
		}

		g.cur = postBB
		g.emitAssign(pos, idxTmp, nextIdx)
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: condBB})

	case xt.IsMap():
		mapTmp := g.newTemp(pos, xt, false)
		g.emitAssign(pos, mapTmp, xExpr)
		hiterT := reflectdata.MapIterType()
		itTmp := g.newTemp(pos, hiterT, true)
		startFn := typecheck.LookupRuntime("mapIterStart", xt.Key(), xt.Elem(), hiterT)
		startCall := typecheck.Call(pos, startFn, []ir.Node{
			reflectdata.TypePtrAt(pos, xt),
			mapTmp,
			typecheck.NodAddrAt(pos, itTmp),
		}, false)
		g.emit(startCall)
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: condBB})

		g.cur = condBB
		keyField := typecheck.Expr(ir.NewSelectorExpr(pos, ir.OXDOT, itTmp, hiterT.Field(0).Sym))
		nilPtr := ir.NewNilExpr(pos, keyField.Type())
		cond := typecheck.Expr(ir.NewBinaryExpr(pos, ir.ONE, keyField, nilPtr))
		g.terminate(blockTerm{kind: termBranch, pos: pos, cond: cond, thenBB: bodyBB, elseBB: afterBB})

		g.cur = bodyBB
		if rn.Key != nil && !ir.IsBlank(rn.Key) {
			kptr := typecheck.ConvNop(keyField, types.NewPtr(xt.Key()))
			g.emitAssign(pos, rn.Key, typecheck.Expr(ir.NewStarExpr(pos, kptr)))
		}
		if rn.Value != nil && !ir.IsBlank(rn.Value) {
			elemField := typecheck.Expr(ir.NewSelectorExpr(pos, ir.OXDOT, itTmp, hiterT.Field(1).Sym))
			vptr := typecheck.ConvNop(elemField, types.NewPtr(xt.Elem()))
			g.emitAssign(pos, rn.Value, typecheck.Expr(ir.NewStarExpr(pos, vptr)))
		}
		g.lowerStmts(rn.Body)
		if g.cur != nil {
			g.terminate(blockTerm{kind: termGoto, pos: pos, target: postBB})
		}

		g.cur = postBB
		nextFn := typecheck.LookupRuntime("mapIterNext", hiterT)
		g.emit(typecheck.Call(pos, nextFn, []ir.Node{typecheck.NodAddrAt(pos, itTmp)}, false))
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: condBB})

	default:
		base.FatalfAt(pos, "coro: unsupported range type %v", xt)
	}
	g.cur = afterBB
}

func (g *funcCFG) lowerSwitch(sw *ir.SwitchStmt) {
	pos := sw.Pos()
	afterBB := g.newBlock()
	g.breakStack = append(g.breakStack, afterBB)
	if sw.Label != nil {
		g.labelBreak[sw.Label] = afterBB
	}
	defer func() {
		g.breakStack = g.breakStack[:len(g.breakStack)-1]
	}()

	isTypeSw := sw.Tag != nil && sw.Tag.Op() == ir.OTYPESW
	var tagTmp *ir.Name
	if isTypeSw {
		tsw := sw.Tag.(*ir.TypeSwitchGuard)
		fe := g.flattenExpr(tsw.X)
		tagTmp = g.newTemp(pos, fe.Type(), false)
		g.emitAssign(pos, tagTmp, fe)
	} else if sw.Tag != nil {
		fe := g.flattenExpr(sw.Tag)
		tagTmp = g.newTemp(pos, fe.Type(), false)
		g.emitAssign(pos, tagTmp, fe)
	}

	caseBBs := make([]*basicBlock, len(sw.Cases))
	defaultIdx := -1
	for i, cc := range sw.Cases {
		caseBBs[i] = g.newBlock()
		if len(cc.List) == 0 {
			defaultIdx = i
		}
	}

	// Emit case matching chain in source order; default is tried last if present.
	for i, cc := range sw.Cases {
		if i == defaultIdx {
			continue
		}
		nextCheck := g.newBlock()
		for j, item := range cc.List {
			var matchExpr ir.Node
			if isTypeSw {
				if item.Op() == ir.ONIL {
					matchExpr = typecheck.Expr(ir.NewBinaryExpr(cc.Pos(), ir.OEQ, tagTmp, ir.NewNilExpr(cc.Pos(), tagTmp.Type())))
				} else {
					okTmp := g.newTemp(cc.Pos(), types.Types[types.TBOOL], false)
					valTmp := g.newTemp(cc.Pos(), item.Type(), false)
					dt := ir.NewTypeAssertExpr(cc.Pos(), tagTmp, item.Type())
					dt.SetOp(ir.ODOTTYPE2)
					dt.SetTypecheck(1)
					al := ir.NewAssignListStmt(cc.Pos(), ir.OAS2DOTTYPE, []ir.Node{valTmp, okTmp}, []ir.Node{dt})
					al.SetTypecheck(1)
					g.emit(al)
					if len(cc.List) == 1 && cc.Var != nil && !ir.IsBlank(cc.Var) {
						// Bind case variable when single-type case matches.
						bindBB := g.newBlock()
						altBB := nextCheck
						if j+1 < len(cc.List) {
							altBB = g.newBlock()
						}
						g.terminate(blockTerm{kind: termBranch, pos: cc.Pos(), cond: okTmp, thenBB: bindBB, elseBB: altBB})
						g.cur = bindBB
						g.emitAssign(cc.Pos(), cc.Var, valTmp)
						g.terminate(blockTerm{kind: termGoto, pos: cc.Pos(), target: caseBBs[i]})
						g.cur = altBB
						continue
					}
					matchExpr = okTmp
				}
			} else {
				rhs := g.flattenExpr(item)
				if tagTmp != nil {
					matchExpr = typecheck.Expr(ir.NewBinaryExpr(cc.Pos(), ir.OEQ, tagTmp, rhs))
				} else {
					matchExpr = rhs
				}
			}
			altBB := nextCheck
			if j+1 < len(cc.List) {
				altBB = g.newBlock()
			}
			g.terminate(blockTerm{kind: termBranch, pos: cc.Pos(), cond: matchExpr, thenBB: caseBBs[i], elseBB: altBB})
			g.cur = altBB
		}
		if g.cur != nextCheck {
			if g.cur != nil {
				g.terminate(blockTerm{kind: termGoto, pos: cc.Pos(), target: nextCheck})
			}
			g.cur = nextCheck
		}
	}

	if defaultIdx >= 0 {
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: caseBBs[defaultIdx]})
	} else {
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: afterBB})
	}

	// Emit each case body (handling fallthrough and multi-type/default type-switch var binding).
	for i, cc := range sw.Cases {
		g.cur = caseBBs[i]
		if isTypeSw && cc.Var != nil && !ir.IsBlank(cc.Var) && (len(cc.List) != 1 || cc.List[0].Op() == ir.ONIL) {
			g.emitAssign(cc.Pos(), cc.Var, tagTmp)
		}
		hasFall := false
		body := cc.Body
		if len(body) > 0 && body[len(body)-1].Op() == ir.OFALL {
			hasFall = true
			body = body[:len(body)-1]
		}
		g.lowerStmts(body)
		if g.cur != nil {
			if hasFall && i+1 < len(sw.Cases) {
				g.terminate(blockTerm{kind: termGoto, pos: cc.Pos(), target: caseBBs[i+1]})
			} else {
				g.terminate(blockTerm{kind: termGoto, pos: cc.Pos(), target: afterBB})
			}
		}
	}
	g.cur = afterBB
}

func (g *funcCFG) lowerSelect(sel *ir.SelectStmt) {
	pos := sel.Pos()
	afterBB := g.newBlock()
	g.breakStack = append(g.breakStack, afterBB)
	if sel.Label != nil {
		g.labelBreak[sel.Label] = afterBB
	}
	defer func() {
		g.breakStack = g.breakStack[:len(g.breakStack)-1]
	}()

	// Evaluate all channel expressions & send values in source order per Go spec,
	// partitioning into sends first, then recvs (matching runtime.coroSelect ABI).
	var sends, recvs []selCaseCFG
	var defaultCC *ir.CommClause
	var defaultBB *basicBlock
	caseClauses := make([]*ir.CommClause, 0, len(sel.Cases))
	var sendClauses, recvClauses []*ir.CommClause

	for _, cc := range sel.Cases {
		if cc.Comm == nil {
			defaultCC = cc
			defaultBB = g.newBlock()
			continue
		}
		g.lowerInit(cc.Comm)
		switch comm := cc.Comm.(type) {
		case *ir.SendStmt:
			ch := g.flattenExpr(comm.Chan)
			val := g.flattenExpr(comm.Value)
			chTmp := g.newTemp(comm.Pos(), ch.Type(), false)
			g.emitAssign(comm.Pos(), chTmp, ch)
			elemTmp := g.newTemp(comm.Pos(), ch.Type().Elem(), true)
			g.emitAssign(comm.Pos(), elemTmp, val)
			sends = append(sends, selCaseCFG{
				isSend:  true,
				ch:      chTmp,
				val:     elemTmp,
				elemTmp: elemTmp,
				bodyBB:  g.newBlock(),
			})
			sendClauses = append(sendClauses, cc)
		case *ir.UnaryExpr: // <-ch
			ch := g.flattenExpr(comm.X)
			chTmp := g.newTemp(comm.Pos(), ch.Type(), false)
			g.emitAssign(comm.Pos(), chTmp, ch)
			recvs = append(recvs, selCaseCFG{
				isSend: false,
				ch:     chTmp,
				bodyBB: g.newBlock(),
			})
			recvClauses = append(recvClauses, cc)
		case *ir.AssignStmt: // v := <-ch
			ch := g.flattenExpr(comm.Y.(*ir.UnaryExpr).X)
			chTmp := g.newTemp(comm.Pos(), ch.Type(), false)
			g.emitAssign(comm.Pos(), chTmp, ch)
			var dst ir.Node
			var elemTmp *ir.Name
			if !ir.IsBlank(comm.X) {
				dst = comm.X
				elemTmp = g.newTemp(comm.Pos(), ch.Type().Elem(), true)
			}
			recvs = append(recvs, selCaseCFG{
				isSend:  false,
				ch:      chTmp,
				dst:     dst,
				elemTmp: elemTmp,
				bodyBB:  g.newBlock(),
			})
			recvClauses = append(recvClauses, cc)
		case *ir.AssignListStmt: // v, ok := <-ch
			ch := g.flattenExpr(comm.Rhs[0].(*ir.UnaryExpr).X)
			chTmp := g.newTemp(comm.Pos(), ch.Type(), false)
			g.emitAssign(comm.Pos(), chTmp, ch)
			var dst, okDst ir.Node
			var elemTmp *ir.Name
			if len(comm.Lhs) > 0 && !ir.IsBlank(comm.Lhs[0]) {
				dst = comm.Lhs[0]
				elemTmp = g.newTemp(comm.Pos(), ch.Type().Elem(), true)
			}
			if len(comm.Lhs) > 1 && !ir.IsBlank(comm.Lhs[1]) {
				okDst = comm.Lhs[1]
			}
			recvs = append(recvs, selCaseCFG{
				isSend:  false,
				ch:      chTmp,
				dst:     dst,
				okDst:   okDst,
				elemTmp: elemTmp,
				bodyBB:  g.newBlock(),
			})
			recvClauses = append(recvClauses, cc)
		default:
			base.FatalfAt(cc.Pos(), "coro: unexpected select comm %v", cc.Comm.Op())
		}
	}

	allCases := append(sends, recvs...)
	caseClauses = append(sendClauses, recvClauses...)
	if len(allCases) > g.maxSelectCases {
		g.maxSelectCases = len(allCases)
	}
	if len(allCases) == 0 && g.maxSelectCases == 0 {
		// Ensure selChosen/selOK exist even for `select {}`.
		g.maxSelectCases = 1
	}

	resumeBB := g.newResumeBlock()
	g.terminate(blockTerm{
		kind:       termSelect,
		pos:        pos,
		target:     resumeBB,
		selCases:   allCases,
		hasDefault: defaultCC != nil,
		defaultBB:  defaultBB,
	})

	// Emit each case body.
	for i, c := range allCases {
		g.cur = c.bodyBB
		g.lowerStmts(caseClauses[i].Body)
		if g.cur != nil {
			g.terminate(blockTerm{kind: termGoto, pos: caseClauses[i].Pos(), target: afterBB})
		}
	}
	if defaultCC != nil {
		g.cur = defaultBB
		g.lowerStmts(defaultCC.Body)
		if g.cur != nil {
			g.terminate(blockTerm{kind: termGoto, pos: defaultCC.Pos(), target: afterBB})
		}
	}
	g.cur = afterBB
}

func (g *funcCFG) flattenLValue(n ir.Node) ir.Node {
	if n == nil || !g.exprHasSuspend(n) {
		return n
	}
	switch x := n.(type) {
	case *ir.IndexExpr:
		x.X = g.flattenExpr(x.X)
		x.Index = g.flattenExpr(x.Index)
		return x
	case *ir.StarExpr:
		x.X = g.flattenExpr(x.X)
		return x
	case *ir.SelectorExpr:
		x.X = g.flattenExpr(x.X)
		return x
	}
	return n
}

func (g *funcCFG) flattenCallArgs(call *ir.CallExpr) *ir.CallExpr {
	g.lowerInit(call)
	if sel, ok := call.Fun.(*ir.SelectorExpr); ok && g.exprHasSuspend(sel.X) {
		sel.X = g.flattenExpr(sel.X)
	} else if g.exprHasSuspend(call.Fun) {
		call.Fun = g.flattenExpr(call.Fun)
	}
	for i, a := range call.Args {
		if g.exprHasSuspend(a) {
			call.Args[i] = g.flattenExpr(a)
		}
	}
	return call
}

func (g *funcCFG) flattenExpr(n ir.Node) ir.Node {
	if n == nil {
		return nil
	}
	g.lowerInit(n)
	if !g.exprHasSuspend(n) {
		return n
	}
	pos := n.Pos()
	switch x := n.(type) {
	case *ir.UnaryExpr:
		if x.Op() == ir.ORECV {
			ch := g.flattenExpr(x.X)
			dst := g.newTemp(pos, x.Type(), false)
			g.emitChanRecv(pos, ch, dst, nil)
			return dst
		}
		x.X = g.flattenExpr(x.X)
		return x

	case *ir.LogicalExpr:
		// Preserve exact short-circuit semantics across suspension!
		res := g.newTemp(pos, x.Type(), false)
		lhs := g.flattenExpr(x.X)
		g.emitAssign(pos, res, lhs)
		rhsBB := g.newBlock()
		joinBB := g.newBlock()
		if x.Op() == ir.OANDAND {
			g.terminate(blockTerm{kind: termBranch, pos: pos, cond: res, thenBB: rhsBB, elseBB: joinBB})
		} else {
			g.terminate(blockTerm{kind: termBranch, pos: pos, cond: res, thenBB: joinBB, elseBB: rhsBB})
		}
		g.cur = rhsBB
		rhs := g.flattenExpr(x.Y)
		g.emitAssign(pos, res, rhs)
		g.terminate(blockTerm{kind: termGoto, pos: pos, target: joinBB})
		g.cur = joinBB
		return res

	case *ir.BinaryExpr:
		lx := g.flattenExpr(x.X)
		if g.exprHasSuspend(x.Y) {
			lt := g.newTemp(pos, lx.Type(), false)
			g.emitAssign(pos, lt, lx)
			lx = lt
		}
		x.X = lx
		x.Y = g.flattenExpr(x.Y)
		return x

	case *ir.ConvExpr:
		x.X = g.flattenExpr(x.X)
		return x

	case *ir.SelectorExpr:
		x.X = g.flattenExpr(x.X)
		return x

	case *ir.IndexExpr:
		lx := g.flattenExpr(x.X)
		if g.exprHasSuspend(x.Index) {
			lt := g.newTemp(pos, lx.Type(), false)
			g.emitAssign(pos, lt, lx)
			lx = lt
		}
		x.X = lx
		x.Index = g.flattenExpr(x.Index)
		return x

	case *ir.SliceExpr:
		x.X = g.flattenExpr(x.X)
		x.Low = g.flattenExpr(x.Low)
		x.High = g.flattenExpr(x.High)
		x.Max = g.flattenExpr(x.Max)
		return x

	case *ir.CompLitExpr:
		for i, e := range x.List {
			if kv, ok := e.(*ir.KeyExpr); ok {
				kv.Key = g.flattenExpr(kv.Key)
				kv.Value = g.flattenExpr(kv.Value)
			} else if sv, ok := e.(*ir.StructKeyExpr); ok {
				sv.Value = g.flattenExpr(sv.Value)
			} else {
				x.List[i] = g.flattenExpr(e)
			}
		}
		return x

	case *ir.CallExpr:
		if matchStdlibIO(x) != ioNone || g.isLoweredCallee(x) != nil {
			resTmp := g.newTemp(pos, x.Type(), false)
			g.lowerMultiCall(pos, x, []ir.Node{resTmp})
			return resTmp
		}
		return g.flattenCallArgs(x)

	case *ir.ParenExpr:
		return g.flattenExpr(x.X)

	case *ir.AddrExpr:
		x.X = g.flattenLValue(x.X)
		return x

	case *ir.StarExpr:
		x.X = g.flattenExpr(x.X)
		return x

	case *ir.TypeAssertExpr:
		x.X = g.flattenExpr(x.X)
		return x
	}
	return n
}
