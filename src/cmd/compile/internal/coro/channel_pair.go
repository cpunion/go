// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"go/constant"
)

const (
	channelPairFirst = 1
	channelPairLast  = 2
)

// A successful direct channel operation can leave a ready peer to its
// current executor when the following operation is reached without executing
// user calls, loops or potentially blocking code. The second operation
// preserves the evaluation order and either waits or flushes the wake.
func lowerChannelPairs(states []lowerState, candidate *lowerCandidate) map[int]uint8 {
	var pairs map[int]uint8
	for i, state := range states {
		if state.transition != SiteChannel || state.channel == nil ||
			state.channel.rangeStmt != nil || knownBufferedChannel(state.channel.channel) {
			continue
		}
		var channels []int
		budget := 8
		if !shortChannelPairPath(states, candidate, state.next, &budget, &channels) || len(channels) == 0 {
			continue
		}
		if pairs == nil {
			pairs = make(map[int]uint8)
		}
		pairs[i] |= channelPairFirst
		for _, next := range channels {
			pairs[next] |= channelPairLast
		}
	}
	return pairs
}

func knownBufferedChannel(channel ir.Node) bool {
	makeChan, ok := ir.StaticValue(channel).(*ir.MakeExpr)
	return ok && makeChan.Op() == ir.OMAKECHAN && makeChan.Len != nil &&
		ir.IsConst(makeChan.Len, constant.Int) && constant.Sign(makeChan.Len.Val()) > 0
}

func shortChannelPairPath(states []lowerState, candidate *lowerCandidate, at int, budget *int, channels *[]int) bool {
	if at < 0 || at >= len(states) || *budget == 0 {
		return false
	}
	*budget--
	state := states[at]
	if state.switchStmt != nil || state.panicValue != nil || state.wait != nil || state.io != nil ||
		!shortChannelPairBody(state.body, candidate.rangeMarkers) {
		return false
	}
	if state.cleanup {
		// A local self-frame completion can jump back into source code
		// without returning to the scheduler.
		return !candidate.dynamicDefers && len(candidate.defers) == 0 && !candidate.localSelfFrameLoop()
	}
	if state.terminal != actionInvalid && state.terminal != actionComplete {
		return false
	}
	if state.transition != SiteInvalid {
		c := state.channel
		if state.transition != SiteChannel || c == nil || c.rangeStmt != nil ||
			c.channel.Type().Elem().Size() > 16 || len(c.node.Init()) != 0 ||
			!shortChannelPairExpr(c.channel, 0) || !shortChannelPairExpr(c.sendValue, 0) {
			return false
		}
		*channels = append(*channels, at)
		return true
	}
	if state.thenState >= 0 {
		if !shortChannelPairExpr(state.condition, 0) ||
			!shortChannelPairPath(states, candidate, state.thenState, budget, channels) {
			return false
		}
		return state.condition == nil || shortChannelPairPath(states, candidate, state.elseState, budget, channels)
	}
	return shortChannelPairPath(states, candidate, state.next, budget, channels)
}

func shortChannelPairBody(body []ir.Node, rangeMarkers map[*ir.AssignStmt]*ir.CallExpr) bool {
	if len(body) > 3 {
		return false
	}
	for _, node := range body {
		if len(node.Init()) != 0 {
			return false
		}
		switch n := node.(type) {
		case *ir.Decl:
			if n.X.Esc() == ir.EscHeap || n.X.Type() == nil || n.X.Type().Size() > 16 {
				return false
			}
		case *ir.AssignStmt:
			if _, ok := n.X.(*ir.Name); !ok || rangeMarkers[n] != nil ||
				!shortChannelPairExpr(n.X, 0) || !shortChannelPairExpr(n.Y, 0) {
				return false
			}
		case *ir.AssignOpStmt:
			if _, ok := n.X.(*ir.Name); !ok || !n.X.Type().IsInteger() ||
				(n.AsOp != ir.OADD && n.AsOp != ir.OSUB) || !shortChannelPairExpr(n.Y, 0) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func shortChannelPairExpr(n ir.Node, depth int) bool {
	if n == nil {
		return true
	}
	if depth > 2 || len(n.Init()) != 0 || n.Type() == nil || n.Type().Size() > 16 {
		return false
	}
	switch n.Op() {
	case ir.ONAME, ir.OLITERAL, ir.ONIL:
		return true
	case ir.OADD, ir.OSUB, ir.OXOR, ir.OAND, ir.OOR:
		b := n.(*ir.BinaryExpr)
		return !b.Type().IsString() && shortChannelPairExpr(b.X, depth+1) && shortChannelPairExpr(b.Y, depth+1)
	case ir.OLT, ir.OLE, ir.OGT, ir.OGE, ir.OEQ, ir.ONE:
		b := n.(*ir.BinaryExpr)
		return b.X.Type().IsInteger() && shortChannelPairExpr(b.X, depth+1) && shortChannelPairExpr(b.Y, depth+1)
	}
	return false
}
