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
// On 64-bit targets, Go allocator size classes above 32 bytes are 16-byte
// multiples, so 16-byte frame multiples and 8-byte multiples >= 40 bytes
// keep the combined allocator class equal to the existing 48-byte batched-task
// share plus the individual frame class. User address-taken frame variables
// keep their existing independent storage; synthetic or non-escaping addresses
// borrowed exclusively by coroutine runtime operations (e.g. coroChanSend,
// coroChanRecv, coroSelect, sync/IO waits) never outlive the coroutine.
func explicitTaskFrameAllocationType(pos src.XPos, frame *types.Type, vars []*ir.Name, runtimeBorrowed map[*ir.Name]bool) *types.Type {
	size := frame.Size()
	if types.PtrSize != 8 || !frame.HasPointers() || size < 16 ||
		size > abi.CoroTaskFrameMaxBytes ||
		(size%16 != 0 && (size <= 32 || size%8 != 0)) {
		return nil
	}
	for _, v := range vars {
		outer := v.Outer.Canonical()
		if outer.Addrtaken() && !runtimeBorrowed[outer] {
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
