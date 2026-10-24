// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/types"
	"slices"
)

// explicitFramePaddingFields fills a small fixed prefix gap using pointer-free
// fields without changing the order of field initialization. The
// caller keeps the state and runtime header offsets fixed. Only layout changes;
// every selected field retains its original type and identity.
func explicitFramePaddingFields(fields []*types.Field, state *types.Field,
	start, end int64) []*types.Field {
	var candidates []*types.Field
	for _, field := range fields {
		types.CalcSize(field.Type)
		if field != state && field.Type.Size() > 0 &&
			field.Type.Size() <= end-start && !field.Type.HasPointers() {
			candidates = append(candidates, field)
		}
	}
	slices.SortStableFunc(candidates, func(a, b *types.Field) int {
		return int(a.Type.Alignment()) - int(b.Type.Alignment())
	})
	// Both gaps have at most four bytes. Keep the best payload for each end
	// offset so an early byte field cannot prevent a later two-byte field
	// from filling the gap. Equal choices retain source capture order.
	type packing struct {
		fields []*types.Field
		bytes  int64
	}
	choices := make([]packing, end-start+1)
	for _, field := range candidates {
		align := int64(field.Type.Alignment())
		for offset := end - field.Type.Size(); offset >= start; offset-- {
			prior := choices[offset-start]
			if offset != start && len(prior.fields) == 0 {
				continue
			}
			next := (offset+align-1)&^(align-1) + field.Type.Size()
			if next > end || prior.bytes+field.Type.Size() <= choices[next-start].bytes {
				continue
			}
			choices[next-start] = packing{
				fields: append(slices.Clone(prior.fields), field),
				bytes:  prior.bytes + field.Type.Size(),
			}
		}
	}
	var best packing
	for _, choice := range choices {
		if choice.bytes > best.bytes {
			best = choice
		}
	}
	return best.fields
}

// explicitFrameFieldsWithStatePadding reuses the four-byte gap after a plain
// frame's state word. Keep this optimization below the runtime's 32 KiB / 256
// cached-task threshold: reducing a larger frame's size can retain more frames
// under the same byte budget. Fused frames have separate chunk admission and
// retention boundaries and keep their original layout.
func explicitFrameFieldsWithStatePadding(fields []*types.Field, state *types.Field) []*types.Field {
	if types.PtrSize != 8 || state == nil || len(fields) < 2 || fields[0] != state {
		return fields
	}
	types.CalcSize(fields[1].Type)
	if fields[1].Type.Alignment() != 8 {
		return fields
	}
	original := types.NewStruct(fields)
	types.CalcSize(original)
	if original.Size() > 128 {
		return fields
	}
	packed := explicitFramePaddingFields(fields, state, 4, 8)
	if len(packed) == 0 {
		return fields
	}
	layout := make([]*types.Field, 0, len(fields))
	layout = append(layout, state)
	layout = append(layout, packed...)
	for _, field := range fields {
		if field != state && !slices.Contains(packed, field) {
			layout = append(layout, field)
		}
	}
	return layout
}

// explicitFusedFrameFieldsWithPadding fills the three-byte gap before state
// in a heterogeneous frame. Preserve the runtime header and never disable an
// existing four-frame chunk proof by crossing an allocator boundary. Native chunk
// caches charge allocation classes and release idle payload after a GC epoch.
func explicitFusedFrameFieldsWithPadding(fields, captured []*types.Field,
	state, savedResume *types.Field) []*types.Field {
	if types.PtrSize != 8 || state == nil || savedResume == nil ||
		len(fields) < 5 || fields[3] != state || fields[4] != savedResume {
		return fields
	}
	original := types.NewStruct(fields)
	types.CalcSize(original)
	originalSize := original.Size()
	if state.Offset != 20 || savedResume.Offset != 24 {
		return fields
	}
	packed := explicitFramePaddingFields(captured, state, 17, 20)
	if len(packed) == 0 {
		return fields
	}
	layout := make([]*types.Field, 0, len(fields))
	layout = append(layout, fields[:3]...)
	layout = append(layout, packed...)
	for _, field := range fields[3:] {
		if !slices.Contains(packed, field) {
			layout = append(layout, field)
		}
	}
	candidate := types.NewStruct(layout)
	types.CalcSize(candidate)
	if candidate.Size() >= originalSize || state.Offset != 20 || savedResume.Offset != 24 ||
		explicitFrameChunkFitsAllocation(originalSize, explicitLargeFrameChunkSize) &&
			!explicitFrameChunkFitsAllocation(candidate.Size(), explicitLargeFrameChunkSize) {
		return fields
	}
	return layout
}
