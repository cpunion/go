// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"fmt"
	"slices"
	"strings"
)

// coroHeaderFields returns the leading header fields matching runtime.coroTask:
//   _ [7]unsafe.Pointer
//   _ [6]uint32
// Exact size: 80 B on 64-bit, 52 B on 32-bit, zero trailing padding, 0x7f GC bitmap.
func coroHeaderFields(pos src.XPos) []*types.Field {
	ptrArr := types.NewArray(types.Types[types.TUNSAFEPTR], 7)
	u32Arr := types.NewArray(types.Types[types.TUINT32], 6)
	return []*types.Field{
		types.NewField(pos, types.LocalPkg.Lookup("_hdrPtr"), ptrArr),
		types.NewField(pos, types.LocalPkg.Lookup("_hdrU32"), u32Arr),
	}
}

// buildFrame performs backward liveness analysis on cfg and constructs the
// combined struct{ coroTask; state uint32; params; captures; results; locals; scratch }
// Go struct type for info.
func buildFrame(info *FuncInfo, g *funcCFG) {
	fn := info.Orig
	pos := fn.Pos()

	// 1. Collect all local variables (PAUTO) and compute per-block use/def sets.
	var locals []*ir.Name
	localIdx := make(map[*ir.Name]int)
	addLocal := func(nm *ir.Name) int {
		if nm == nil {
			return -1
		}
		c := nm.Canonical()
		if c.Class != ir.PAUTO || c.Curfn != fn {
			return -1
		}
		if idx, ok := localIdx[c]; ok {
			return idx
		}
		idx := len(locals)
		locals = append(locals, c)
		localIdx[c] = idx
		return idx
	}

	// Register all locals mentioned anywhere in the CFG (including CFG temps).
	for _, nm := range g.temps {
		addLocal(nm)
	}
	for _, nm := range fn.Dcl {
		addLocal(nm)
	}

	nBlocks := len(g.blocks)
	nVars := len(locals)
	words := (nVars + 63) / 64

	use := make([][]uint64, nBlocks)
	def := make([][]uint64, nBlocks)
	liveIn := make([][]uint64, nBlocks)
	liveOut := make([][]uint64, nBlocks)
	for i := range nBlocks {
		use[i] = make([]uint64, words)
		def[i] = make([]uint64, words)
		liveIn[i] = make([]uint64, words)
		liveOut[i] = make([]uint64, words)
	}
	setBit := func(bs []uint64, idx int) { bs[idx/64] |= 1 << (uint(idx) % 64) }
	hasBit := func(bs []uint64, idx int) bool { return bs[idx/64]&(1<<(uint(idx)%64)) != 0 }

	markRead := func(bi int, n ir.Node) {
		if n == nil {
			return
		}
		ir.Visit(n, func(x ir.Node) {
			if nm, ok := x.(*ir.Name); ok {
				if idx := addLocal(nm); idx >= 0 && idx < nVars {
					if !hasBit(def[bi], idx) {
						setBit(use[bi], idx)
					}
				}
			} else if clo, ok := x.(*ir.ClosureExpr); ok && clo.Func != nil {
				for _, cv := range clo.Func.ClosureVars {
					if cv.Outer != nil {
						if idx := addLocal(cv.Outer); idx >= 0 && idx < nVars {
							if !hasBit(def[bi], idx) {
								setBit(use[bi], idx)
							}
						}
					}
				}
			}
		})
	}
	markWrite := func(bi int, n ir.Node) {
		if n == nil {
			return
		}
		if nm, ok := n.(*ir.Name); ok {
			if idx := addLocal(nm); idx >= 0 && idx < nVars {
				setBit(def[bi], idx)
			}
			return
		}
		// Assigning through pointer/index/field reads the base expression.
		markRead(bi, n)
	}

	// Record unconditional resume-epilogue writes at the start of their target
	// resume blocks (BEFORE scanning target's own statements).
	for _, bb := range g.blocks {
		t := &bb.term
		if t.target == nil {
			continue
		}
		ti := t.target.id
		switch t.kind {
		case termChanRecv:
			if t.dst != nil {
				markWrite(ti, t.dst)
			}
			if t.okDst != nil {
				markWrite(ti, t.okDst)
			}
		case termIO:
			if t.ioNDst != nil {
				markWrite(ti, t.ioNDst)
			}
			if t.ioErrDst != nil {
				markWrite(ti, t.ioErrDst)
			}
		case termAwait:
			for _, d := range t.awaitDsts {
				if d != nil {
					markWrite(ti, d)
				}
			}
		}
	}

	for bi, bb := range g.blocks {
		for _, stmt := range bb.stmts {
			recordStmtUseDef(bi, stmt, markRead, markWrite)
		}
		recordTermUseDef(bi, &bb.term, markRead, markWrite)
	}

	// Standard backward dataflow fixed-point:
	//   liveOut[b] = union_{s in succs(b)} liveIn[s]
	//   liveIn[b]  = use[b] U (liveOut[b] \ def[b])
	for changed := true; changed; {
		changed = false
		for bi := nBlocks - 1; bi >= 0; bi-- {
			bb := g.blocks[bi]
			for _, succ := range bb.succs {
				si := succ.id
				for w := range words {
					old := liveOut[bi][w]
					nv := old | liveIn[si][w]
					if nv != old {
						liveOut[bi][w] = nv
						changed = true
					}
				}
			}
			for w := range words {
				old := liveIn[bi][w]
				nv := use[bi][w] | (liveOut[bi][w] &^ def[bi][w])
				if nv != old {
					liveIn[bi][w] = nv
					changed = true
				}
			}
		}
	}

	// A local must be stored in the heap frame if:
	//   - it is live at entry of any resume target block (crosses a suspension), OR
	//   - it is address-taken (may be referenced across suspension via pointer/slice/defer), OR
	//   - it is a CFG terminator output/scratch slot (written on resume or passed by address to runtime).
	needFrame := make([]bool, nVars)
	for _, bb := range g.blocks {
		if bb.resumeState > 0 {
			for i := range nVars {
				if hasBit(liveIn[bb.id], i) {
					needFrame[i] = true
				}
			}
		}
	}
	for i, nm := range locals {
		if g.pinnedLocals[nm] {
			needFrame[i] = true
		}
	}

	// 2. Assemble fields of the combined frame struct.
	fields := coroHeaderFields(pos)
	stateField := types.NewField(pos, types.LocalPkg.Lookup("_state"), types.Types[types.TUINT32])
	fields = append(fields, stateField)
	info.StateField = stateField
	info.VarFields = make(map[*ir.Name]*types.Field)

	fieldSeq := 0
	addField := func(prefix string, t *types.Type) *types.Field {
		sym := types.LocalPkg.Lookup(fmt.Sprintf("_%s%d", prefix, fieldSeq))
		fieldSeq++
		f := types.NewField(pos, sym, t)
		fields = append(fields, f)
		return f
	}

	// Captured closure variables (first, matching FactoryFn parameter order).
	for i, cv := range fn.ClosureVars {
		ft := cv.Type()
		if i < len(info.CapByRef) && info.CapByRef[i] {
			ft = types.NewPtr(ft)
		}
		f := addField("cap", ft)
		info.CapFields = append(info.CapFields, f)
		info.VarFields[cv.Canonical()] = f
	}

	// Receiver + regular parameters.
	sig := fn.Type()
	if recv := sig.Recv(); recv != nil {
		var rname *ir.Name
		if recv.Nname != nil {
			rname = recv.Nname.(*ir.Name).Canonical()
		}
		f := addField("recv", recv.Type)
		info.ParamFields = append(info.ParamFields, f)
		if rname != nil {
			info.VarFields[rname] = f
		}
	}
	for _, p := range sig.Params() {
		var pname *ir.Name
		if p.Nname != nil {
			pname = p.Nname.(*ir.Name).Canonical()
		}
		f := addField("arg", p.Type)
		info.ParamFields = append(info.ParamFields, f)
		if pname != nil {
			info.VarFields[pname] = f
		}
	}

	// Return values (always in frame so caller/parent reads them before coroReleaseFrame).
	for _, r := range sig.Results() {
		var rname *ir.Name
		if r.Nname != nil {
			rname = r.Nname.(*ir.Name).Canonical()
		}
		f := addField("res", r.Type)
		info.ResFields = append(info.ResFields, f)
		if rname != nil {
			info.VarFields[rname] = f
		}
	}

	// Frame-resident locals (sorted deterministically by name for reproducible builds).
	var chosen []*ir.Name
	for i, nm := range locals {
		if needFrame[i] {
			chosen = append(chosen, nm)
		}
	}
	slices.SortFunc(chosen, func(a, b *ir.Name) int {
		if c := strings.Compare(a.Sym().Name, b.Sym().Name); c != 0 {
			return c
		}
		return int(a.Pos().Line()) - int(b.Pos().Line())
	})
	for _, nm := range chosen {
		f := addField("loc", nm.Type())
		info.VarFields[nm] = f
	}

	// Scratch fields required by terminators (awaitChild, io, select, chan ok).
	if g.needAwaitChild {
		g.awaitChildField = addField("child", types.Types[types.TUNSAFEPTR])
	}
	if g.needIO {
		g.ioNField = addField("ioN", types.Types[types.TINT])
		g.ioErrField = addField("ioErr", types.ErrorType)
		g.ioDirectField = addField("ioDir", types.Types[types.TBOOL])
		g.ioStatusField = addField("ioSt", types.Types[types.TUINTPTR])
	}
	if g.needChanOK {
		g.chanOKField = addField("chOK", types.Types[types.TBOOL])
	}
	if g.maxSelectCases > 0 {
		// runtime.scase is struct { c unsafe.Pointer; elem unsafe.Pointer }.
		scaseType := types.NewStruct([]*types.Field{
			types.NewField(pos, types.LocalPkg.Lookup("c"), types.Types[types.TUNSAFEPTR]),
			types.NewField(pos, types.LocalPkg.Lookup("elem"), types.Types[types.TUNSAFEPTR]),
		})
		types.CalcSize(scaseType)
		g.selCasesField = addField("selC", types.NewArray(scaseType, int64(g.maxSelectCases)))
		g.selChosenField = addField("selI", types.Types[types.TINT])
		g.selOKField = addField("selOK", types.Types[types.TBOOL])
	}

	frameType := types.NewStruct(fields)
	types.CalcSize(frameType)
	info.FrameType = frameType
}

func recordStmtUseDef(bi int, n ir.Node, markRead, markWrite func(int, ir.Node)) {
	if n == nil {
		return
	}
	// Visit any Init statements attached to n first.
	for _, init := range n.Init() {
		recordStmtUseDef(bi, init, markRead, markWrite)
	}
	switch n.Op() {
	case ir.ODCL:
		markWrite(bi, n.(*ir.Decl).X)
	case ir.OAS:
		as := n.(*ir.AssignStmt)
		markRead(bi, as.Y)
		markWrite(bi, as.X)
	case ir.OASOP:
		as := n.(*ir.AssignOpStmt)
		markRead(bi, as.X)
		markRead(bi, as.Y)
		markWrite(bi, as.X)
	case ir.OAS2, ir.OAS2DOTTYPE, ir.OAS2FUNC, ir.OAS2MAPR, ir.OAS2RECV:
		al := n.(*ir.AssignListStmt)
		for _, r := range al.Rhs {
			markRead(bi, r)
		}
		for _, l := range al.Lhs {
			markWrite(bi, l)
		}
	case ir.OBLOCK:
		for _, s := range n.(*ir.BlockStmt).List {
			recordStmtUseDef(bi, s, markRead, markWrite)
		}
	default:
		markRead(bi, n)
	}
}

func recordTermUseDef(bi int, t *blockTerm, markRead, markWrite func(int, ir.Node)) {
	switch t.kind {
	case termBranch:
		markRead(bi, t.cond)
	case termSleep:
		markRead(bi, t.arg)
	case termChanSend:
		markRead(bi, t.ch)
		markRead(bi, t.val)
	case termChanRecv:
		markRead(bi, t.ch)
	case termSelect:
		for _, c := range t.selCases {
			markRead(bi, c.ch)
			markRead(bi, c.val)
		}
	case termIO:
		markRead(bi, t.ioRecv)
		markRead(bi, t.ioBuf)
	case termAwait:
		for _, a := range t.awaitArgs {
			markRead(bi, a)
		}
	case termPanic:
		markRead(bi, t.arg)
	}
}
