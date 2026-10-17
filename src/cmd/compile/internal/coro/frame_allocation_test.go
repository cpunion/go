// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/types"
	"testing"
)

func TestExplicitFrameChunkAllocationTargets(t *testing.T) {
	prepareLowerTest(t)
	ptrSize := types.PtrSize
	defer func() { types.PtrSize = ptrSize }()
	for _, test := range []struct {
		ptrSize, frameSize, length int64
		want                       bool
	}{
		{8, 32, 4, true}, // Equal bytes after state packing.
		{4, 32, 4, true},
		{8, 64, 4, true},
		{4, 64, 4, false}, // The chunk needs a malloc header on this target.
		{8, 72, 4, true},
		{4, 72, 4, true},
		{8, 80, 4, true},
		{4, 80, 4, false},
		{8, 0, 4, false},
		{8, -1, 4, false},
		{8, 32, 0, false},
		{8, explicitFrameChunkByteLimit, 4, false},
	} {
		types.PtrSize = int(test.ptrSize)
		if got := explicitFrameChunkFitsAllocation(test.frameSize, test.length); got != test.want {
			t.Errorf("ptr%d frame%d chunk%d: admitted = %v, want %v",
				test.ptrSize, test.frameSize, test.length, got, test.want)
		}
	}
}
