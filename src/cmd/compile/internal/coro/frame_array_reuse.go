// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import "cmd/compile/internal/ir"

// frameArrayReuse keeps the source clear until the emitted chunk layouts
// prove that every resume sharing the storage overwrites the same byte.
// Fresh memory is zero, and subsequent activations overwrite that byte
// before any use. The remaining bytes are never modified.
type frameArrayReuse struct {
	plan   *rootArrayPlan
	clear  *ir.BlockStmt
	offset int64
	peers  []*frameArrayReuse
	block  bool
}

// pairFrameArrayReuse is called only for compatible typed chunk edges.
// Agreeing peers can still be disqualified by another activation in the
// chain. Keep the original layouts and chunk eligibility in either case.
func pairFrameArrayReuse(parent, child *frameArrayReuse) {
	if parent == nil || child == nil || parent.offset != child.offset ||
		parent.plan.writeIndex != child.plan.writeIndex {
		if parent != nil {
			parent.block = true
		}
		if child != nil {
			child.block = true
		}
		return
	}
	parent.peers = append(parent.peers, child)
	child.peers = append(child.peers, parent)
}

func resolveFrameArrayReuse(candidates map[*ir.Func]*lowerCandidate) {
	var rejected []*frameArrayReuse
	for _, candidate := range candidates {
		if array := candidate.reusedArray; array != nil && array.block {
			rejected = append(rejected, array)
		}
	}
	for i := 0; i < len(rejected); i++ {
		for _, peer := range rejected[i].peers {
			if !peer.block {
				peer.block = true
				rejected = append(rejected, peer)
			}
		}
	}
	for _, candidate := range candidates {
		if array := candidate.reusedArray; array != nil && !array.block {
			array.clear.List = nil
		}
	}
}
