// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"testing"
)

func TestTypedTaskFrameAllocationType(t *testing.T) {
	prepareLowerTest(t)
	if types.PtrSize != 8 {
		t.Skip("combined task frames are restricted to 64-bit targets")
	}
	for _, size := range []int64{8, 16, 24, 32, 40, 48, 56, 64, 72, 200, 208, 216, 224, 256} {
		frame := types.NewArray(types.Types[types.TUNSAFEPTR], size/8)
		types.CalcSize(frame)
		allocation := explicitTaskFrameAllocationType(src.NoXPos, frame, nil, nil)
		want := size >= 16 && size <= 208 && (size%16 == 0 || (size > 32 && size%8 == 0))
		if (allocation != nil) != want {
			t.Fatalf("size %d: eligible=%t, want %t", size, allocation != nil, want)
		}
		if allocation == nil {
			continue
		}
		header := allocation.Field(0).Type
		if header.Size() != 48 || types.PtrDataSize(header) != 48 ||
			header.Field(3).Type.HasPointers() || header.Field(3).Offset != 24 ||
			header.Field(4).Offset != 32 || header.Field(5).Offset != 40 ||
			allocation.Field(1).Offset != 48 || allocation.Size() != 48+size {
			t.Fatalf("size %d: invalid private task/header allocation layout", size)
		}
	}
	frame := types.NewArray(types.Types[types.TUINT64], 4)
	types.CalcSize(frame)
	if explicitTaskFrameAllocationType(src.NoXPos, frame, nil, nil) != nil {
		t.Fatal("pointer-free frame selected instead of existing scalar/frame paths")
	}
	frame = types.NewArray(types.Types[types.TUNSAFEPTR], 4)
	types.CalcSize(frame)
	fn := testFunc("addressedTaskFrame")
	fn.DeclareParams(true)
	outer := typecheck.TempAt(src.NoXPos, fn, types.Types[types.TUNSAFEPTR])
	outer.SetAddrtaken(true)
	captured := ir.NewNameAt(src.NoXPos, typecheck.Lookup("capturedTaskFrame"), outer.Type())
	captured.Outer = outer
	if explicitTaskFrameAllocationType(src.NoXPos, frame, []*ir.Name{captured}, nil) != nil {
		t.Fatal("address-taken frame variable selected for combined storage")
	}
	if explicitTaskFrameAllocationType(src.NoXPos, frame, []*ir.Name{captured}, map[*ir.Name]bool{outer: true}) == nil {
		t.Fatal("runtime-borrowed channel/wait temporary wrongly rejected from combined storage")
	}
}
