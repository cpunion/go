// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package runtime_test

import (
	"internal/runtime/cgobench"
	"runtime"
	"testing"
	"unsafe"
)

type stacklessCoroComparisonDirectCFrame struct {
	remaining int
	value     uint64
	episodes  bool
}

func stacklessCoroComparisonDirectCResume(ctx unsafe.Pointer) uint8 {
	frame := (*stacklessCoroComparisonDirectCFrame)(
		runtime.FrameStacklessCoroForTest(ctx))
	if frame.remaining == 0 {
		return runtime.StacklessCoroActionComplete
	}
	if !frame.episodes {
		frame.value += cgobench.NoBlockCalls(frame.remaining)
		frame.remaining = 0
		return runtime.StacklessCoroActionComplete
	}
	frame.value += cgobench.NoBlockCall()
	frame.remaining--
	return runtime.StacklessCoroActionYield
}

func runStacklessCoroComparisonDirectC(driver stacklessCoroComparisonDriver,
	iterations int, episodes bool) uint64 {
	frame := &stacklessCoroComparisonDirectCFrame{
		remaining: iterations,
		episodes:  episodes,
	}
	driver.run(unsafe.Pointer(frame), stacklessCoroComparisonDirectCResume)
	return frame.value
}

func TestStacklessCoroPushPullDirectCComparison(t *testing.T) {
	for _, driver := range stacklessCoroComparisonDrivers {
		for _, episodes := range [...]bool{false, true} {
			if got := runStacklessCoroComparisonDirectC(driver, 3,
				episodes); got != 3 {
				t.Fatalf("%s direct C comparison (episodes=%t) = %d, want 3",
					driver.name, episodes, got)
			}
		}
	}
}

func BenchmarkStacklessCoroPushPullDirectCSteady(b *testing.B) {
	b.Run("OrdinaryCgo", func(b *testing.B) {
		b.ReportAllocs()
		cgobench.CgoCalls(b.N)
	})
	b.Run("DirectNoBlock", func(b *testing.B) {
		b.ReportAllocs()
		if got := cgobench.NoBlockCalls(b.N); got != uint64(b.N) {
			b.Fatalf("direct calls = %d, want %d", got, b.N)
		}
	})
	for _, driver := range stacklessCoroComparisonDrivers {
		b.Run(driver.name, func(b *testing.B) {
			b.ReportAllocs()
			if got := runStacklessCoroComparisonDirectC(driver, b.N,
				false); got != uint64(b.N) {
				b.Fatalf("direct calls = %d, want %d", got, b.N)
			}
		})
	}
}

func BenchmarkStacklessCoroPushPullDirectCEpisodes(b *testing.B) {
	for _, driver := range stacklessCoroComparisonDrivers {
		b.Run(driver.name, func(b *testing.B) {
			b.ReportAllocs()
			if got := runStacklessCoroComparisonDirectC(driver, b.N,
				true); got != uint64(b.N) {
				b.Fatalf("direct calls = %d, want %d", got, b.N)
			}
		})
	}
}
