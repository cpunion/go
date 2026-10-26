// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (386 || arm || mips || mipsle)

package runtime

import "internal/goarch"

var (
	_ [goarch.PtrSize - 4]byte
	_ [4 - goarch.PtrSize]byte
)

// Three inline resource pointers keep the existing 32-bit allocation classes.
// A separate resource pointer would not reduce these operation sizes.
type stacklessCoroOperation struct {
	stacklessCoroOperationState
	stacklessCoroOperationResources
	waiter *sudog
}

const stacklessCoroExtraResources = 0

func (op *stacklessCoroOperation) hasResources() bool { return true }

func (op *stacklessCoroOperation) resources() *stacklessCoroOperationResources {
	return &op.stacklessCoroOperationResources
}

func (op *stacklessCoroOperation) setResources(resources *stacklessCoroOperationResources) {
	op.stacklessCoroOperationResources = *resources
}
