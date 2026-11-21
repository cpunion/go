// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/internal/src"
	"testing"
)

func TestFrameArrayReuseChunkPeers(t *testing.T) {
	prepareLowerTest(t)
	candidates := make(map[*ir.Func]*lowerCandidate)
	add := func(offset, index int64) *frameArrayReuse {
		array := &frameArrayReuse{
			plan:   &rootArrayPlan{writeIndex: index},
			clear:  ir.NewBlockStmt(src.NoXPos, ir.Nodes{ir.NewBlockStmt(src.NoXPos, nil)}),
			offset: offset,
		}
		candidates[new(ir.Func)] = &lowerCandidate{reusedArray: array}
		return array
	}
	first, second := add(64, 0), add(64, 0)
	pairFrameArrayReuse(first, second)

	leader, middle, tail := add(64, 0), add(64, 0), add(64, 0)
	differentIndex := add(64, 1)
	pairFrameArrayReuse(leader, middle)
	pairFrameArrayReuse(middle, tail)
	pairFrameArrayReuse(tail, differentIndex)

	left, differentOffset := add(64, 0), add(72, 0)
	pairFrameArrayReuse(left, differentOffset)
	unknown := add(64, 0)
	pairFrameArrayReuse(nil, unknown)

	resolveFrameArrayReuse(candidates)
	for _, array := range []*frameArrayReuse{first, second} {
		if len(array.clear.List) != 0 {
			t.Fatal("matching typed chunk peers did not elide the array clear")
		}
	}
	for _, array := range []*frameArrayReuse{leader, middle, tail, differentIndex, left, differentOffset, unknown} {
		if len(array.clear.List) == 0 {
			t.Fatal("chunk lineage with a different or unknown writer elided the array clear")
		}
	}
}
