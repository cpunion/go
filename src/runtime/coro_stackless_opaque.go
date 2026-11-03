// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

// An opaque go target still owns a scheduler-tracked logical task. Its first
// resume hands execution to an independent ordinary host; subsequent resumes
// only propagate its terminal state or completion, without replaying the call.
type stacklessCoroOpaqueFrame struct {
	call    func()
	started bool
}

func coroSpawnOpaque(ctx unsafe.Pointer, call func()) bool {
	if call == nil {
		throw("runtime: nil stackless coroutine opaque spawn")
	}
	frame := &stacklessCoroOpaqueFrame{call: call}
	return coroSpawnFrame(ctx, unsafe.Pointer(frame), stacklessCoroOpaqueResume)
}

func stacklessCoroOpaqueResume(ctx unsafe.Pointer) uint8 {
	if action := coroTerminalAction(ctx); action != stacklessCoroActionInvalid {
		return action
	}
	frame := (*stacklessCoroOpaqueFrame)(coroFrame(ctx))
	if !frame.started {
		frame.started = true
		call := frame.call
		frame.call = nil
		return coroHostCall(ctx, call)
	}
	return stacklessCoroActionComplete
}
