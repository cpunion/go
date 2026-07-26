// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime_test

import (
	"runtime"
	"testing"
	"unsafe"
)

func TestStacklessCoroYield(t *testing.T) {
	var state int
	var trace []int
	runtime.RunStacklessCoroForTest(func(unsafe.Pointer) uint8 {
		switch state {
		case 0:
			trace = append(trace, 1)
			state = 1
			return runtime.StacklessCoroActionYield
		case 1:
			trace = append(trace, 2)
			state = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected state %d", state)
			return runtime.StacklessCoroActionInvalid
		}
	})
	if got, want := state, 2; got != want {
		t.Fatalf("state = %d, want %d", got, want)
	}
	if got := len(trace); got != 2 || trace[0] != 1 || trace[1] != 2 {
		t.Fatalf("trace = %v, want [1 2]", trace)
	}
}

func TestStacklessCoroAwait(t *testing.T) {
	var parentState, childState int
	var trace []string
	child := func(unsafe.Pointer) uint8 {
		switch childState {
		case 0:
			trace = append(trace, "child-yield")
			childState = 1
			return runtime.StacklessCoroActionYield
		case 1:
			trace = append(trace, "child-complete")
			childState = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected child state %d", childState)
			return runtime.StacklessCoroActionInvalid
		}
	}
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch parentState {
		case 0:
			trace = append(trace, "parent-await")
			parentState = 1
			runtime.AwaitStacklessCoroForTest(ctx, child)
			return runtime.StacklessCoroActionWait
		case 1:
			trace = append(trace, "parent-complete")
			parentState = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected parent state %d", parentState)
			return runtime.StacklessCoroActionInvalid
		}
	})
	want := []string{
		"parent-await",
		"child-yield",
		"child-complete",
		"parent-complete",
	}
	if len(trace) != len(want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
	for i := range trace {
		if trace[i] != want[i] {
			t.Fatalf("trace = %v, want %v", trace, want)
		}
	}
}

func TestStacklessCoroSpawn(t *testing.T) {
	const tasks = 100000
	var completed int
	var rootState int
	child := func(unsafe.Pointer) uint8 {
		completed++
		return runtime.StacklessCoroActionComplete
	}
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch rootState {
		case 0:
			for range tasks {
				runtime.SpawnStacklessCoroForTest(ctx, child)
			}
			rootState = 1
			return runtime.StacklessCoroActionYield
		case 1:
			if completed != tasks {
				t.Fatalf("completed = %d, want %d", completed, tasks)
			}
			rootState = 2
			return runtime.StacklessCoroActionComplete
		default:
			t.Fatalf("unexpected root state %d", rootState)
			return runtime.StacklessCoroActionInvalid
		}
	})
}
