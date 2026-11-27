// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"internal/testenv"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"unsafe"
)

func TestStacklessCoroDirectIOAndShards(t *testing.T) {
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)
	if msg := runtime.CheckStacklessCoroDirectIOAndShardsForTest(); msg != "" {
		t.Fatal(msg)
	}
}

func TestStacklessCoroNetpollThresholdFanout(t *testing.T) {
	const child = "GO_TEST_CORO_NETPOLL_THRESHOLD_FANOUT"
	if os.Getenv(child) != "1" {
		cmd := testenv.CleanCmdEnv(testenv.Command(t, testenv.Executable(t),
			"-test.run=^TestStacklessCoroNetpollThresholdFanout$"))
		cmd.Env = append(cmd.Env, child+"=1", "GOMAXPROCS=4")
		if debug := os.Getenv("GODEBUG"); debug != "" {
			cmd.Env = append(cmd.Env, "GODEBUG="+debug)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("netpoll threshold fanout subprocess: %v\n%s", err, out)
		}
		return
	}
	if msg := runtime.CheckStacklessCoroNetpollThresholdFanoutForTest(); msg != "" {
		t.Fatal(msg)
	}
}

func TestStacklessCoroSwitchStreakFairness(t *testing.T) {
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)
	var peerRan atomic.Bool
	var rootState int
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if rootState == 0 {
			rootState = 1
			runtime.SpawnStacklessCoroForTest(ctx, func(unsafe.Pointer) uint8 {
				peerRan.Store(true)
				return runtime.StacklessCoroActionComplete
			})
			return runtime.StacklessCoroActionYield
		}
		if !peerRan.Load() {
			return runtime.StacklessCoroActionYield
		}
		return runtime.StacklessCoroActionComplete
	})
	if !peerRan.Load() {
		t.Fatal("peer task did not run")
	}
}
