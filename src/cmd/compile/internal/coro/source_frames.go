// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/staticdata"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/src"
	"internal/abi"
	"strings"
)

// Source identities come from an explicit body-copy relationship. In
// particular private helper suffixes and source positions are not identities.
var sourceFrameCopies = make(map[*ir.Func]*obj.LSym)
var sourceFrameNames = make(map[*obj.LSym]*obj.LSym)
var nativeSourceFrames = make(map[*ir.Func]*SourceFrame)

type SourceFrame struct {
	Source    *obj.LSym
	Name      *obj.LSym
	NameSize  uint32
	Flags     uint32
	PC        *ir.Name
	Field     *types.Field
	Positions []SourcePosition
}

type SourcePosition struct {
	File           *obj.LSym
	FileSize, Line uint32
}

func recordSourceName(source *obj.LSym) *obj.LSym {
	name := sourceFrameNames[source]
	if name == nil {
		name = staticdata.StringSym(src.NoXPos, source.Name)
		sourceFrameNames[source] = name
	}
	return name
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
	recordSourceName(origin)
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
				recordSourceName(sourceFrameCopies[child])
			}
			visit(child)
		})
	}
	visit(copy)
}

func recordSourceResume(resume, source *ir.Func, pc *ir.Name, positions []src.XPos) {
	// StringSym can create and publish a new object symbol. Prepare all such
	// references here, while lowering is serial, before parallel SSA emission.
	encoded := make([]SourcePosition, len(positions))
	for i, pos := range positions {
		if !pos.IsKnown() {
			continue
		}
		p := base.Ctxt.InnermostPos(pos)
		filename := p.AbsFilename()
		encoded[i].Line = uint32(p.RelLine())
		if filename != "" {
			encoded[i].File = staticdata.StringSym(pos, filename)
			encoded[i].FileSize = uint32(len(filename))
		}
	}
	origin := sourceFrameSymbol(source)
	nativeSourceFrames[resume] = &SourceFrame{
		Source: origin, Name: recordSourceName(origin), NameSize: uint32(len(origin.Name)), Flags: abi.CoroSourceResume,
		PC: pc, Positions: encoded,
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
		return &SourceFrame{Source: source, Name: sourceFrameNames[source], NameSize: uint32(len(source.Name)), Flags: abi.CoroSourceCopy}
	}
	return nil
}
