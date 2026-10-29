// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime_test

import (
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"unsafe"
)

// channelPopulationRound uses actual blocked receives and channel close
// completion. Tokens identify cleared operations, independently of cache
// counters or the allocation strategy used by the implementation.
func channelPopulationRound(t *testing.T, tokens []unsafe.Pointer) {
	t.Helper()
	channel := make(chan int)
	var started, completed atomic.Int32
	step := 0
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch step {
		case 0:
			step++
			for range tokens {
				childStep, value := 0, 0
				var ok bool
				runtime.SpawnStacklessCoroForTest(ctx, func(child unsafe.Pointer) uint8 {
					if childStep == 0 {
						childStep++
						runtime.RecvIntStacklessCoroForTest(child, channel, &value, &ok)
						started.Add(1)
						return runtime.StacklessCoroActionWait
					}
					if value != 0 || ok {
						t.Errorf("closed receive = (%d, %t)", value, ok)
					}
					completed.Add(1)
					return runtime.StacklessCoroActionComplete
				})
			}
			return runtime.StacklessCoroActionYield
		case 1:
			if int(started.Load()) != len(tokens) {
				return runtime.StacklessCoroActionYield
			}
			if got := runtime.StacklessCoroChannelPopulationForTest(channel, tokens); got != len(tokens) {
				t.Fatalf("queued channel operations = %d, want %d", got, len(tokens))
			}
			step++
			close(channel)
			return runtime.StacklessCoroActionYield
		}
		if int(completed.Load()) != len(tokens) {
			return runtime.StacklessCoroActionYield
		}
		return runtime.StacklessCoroActionComplete
	})
	if int(started.Load()) != len(tokens) || int(completed.Load()) != len(tokens) {
		t.Fatal("channel population did not complete")
	}
}

func TestStacklessCoroNativeChannelPopulationReuse(t *testing.T) {
	const child = "GO_CORO_CHANNEL_POPULATION"
	if os.Getenv(child) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestStacklessCoroNativeChannelPopulationReuse$", "-test.count=1")
		cmd.Env = append(os.Environ(), child+"=1", "GOMAXPROCS=1", "GOGC=off")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("channel population child: %v\n%s", err, output)
		}
		return
	}
	procs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(procs)
	gc := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gc)
	const population = 4096
	first, second := make([]unsafe.Pointer, population), make([]unsafe.Pointer, population)
	channelPopulationRound(t, first)
	channelPopulationRound(t, second)
	old := make(map[unsafe.Pointer]bool, population)
	for _, token := range first {
		if token == nil || old[token] {
			t.Fatal("first population has nil or duplicate active operation")
		}
		old[token] = true
	}
	reused := 0
	for _, token := range second {
		if old[token] {
			reused++
		}
	}
	if reused < 9*population/10 {
		t.Fatalf("4096 blocked channel waiters reused only %d operation addresses", reused)
	}
	runtime.KeepAlive(first)
}
