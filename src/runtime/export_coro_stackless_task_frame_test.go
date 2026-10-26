// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/abi"
	"internal/runtime/atomic"
	"unsafe"
)

type stacklessCoroTaskFrameProbe struct {
	done, sum, failed, finalized atomic.Uint64
	release                      atomic.Uint32
}

type stacklessCoroTaskFramePayload struct {
	value uint64
	probe *stacklessCoroTaskFrameProbe
	pad   [24]byte
}

type stacklessCoroTaskFrameTestFrame struct {
	payload, other *stacklessCoroTaskFramePayload
	probe          *stacklessCoroTaskFrameProbe
	state          uint64
}

type stacklessCoroTaskFrameTestAllocation struct {
	task  stacklessCoroTask
	frame stacklessCoroTaskFrameTestFrame
}

func CheckStacklessCoroTypedTaskFrameTypesForTest() bool {
	valid := abi.TypeFor[stacklessCoroTaskFrameTestAllocation]()
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return !validStacklessCoroTaskFrameType(valid, unsafe.Sizeof(stacklessCoroTaskFrameTestFrame{}))
	}
	badPointers := abi.TypeFor[struct {
		header [6]unsafe.Pointer
		frame  stacklessCoroTaskFrameTestFrame
	}]()
	badScalars := abi.TypeFor[struct {
		header [6]uintptr
		frame  stacklessCoroTaskFrameTestFrame
	}]()
	return validStacklessCoroTaskFrameType(valid, 32) &&
		!validStacklessCoroTaskFrameType(badPointers, 32) &&
		!validStacklessCoroTaskFrameType(badScalars, 32) &&
		!validStacklessCoroTaskFrameType(valid, 24) &&
		!validStacklessCoroTaskFrameType(nil, 32)
}

func stacklessCoroTaskFrameTestResume(ctx unsafe.Pointer) uint8 {
	frame := (*stacklessCoroTaskFrameTestFrame)(coroFrame(ctx))
	index := frame.state >> 1
	if frame.payload == nil || frame.payload != frame.other || frame.payload.value != index+17 {
		throw("runtime: lost combined task-frame payload")
	}
	if frame.state&1 == 0 {
		frame.state++
		return stacklessCoroActionYield
	}
	probe := frame.probe
	if probe.release.Load() == 0 {
		return stacklessCoroActionYield
	}
	probe.sum.Add(int64(frame.payload.value))
	probe.done.Add(1)
	if coroFrameNeedsClear(ctx) {
		frame.payload, frame.other, frame.probe = nil, nil, nil
	}
	return stacklessCoroActionComplete
}

type stacklessCoroTaskFrameTestRoot struct {
	probe    *stacklessCoroTaskFrameProbe
	combined []*stacklessCoroTask
	count    int
	started  bool
}

func stacklessCoroTaskFrameTestFinalize(payload *stacklessCoroTaskFramePayload) {
	payload.probe.finalized.Add(1)
}

func stacklessCoroTaskFrameTestRootResume(ctx unsafe.Pointer) uint8 {
	root := (*stacklessCoroTaskFrameTestRoot)(coroFrame(ctx))
	probe := root.probe
	if !root.started {
		root.started = true
		for i := 0; i < root.count; i++ {
			frame := (*stacklessCoroTaskFrameTestFrame)(coroTakeTypedTaskFrame(ctx,
				stacklessCoroTaskFrameTestResume, unsafe.Sizeof(stacklessCoroTaskFrameTestFrame{}),
				abi.TypeFor[stacklessCoroTaskFrameTestAllocation]()))
			if frame == nil {
				frame = new(stacklessCoroTaskFrameTestFrame)
			} else {
				task := (*stacklessCoroContext)(ctx).scheduler.reservedTasks
				base, span, _ := findObject(uintptr(unsafe.Pointer(frame)), 0, 0)
				if base == uintptr(unsafe.Pointer(task)) {
					if span.elemsize != unsafe.Sizeof(stacklessCoroTaskFrameTestAllocation{}) || task.frameSize != stacklessCoroUncachedFrameLineage {
						probe.failed.Add(1)
					}
					root.combined = append(root.combined, task)
				}
			}
			payload := &stacklessCoroTaskFramePayload{value: uint64(i) + 17, probe: probe}
			SetFinalizer(payload, stacklessCoroTaskFrameTestFinalize)
			frame.payload, frame.other, frame.probe = payload, payload, probe
			frame.state = uint64(i) << 1
			coroSpawnFrame(ctx, unsafe.Pointer(frame), stacklessCoroTaskFrameTestResume)
		}
		GC()
		GC()
		if probe.finalized.Load() != 0 {
			probe.failed.Add(1)
		}
		probe.release.Store(1)
	}
	if probe.done.Load() != uint64(root.count) {
		return stacklessCoroActionYield
	}
	return stacklessCoroActionComplete
}

// Exercise the real reservation/spawn/completion paths beyond the cache
// prefix, with precise pointer maps and payloads surviving both GC and yield.
func CheckStacklessCoroTypedTaskFramesForTest(count int) (bool, int, unsafe.Pointer) {
	StacklessCoroClearNativeTaskCachesForTest()
	probe := new(stacklessCoroTaskFrameProbe)
	root := &stacklessCoroTaskFrameTestRoot{probe: probe, count: count}
	coroRunFrame(unsafe.Pointer(root), stacklessCoroTaskFrameTestRootResume, 0)
	valid := probe.failed.Load() == 0 && probe.done.Load() == uint64(count) && probe.sum.Load() == uint64(count*(count-1)/2+17*count)
	for _, task := range root.combined {
		if task.resume != nil || task.parent != nil || task.next != nil || task.context.frame != nil || task.context.scheduler != nil || task.flags != 0 || task.frameSize != 0 || task.state != stacklessCoroTaskComplete || task.terminal != stacklessCoroTerminalNone || task.goexit || task.resuming || task.readyPending {
			valid = false
		}
	}
	n := len(root.combined)
	root.combined = nil
	KeepAlive(root)
	return valid, n, unsafe.Pointer(probe)
}

func StacklessCoroTypedTaskFrameFinalizedForTest(token unsafe.Pointer) uint64 {
	return (*stacklessCoroTaskFrameProbe)(token).finalized.Load()
}
