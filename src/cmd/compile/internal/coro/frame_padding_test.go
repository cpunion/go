// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"fmt"
	"slices"
	"testing"
)

func TestExplicitFramePadding(t *testing.T) {
	prepareLowerTest(t)
	pkg := types.NewPkg("example.com/coro/framepadding", "framepadding")
	u8, u16, u32 := types.Types[types.TUINT8], types.Types[types.TUINT16], types.Types[types.TUINT32]
	pointer := types.NewPtr(types.Types[types.TINT])
	for _, test := range []struct {
		name  string
		types []*types.Type
		plain int64
		fused int64
	}{
		{"empty", nil, 0, 0},
		{"bytes", []*types.Type{u8, u8, u8, u8, u8}, 4, 3},
		{"alignment", []*types.Type{u8, u16, u8}, 4, 3},
		{"short", []*types.Type{u16}, 2, 2},
		{"word", []*types.Type{u32}, 4, 0},
		{"word-or-bytes", []*types.Type{u32, u8, u8, u8}, 4, 3},
		{"array", []*types.Type{types.NewArray(u8, 3), u16, u8}, 4, 3},
		{"two-shorts", []*types.Type{u16, types.NewArray(u8, 2)}, 4, 2},
		{"pointer-and-zero", []*types.Type{pointer, types.NewArray(pointer, 0)}, 0, 0},
		{"mixed", []*types.Type{pointer, u8, pointer, u16}, 3, 3},
	} {
		for mode := 0; mode < 3; mode++ {
			t.Run(fmt.Sprintf("%s/mode%d", test.name, mode), func(t *testing.T) {
				state := types.NewField(src.NoXPos, pkg.Lookup("state"), u32)
				fields := []*types.Field{state}
				for i, typ := range test.types {
					fields = append(fields, types.NewField(src.NoXPos, pkg.Lookup(fmt.Sprintf("F%d", i)), typ))
				}
				start, end, want := int64(4), int64(8), test.plain
				if mode != 0 {
					start, end, want = 17, 20, test.fused
				}
				packed := explicitFramePaddingFields(fields, state, start, end)
				seen := make(map[*types.Field]bool)
				offset, payload := start, int64(0)
				for _, field := range packed {
					if field == state || seen[field] || !slices.Contains(fields, field) || field.Type.HasPointers() {
						t.Fatalf("invalid packed field %v", field)
					}
					seen[field] = true
					align := int64(field.Type.Alignment())
					offset = (offset+align-1)&^(align-1) + field.Type.Size()
					payload += field.Type.Size()
				}
				if offset > end || payload != want {
					t.Fatalf("end=%d payload=%d, want end <= %d and payload=%d", offset, payload, end, want)
				}
				var prefix []*types.Field
				if mode != 0 {
					for _, name := range []string{"parent", "owner"} {
						prefix = append(prefix, types.NewField(src.NoXPos, pkg.Lookup(name), types.Types[types.TUNSAFEPTR]))
					}
					prefix = append(prefix, types.NewField(src.NoXPos, pkg.Lookup("marker"), u8))
				}
				layout := slices.Clone(prefix)
				if mode == 0 {
					layout = append(layout, state)
				}
				layout = append(layout, packed...)
				if mode != 0 {
					layout = append(layout, state)
				}
				var savedResume *types.Field
				if mode == 2 {
					savedResume = types.NewField(src.NoXPos, pkg.Lookup("resume"), types.Types[types.TUNSAFEPTR])
					layout = append(layout, savedResume)
				}
				for _, field := range fields {
					if field != state && !seen[field] {
						layout = append(layout, field)
					}
				}
				frame := types.NewStruct(layout)
				types.CalcSize(frame)
				wantState := int64(0)
				if mode != 0 {
					wantState = 20
				}
				if state.Offset != wantState || (savedResume != nil && savedResume.Offset != 24) {
					t.Fatalf("changed fixed state/resume offsets: state=%d resume=%v", state.Offset, savedResume)
				}
				for _, field := range packed {
					if field.Offset < start || field.Offset+field.Type.Size() > end {
						t.Fatalf("packed field outside padding: %v", field)
					}
				}
			})
		}
	}
}

func TestExplicitFrameFieldsWithStatePadding(t *testing.T) {
	prepareLowerTest(t)
	pkg := types.NewPkg("example.com/coro/statepadding", "statepadding")
	u8, u32 := types.Types[types.TUINT8], types.Types[types.TUINT32]
	pointer := types.NewPtr(types.Types[types.TINT])
	for _, test := range []struct {
		name       string
		fields     []*types.Type
		stateIndex int
		packed     bool
	}{
		{"empty", nil, -1, false},
		{"no-state", []*types.Type{u32, pointer, u8}, -1, false},
		{"only-state", []*types.Type{u32}, 0, false},
		{"state-not-first", []*types.Type{pointer, u32, u8}, 1, false},
		{"no-gap", []*types.Type{u32, u8, pointer}, 0, false},
		{"no-small-field", []*types.Type{u32, pointer, pointer}, 0, false},
		{"small", []*types.Type{u32, pointer, u8}, 0, true},
		{"limit", []*types.Type{u32, pointer, types.NewArray(u8, 104), u8}, 0, true},
		{"over-limit", []*types.Type{u32, pointer, types.NewArray(u8, 112), u8}, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var fields []*types.Field
			for i, typ := range test.fields {
				fields = append(fields, types.NewField(src.NoXPos, pkg.Lookup(fmt.Sprintf("F%d", i)), typ))
			}
			var state *types.Field
			if test.stateIndex >= 0 {
				state = fields[test.stateIndex]
			}
			layout := explicitFrameFieldsWithStatePadding(fields, state)
			if got := !slices.Equal(layout, fields); got != test.packed {
				t.Fatalf("packed=%v, want %v", got, test.packed)
			}
			if test.packed {
				frame := types.NewStruct(layout)
				types.CalcSize(frame)
				if state.Offset != 0 || layout[1].Offset != 4 || fields[1].Offset != 8 {
					t.Fatalf("changed state/pointer offsets: %d/%d/%d", state.Offset, layout[1].Offset, fields[1].Offset)
				}
			}
		})
	}
}
