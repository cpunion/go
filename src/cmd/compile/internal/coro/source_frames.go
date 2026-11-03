// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/src"
	"internal/abi"
	"strings"
)

// Source identities come from an explicit body-copy relationship. In
// particular private helper suffixes and source positions are not identities.
var sourceFrameCopies = make(map[*ir.Func]*obj.LSym)
var nativeSourceFrames = make(map[*ir.Func]*SourceFrame)

type SourceFrame struct {
	Source    *obj.LSym
	Flags     uint32
	PC        *ir.Name
	Field     *types.Field
	Positions []src.XPos
}

func sourceFrameSymbol(fn *ir.Func) *obj.LSym {
	if source := sourceFrameCopies[fn]; source != nil {
		return source
	}
	return fn.Linksym()
}

// RecordSourceCopy is called before subsequent transformations of a fresh
// serialized body. Its nested closure names have the same serialized suffixes
// beneath the new owner; map those exact copies back to the original owner.
func RecordSourceCopy(copy, source *ir.Func) {
	if copy == nil || source == nil {
		return
	}
	origin := sourceFrameSymbol(source)
	sourceFrameCopies[copy] = origin
	prefix := copy.Sym().Name
	var visit func(*ir.Func)
	visit = func(fn *ir.Func) {
		ir.Visit(fn, func(node ir.Node) {
			closure, ok := node.(*ir.ClosureExpr)
			if !ok {
				return
			}
			child := closure.Func
			if strings.HasPrefix(child.Sym().Name, prefix+".") {
				name := origin.Name + strings.TrimPrefix(child.Sym().Name, prefix)
				sourceFrameCopies[child] = base.Ctxt.LookupABI(name, obj.ABIInternal)
			}
			visit(child)
		})
	}
	visit(copy)
}

func recordSourceResume(resume, source *ir.Func, pc *ir.Name, positions []src.XPos) {
	nativeSourceFrames[resume] = &SourceFrame{
		Source: sourceFrameSymbol(source), Flags: abi.CoroSourceResume,
		PC: pc, Positions: positions,
	}
}

func recordSourceBridge(fn *ir.Func) {
	nativeSourceFrames[fn] = &SourceFrame{Flags: abi.CoroSourceBridge}
}

// SourceFrameInfo is read only by the parallel backends, after explicit-frame
// layout and ordinary-source restoration have finished.
func SourceFrameInfo(fn *ir.Func) *SourceFrame {
	if frame := nativeSourceFrames[fn]; frame != nil {
		return frame
	}
	if source := sourceFrameCopies[fn]; source != nil {
		return &SourceFrame{Source: source, Flags: abi.CoroSourceCopy}
	}
	return nil
}
