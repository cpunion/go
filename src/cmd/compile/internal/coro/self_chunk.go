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

// inlineSelfChunkAwaits installs the interior fast path only for an exact
// local self-await. Those resumes already check terminal actions at entry,
// and return to the scheduler before a new task can become active. Race and
// comparison fallbacks never publish a validated fused chunk marker.
func inlineSelfChunkAwaits(candidate *lowerCandidate, resume *ir.Func,
	frame *ir.Name, markerField *types.Field, factoryFrame, factoryZero *ir.Name,
	prologue, initializers ir.Nodes) {
	factory := candidate.factory
	locals := chunkFactoryLocals(prologue)
	if locals == nil || !chunkFactoryStoresKnown(factory, factoryFrame, factoryZero, locals, initializers) {
		return
	}

	first := selfChunkFirstMarker(candidate.frameChunkLength)
	last := first + uint8(candidate.frameChunkLength-1)
	ctx := resume.Dcl[0]
	chunk := types.NewArray(candidate.frameType, candidate.frameChunkLength)
	unsafePointer := types.Types[types.TUNSAFEPTR]
	for _, proof := range candidate.selfChunkAwaits {
		if proof.await == nil {
			continue
		}
		pos := proof.factory.Pos()
		next := typecheck.TempAt(pos, resume, frame.Type())
		markerType := markerField.Type
		marker := typecheck.TempAt(pos, resume, markerType)
		bindings := map[*ir.Name]ir.Node{
			factoryFrame:                             next,
			factory.Type().Param(0).Nname.(*ir.Name): ctx,
		}
		if factoryZero != nil {
			fresh := ir.NewBinaryExpr(pos, ir.OAND, explicitFrameField(pos, next, markerField),
				typedInt(pos, markerType, int64(abi.CoroFrameFresh)))
			bindings[factoryZero] = typecheck.Expr(ir.NewBinaryExpr(pos, ir.OEQ, fresh,
				typedInt(pos, markerType, 0)))
		}
		for name, value := range locals {
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
			bindings[factory.Type().Param(i).Nname.(*ir.Name)] = value
			proof.factory.Args[i] = value
		}
		// Evaluate every argument in source order before writing the new frame.
		prefix = append(prefix, ir.NewDecl(pos, ir.ODCL, marker),
			ir.NewAssignStmt(pos, marker, explicitFrameField(pos, frame, markerField)))
		inChunk := markerInSelfChunk(pos, marker, first, last+1)
		size := typedInt(pos, types.Types[types.TUINTPTR], candidate.frameType.Size())
		addr := typecheck.Expr(ir.NewBinaryExpr(pos, ir.OUNSAFEADD,
			typecheck.ConvNop(frame, unsafePointer), size))
		nextMarker := typecheck.TempAt(pos, resume, markerType)
		take := typecheck.Call(pos, typecheck.LookupRuntime("coroTakeSelfChunk"),
			ir.Nodes{ctx, size, reflectdata.TypePtrAt(pos, chunk)}, false)
		atEnd := ir.NewBinaryExpr(pos, ir.OEQ, marker, typedInt(pos, markerType, int64(last)))
		useNext := ir.Nodes{
			ir.NewDecl(pos, ir.ODCL, next), ir.NewDecl(pos, ir.ODCL, nextMarker),
			ir.NewIfStmt(pos, atEnd, ir.Nodes{
				ir.NewAssignStmt(pos, next, typecheck.ConvNop(take, next.Type())),
				ir.NewAssignStmt(pos, nextMarker, typedInt(pos, markerType, int64(first))),
			}, ir.Nodes{
				ir.NewAssignStmt(pos, next, typecheck.ConvNop(addr, next.Type())),
				ir.NewAssignStmt(pos, nextMarker, ir.NewBinaryExpr(pos, ir.OADD, marker, typedInt(pos, markerType, 1))),
			}),
		}
		useNext = append(useNext, copyChunkFactoryStores(initializers, bindings)...)
		enter := typecheck.Call(pos, typecheck.LookupRuntime("coroAdvanceSelfChunk"),
			ir.Nodes{ctx, typecheck.ConvNop(next, unsafePointer), nextMarker}, false)
		useNext = append(useNext, ir.NewReturnStmt(pos, ir.Nodes{enter}))
		proof.await.List = append(append(prefix, ir.NewIfStmt(pos, inChunk, useNext, nil)), proof.await.List...)
	}
}

func selfChunkFirstMarker(length int64) uint8 {
	first := abi.CoroFrameChunkValidated | abi.CoroFrameChunkFirst
	if length == explicitLargeFrameChunkSize {
		first |= abi.CoroFrameShortChunk
	}
	return first
}

func markerInSelfChunk(pos src.XPos, marker ir.Node, first, limit uint8) ir.Node {
	typ := marker.Type()
	offset := ir.NewBinaryExpr(pos, ir.OSUB, marker, typedInt(pos, typ, int64(first)))
	return ir.NewBinaryExpr(pos, ir.OLT, offset, typedInt(pos, typ, int64(limit-first)))
}
