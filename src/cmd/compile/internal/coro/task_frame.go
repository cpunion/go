// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"internal/abi"
)

// explicitTaskFrameAllocationType describes one task followed by its frame.
// The small 16-byte frame multiples keep the combined allocator class equal
// to the existing batched-task share plus the individual frame class. Address
// taken frame variables keep their existing independent storage.
func explicitTaskFrameAllocationType(pos src.XPos, frame *types.Type, vars []*ir.Name) *types.Type {
	if types.PtrSize != 8 || !frame.HasPointers() || frame.Size() == 0 ||
		frame.Size()%16 != 0 || frame.Size() > abi.CoroTaskFrameMaxBytes {
		return nil
	}
	for _, v := range vars {
		if v.Outer.Canonical().Addrtaken() {
			return nil
		}
	}
	ptr := types.Types[types.TUNSAFEPTR]
	header := types.NewStruct([]*types.Field{
		types.NewField(pos, typecheck.Lookup("resume"), ptr),
		types.NewField(pos, typecheck.Lookup("parent"), ptr),
		types.NewField(pos, typecheck.Lookup("next"), ptr),
		types.NewField(pos, typecheck.Lookup("state"), types.Types[types.TUINT64]),
		types.NewField(pos, typecheck.Lookup("frame"), ptr),
		types.NewField(pos, typecheck.Lookup("scheduler"), ptr),
	})
	header.SetNoalg(true)
	types.CalcSize(header)
	combined := types.NewStruct([]*types.Field{
		types.NewField(pos, typecheck.Lookup("task"), header),
		types.NewField(pos, typecheck.Lookup("frame"), frame),
	})
	combined.SetNoalg(true)
	types.CalcSize(combined)
	return combined
}
