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
	"internal/abi"
)

type frameChunkFactory struct {
	factory, resume *ir.Func
	frame, zero     *ir.Name
	length          int64
	locals          map[*ir.Name]ir.Node
	stores          ir.Nodes
}

func newFrameChunkFactory(candidate *lowerCandidate, resume *ir.Func, frame, zero *ir.Name,
	prologue, stores ir.Nodes) *frameChunkFactory {
	locals := chunkFactoryLocals(prologue)
	if locals == nil || !chunkFactoryStoresKnown(candidate.factory, frame, zero, locals, stores) {
		return nil
	}
	return &frameChunkFactory{
		factory: candidate.factory, resume: resume, frame: frame, zero: zero,
		length: candidate.frameChunkLength, locals: locals, stores: stores,
	}
}

// inlineAwait borrows the next typed slot after evaluating every operand.
// The marker proves the active frame owns a compatible chunk. Direct slots
// and separate tasks keep using the ordinary reservation protocol.
func (factory *frameChunkFactory) inlineAwait(proof *fusedChunkProof, frame *ir.Name, markerField *types.Field) {
	resume := proof.resume
	heterogeneous := resume != factory.resume
	first := frameChunkFirstMarker(factory.length, heterogeneous)
	last := first + uint8(factory.length-1)
	ctx := resume.Dcl[0]
	pos := proof.factory.Pos()
	unsafePointer := types.Types[types.TUNSAFEPTR]
	next := typecheck.TempAt(pos, resume, factory.frame.Type())
	markerType := markerField.Type
	marker := typecheck.TempAt(pos, resume, markerType)
	bindings := map[*ir.Name]ir.Node{
		factory.frame: next,
		factory.factory.Type().Param(0).Nname.(*ir.Name): ctx,
	}
	if factory.zero != nil {
		bindings[factory.zero] = ir.NewBool(pos, true)
		if !heterogeneous {
			fresh := ir.NewBinaryExpr(pos, ir.OAND, explicitFrameField(pos, next, markerField),
				typedInt(pos, markerType, int64(abi.CoroFrameFresh)))
			bindings[factory.zero] = typecheck.Expr(ir.NewBinaryExpr(pos, ir.OEQ, fresh,
				typedInt(pos, markerType, 0)))
		}
	}
	for name, value := range factory.locals {
		if value == nil {
			value = typecheck.Expr(ir.NewZero(name.Pos(), name.Type()))
		}
		bindings[name] = value
	}
	var prefix ir.Nodes
	for i, arg := range proof.factory.Args {
		if i == 0 {
			continue
		}
		value := typecheck.TempAt(arg.Pos(), resume, arg.Type())
		prefix = append(prefix, ir.NewDecl(arg.Pos(), ir.ODCL, value),
			ir.NewAssignStmt(arg.Pos(), value, arg))
		bindings[factory.factory.Type().Param(i).Nname.(*ir.Name)] = value
		proof.factory.Args[i] = value
	}
	prefix = append(prefix, ir.NewDecl(pos, ir.ODCL, marker),
		ir.NewAssignStmt(pos, marker, explicitFrameField(pos, frame, markerField)))
	frameType := next.Type().Elem()
	size := typedInt(pos, types.Types[types.TUINTPTR], frameType.Size())
	chunkType := reflectdata.TypePtrAt(pos, types.NewArray(frameType, factory.length))
	addr := typecheck.Expr(ir.NewBinaryExpr(pos, ir.OUNSAFEADD,
		typecheck.ConvNop(frame, unsafePointer), size))
	takeHelper, advanceHelper := "coroTakeSelfChunk", "coroAdvanceSelfChunk"
	takeArgs := ir.Nodes{ctx, size, chunkType}
	if heterogeneous {
		takeHelper, advanceHelper = "coroTakeFusedChunkBoundary", "coroAdvanceFusedChunk"
		// Explicit resumes have no captured variables. A named reference
		// uses the same funcval as their capture-free closure expression.
		takeArgs[0] = factory.resume.Nname
	}
	take := typecheck.Call(pos, typecheck.LookupRuntime(takeHelper), takeArgs, false)
	firstMarker := typedInt(pos, markerType, int64(first))
	atEnd := ir.NewBinaryExpr(pos, ir.OEQ, marker, typedInt(pos, markerType, int64(last)))
	incremented := ir.NewBinaryExpr(pos, ir.OADD, marker, typedInt(pos, markerType, 1))
	initialize := func(slotMarker ir.Node) ir.Nodes {
		args := ir.Nodes{ctx, typecheck.ConvNop(next, unsafePointer), slotMarker}
		if heterogeneous {
			args = append(args, factory.resume.Nname)
		}
		enter := typecheck.Call(pos, typecheck.LookupRuntime(advanceHelper), args, false)
		return append(copyChunkFactoryStores(factory.stores, bindings),
			ir.NewReturnStmt(pos, ir.Nodes{enter}))
	}
	if heterogeneous {
		// Keep the interior path's operands out of the boundary allocation.
		useSlot := func(slot, slotMarker ir.Node) ir.Nodes {
			return append(ir.Nodes{ir.NewAssignStmt(pos, next, typecheck.ConvNop(slot, next.Type()))},
				initialize(slotMarker)...)
		}
		interior := markerInFrameChunk(pos, marker, first, last)
		prefix = append(prefix, ir.NewDecl(pos, ir.ODCL, next),
			ir.NewIfStmt(pos, interior, useSlot(addr, incremented), nil),
			ir.NewIfStmt(pos, atEnd, useSlot(take, firstMarker), nil))
	} else {
		// The local self-frame loop needs one common reload edge.
		nextMarker := typecheck.TempAt(pos, resume, markerType)
		useNext := ir.Nodes{
			ir.NewDecl(pos, ir.ODCL, next), ir.NewDecl(pos, ir.ODCL, nextMarker),
			ir.NewIfStmt(pos, atEnd, ir.Nodes{
				ir.NewAssignStmt(pos, next, typecheck.ConvNop(take, next.Type())),
				ir.NewAssignStmt(pos, nextMarker, firstMarker),
			}, ir.Nodes{
				ir.NewAssignStmt(pos, next, typecheck.ConvNop(addr, next.Type())),
				ir.NewAssignStmt(pos, nextMarker, incremented),
			}),
		}
		useNext = append(useNext, initialize(nextMarker)...)
		inChunk := markerInFrameChunk(pos, marker, first, last+1)
		prefix = append(prefix, ir.NewIfStmt(pos, inChunk, useNext, nil))
	}
	proof.await.List = append(prefix, proof.await.List...)
}

func frameChunkFirstMarker(length int64, heterogeneous bool) uint8 {
	first := abi.CoroFrameChunkValidated | abi.CoroFrameChunkFirst
	if heterogeneous {
		first |= abi.CoroFrameFusedParent | abi.CoroFrameSavedResume
	}
	if length == explicitLargeFrameChunkSize {
		first |= abi.CoroFrameShortChunk
	}
	return first
}

func markerInFrameChunk(pos src.XPos, marker ir.Node, first, limit uint8) ir.Node {
	typ := marker.Type()
	offset := ir.NewBinaryExpr(pos, ir.OSUB, marker, typedInt(pos, typ, int64(first)))
	return ir.NewBinaryExpr(pos, ir.OLT, offset, typedInt(pos, typ, int64(limit-first)))
}
