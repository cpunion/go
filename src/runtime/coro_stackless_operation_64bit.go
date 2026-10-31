// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !(386 || arm || mips || mipsle)

package runtime

import "internal/goarch"

var (
	_ [goarch.PtrSize - 8]byte
	_ [8 - goarch.PtrSize]byte
)

// Channel operations need only a retained waiter. Allocate the other reusable
// resources with the first IO, timer, or select payload that needs them.
type stacklessCoroOperation struct {
	stacklessCoroOperationState
	*stacklessCoroOperationResources
	waiter *channelWaiter
}

const stacklessCoroExtraResources = 1

func (op *stacklessCoroOperation) hasResources() bool {
	return op.stacklessCoroOperationResources != nil
}

func (op *stacklessCoroOperation) resources() *stacklessCoroOperationResources {
	return op.stacklessCoroOperationResources
}

func (op *stacklessCoroOperation) setResources(resources *stacklessCoroOperationResources) {
	op.stacklessCoroOperationResources = resources
}

const stacklessCoroChannelWaiterPadding = 0
