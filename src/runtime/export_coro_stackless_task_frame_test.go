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

type stacklessCoroLargeTaskFrameTestFrame struct {
	stacklessCoroTaskFrameTestFrame
	pad [176]byte
}

type stacklessCoroLargeTaskFrameTestAllocation struct {
	task  stacklessCoroTask
	frame stacklessCoroLargeTaskFrameTestFrame
}

type stacklessCoroSmallTaskFrameTestFrame struct {
	payload *stacklessCoroTaskFramePayload
	probe   *stacklessCoroTaskFrameProbe
}

type stacklessCoroSmallTaskFrameTestAllocation struct {
	task  stacklessCoroTask
	frame stacklessCoroSmallTaskFrameTestFrame
}

func stacklessCoroScalarTaskFrameTestResume(ctx unsafe.Pointer) uint8 {
	context := (*stacklessCoroContext)(ctx)
	root := (*stacklessCoroTaskFrameTestRoot)(context.scheduler.root.context.frame)
	probe := root.probe
	if probe.release.Load() == 0 {
		return stacklessCoroActionYield
	}
	index := *(*uint32)(coroFrame(ctx))
	probe.sum.Add(int64(index) + 17)
	probe.done.Add(1)
	return stacklessCoroActionComplete
}

func stacklessCoroSmallTaskFrameTestResume(ctx unsafe.Pointer) uint8 {
	frame := (*stacklessCoroSmallTaskFrameTestFrame)(coroFrame(ctx))
	probe := frame.probe
	if probe.release.Load() == 0 {
		return stacklessCoroActionYield
	}
	probe.sum.Add(int64(frame.payload.value))
	probe.done.Add(1)
	if coroFrameNeedsClear(ctx) {
		frame.payload, frame.probe = nil, nil
	}
	return stacklessCoroActionComplete
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
	probe              *stacklessCoroTaskFrameProbe
	combined           []*stacklessCoroTask
	kept               []unsafe.Pointer
	count              int
	started            bool
	varyPrefix         bool
	cancelReservations bool
	phaseSwitch        bool
	largeFrame         bool
	invalidType        bool
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
			if root.phaseSwitch && i >= stacklessCoroTaskCacheSize && i%4 == 3 {
				frame := (*uint32)(coroTakeFrame(ctx, stacklessCoroScalarTaskFrameTestResume, 4))
				if frame == nil {
					frame = new(uint32)
				}
				*frame = uint32(i)
				s := (*stacklessCoroContext)(ctx).scheduler
				if unsafe.Sizeof(uintptr(0)) == 8 && !raceenabled {
					task := s.reservedTasks
					base, span, _ := findObject(uintptr(unsafe.Pointer(task)), 0, 0)
					if isStacklessCoroScalarTaskCacheMarker(task.frameSize) {
						index := uintptr(stacklessCoroScalarTaskCacheFirst - task.frameSize)
						chunk := (*stacklessCoroScalarTaskChunk)(unsafe.Add(unsafe.Pointer(task), -int(index*unsafe.Sizeof(*task))))
						if s.liveSpawns <= stacklessCoroTaskCacheSize+stacklessCoroDirectBatchedTaskCount ||
							base != uintptr(unsafe.Pointer(chunk)) || span.elemsize != unsafe.Sizeof(*chunk) ||
							unsafe.Pointer(frame) != unsafe.Pointer(&chunk.frames[index]) {
							probe.failed.Add(1)
						}
					} else if base != uintptr(unsafe.Pointer(task)) || span.elemsize != unsafe.Sizeof(stacklessCoroTask{}) {
						probe.failed.Add(1)
					}
				}
				payload := &stacklessCoroTaskFramePayload{value: uint64(i) + 17, probe: probe}
				SetFinalizer(payload, stacklessCoroTaskFrameTestFinalize)
				root.kept = append(root.kept, unsafe.Pointer(payload))
				coroSpawnFrame(ctx, unsafe.Pointer(frame), stacklessCoroScalarTaskFrameTestResume)
				continue
			}
			if root.varyPrefix && i < stacklessCoroTaskCacheSize && i%2 != 0 {
				frame := (*stacklessCoroSmallTaskFrameTestFrame)(coroTakeTypedTaskFrame(ctx,
					stacklessCoroSmallTaskFrameTestResume, unsafe.Sizeof(stacklessCoroSmallTaskFrameTestFrame{}),
					abi.TypeFor[stacklessCoroSmallTaskFrameTestAllocation]()))
				if frame == nil {
					frame = new(stacklessCoroSmallTaskFrameTestFrame)
				}
				payload := &stacklessCoroTaskFramePayload{value: uint64(i) + 17, probe: probe}
				SetFinalizer(payload, stacklessCoroTaskFrameTestFinalize)
				frame.payload, frame.probe = payload, probe
				if root.cancelReservations {
					root.kept = append(root.kept, unsafe.Pointer(frame))
					continue
				}
				coroSpawnFrame(ctx, unsafe.Pointer(frame), stacklessCoroSmallTaskFrameTestResume)
				continue
			}
			allocationType := abi.TypeFor[stacklessCoroTaskFrameTestAllocation]()
			frameSize := unsafe.Sizeof(stacklessCoroTaskFrameTestFrame{})
			large := root.largeFrame && i >= stacklessCoroTaskCacheSize
			if large {
				allocationType = abi.TypeFor[stacklessCoroLargeTaskFrameTestAllocation]()
				frameSize = unsafe.Sizeof(stacklessCoroLargeTaskFrameTestFrame{})
			}
			if root.invalidType && i == stacklessCoroTaskCacheSize {
				allocationType = abi.TypeFor[struct {
					header [6]unsafe.Pointer
					frame  stacklessCoroTaskFrameTestFrame
				}]()
			}
			frame := (*stacklessCoroTaskFrameTestFrame)(coroTakeTypedTaskFrame(ctx,
				stacklessCoroTaskFrameTestResume, frameSize,
				allocationType))
			if frame == nil {
				if large {
					frame = &new(stacklessCoroLargeTaskFrameTestFrame).stacklessCoroTaskFrameTestFrame
				} else {
					frame = new(stacklessCoroTaskFrameTestFrame)
				}
			} else {
				task := (*stacklessCoroContext)(ctx).scheduler.reservedTasks
				base, span, _ := findObject(uintptr(unsafe.Pointer(frame)), 0, 0)
				if base == uintptr(unsafe.Pointer(task)) {
					if span.elemsize != unsafe.Sizeof(stacklessCoroTask{})+frameSize || task.frameSize != stacklessCoroUncachedFrameLineage {
						probe.failed.Add(1)
					}
					root.combined = append(root.combined, task)
				}
			}
			payload := &stacklessCoroTaskFramePayload{value: uint64(i) + 17, probe: probe}
			SetFinalizer(payload, stacklessCoroTaskFrameTestFinalize)
			frame.payload, frame.other, frame.probe = payload, payload, probe
			frame.state = uint64(i) << 1
			if root.cancelReservations {
				root.kept = append(root.kept, unsafe.Pointer(frame))
				continue
			}
			coroSpawnFrame(ctx, unsafe.Pointer(frame), stacklessCoroTaskFrameTestResume)
		}
		GC()
		GC()
		if probe.finalized.Load() != 0 {
			probe.failed.Add(1)
		}
		probe.release.Store(1)
	}
	if root.cancelReservations {
		coroPanic(ctx, "typed task-frame reservation cleanup")
		return stacklessCoroActionPanic
	}
	if probe.done.Load() != uint64(root.count) {
		return stacklessCoroActionYield
	}
	// A child publishes its result before its resume call returns. Observe
	// retirement under the scheduler lock before inspecting task headers;
	// root completion alone does not join still-retiring spawned tasks.
	s := (*stacklessCoroContext)(ctx).scheduler
	lock(&s.lock)
	live := s.liveSpawns
	unlock(&s.lock)
	if live != 0 {
		return stacklessCoroActionYield
	}
	return stacklessCoroActionComplete
}

func stacklessCoroTaskFrameTestRun(root *stacklessCoroTaskFrameTestRoot) (caught bool) {
	defer func() {
		if value := recover(); value != nil {
			if value != "typed task-frame reservation cleanup" {
				panic(value)
			}
			caught = true
		}
	}()
	coroRunFrame(unsafe.Pointer(root), stacklessCoroTaskFrameTestRootResume, 0)
	return
}

// Exercise the real reservation/spawn/completion paths beyond the cache
// prefix, with precise pointer maps and payloads surviving both GC and yield.
func CheckStacklessCoroTypedTaskFramesForTest(count int, varyPrefix, cancelReservations, phaseSwitch bool) (bool, int, unsafe.Pointer) {
	return checkStacklessCoroTypedTaskFrames(count, varyPrefix, cancelReservations, phaseSwitch, false)
}

func CheckStacklessCoroLargeTaskFramesForTest(count int, varyPrefix, cancelReservations, phaseSwitch bool) (bool, int, unsafe.Pointer) {
	return checkStacklessCoroTypedTaskFrames(count, varyPrefix, cancelReservations, phaseSwitch, true)
}

func checkStacklessCoroTypedTaskFrames(count int, varyPrefix, cancelReservations, phaseSwitch, largeFrame bool) (bool, int, unsafe.Pointer) {
	StacklessCoroClearNativeTaskCachesForTest()
	probe := new(stacklessCoroTaskFrameProbe)
	root := &stacklessCoroTaskFrameTestRoot{probe: probe, count: count, varyPrefix: varyPrefix, cancelReservations: cancelReservations, phaseSwitch: phaseSwitch, largeFrame: largeFrame}
	caught := stacklessCoroTaskFrameTestRun(root)
	wantDone, wantSum := uint64(count), uint64(count*(count-1)/2+17*count)
	if cancelReservations {
		wantDone, wantSum = 0, 0
	}
	valid := caught == cancelReservations && probe.failed.Load() == 0 && probe.done.Load() == wantDone && probe.sum.Load() == wantSum
	for _, task := range root.combined {
		if task.resume != nil || task.parent != nil || task.next != nil || task.context.frame != nil || task.context.scheduler != nil || task.flags != 0 || task.frameSize != 0 || task.state != stacklessCoroTaskComplete || task.terminal != stacklessCoroTerminalNone || task.goexit || task.resuming || task.readyPending {
			valid = false
		}
	}
	n := len(root.combined)
	root.combined = nil
	root.kept = nil
	KeepAlive(root)
	return valid, n, unsafe.Pointer(probe)
}

func StacklessCoroInvalidTaskFrameTypeForTest() {
	root := &stacklessCoroTaskFrameTestRoot{
		probe:              new(stacklessCoroTaskFrameProbe),
		count:              stacklessCoroTaskCacheSize + 1,
		cancelReservations: true,
		invalidType:        true,
	}
	coroRunFrame(unsafe.Pointer(root), stacklessCoroTaskFrameTestRootResume, 0)
}

func StacklessCoroTypedTaskFrameFinalizedForTest(token unsafe.Pointer) uint64 {
	return (*stacklessCoroTaskFrameProbe)(token).finalized.Load()
}
