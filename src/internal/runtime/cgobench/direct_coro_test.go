// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package cgobench_test

import (
	"internal/runtime/cgobench"
	"io"
	"os"
	"runtime"
	"testing"
)

func TestCalls(t *testing.T) {
	cgobench.CgoCalls(2)
	cgobench.CgoCall()
	cgobench.DirectCalls(2)
	cgobench.DirectCall()
	if got := cgobench.NoBlockCalls(2); got != 2 {
		t.Fatalf("NoBlockCalls(2) = %d, want 2", got)
	}
	if got := cgobench.NoBlockCall(); got != 1 {
		t.Fatalf("NoBlockCall() = %d, want 1", got)
	}

	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)
	testHandoffs(t, 8,
		cgobench.ResetCgoHandoff,
		cgobench.ReleaseCgoHandoff,
		cgobench.CgoHandoffs)
	testFailedHandoff(t, cgobench.ResetCgoHandoff, cgobench.CgoHandoffs)
	testHandoffs(t, 8,
		cgobench.ResetDirectHandoff,
		cgobench.ReleaseDirectHandoff,
		cgobench.DirectHandoffs)
	testFailedHandoff(t, cgobench.ResetDirectHandoff, cgobench.DirectHandoffs)
	testRunnableHandoffs(t, cgobench.CgoRunnableHandoffs)
	testRunnableHandoffs(t, cgobench.DirectRunnableHandoffs)
}

func testHandoffs(t *testing.T, iterations int, reset func(),
	release func(uint32), handoffs func(int, int) uint64) {
	t.Helper()
	reset()
	readFile, writeFile, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readFile.Close()
	defer writeFile.Close()
	workerErr := make(chan error, 1)
	go func() {
		var token [1]byte
		for epoch := uint32(1); epoch <= uint32(iterations); epoch++ {
			if _, err := io.ReadFull(readFile, token[:]); err != nil {
				workerErr <- err
				return
			}
			release(epoch)
		}
		workerErr <- nil
	}()
	if got := handoffs(iterations, int(writeFile.Fd())); got == ^uint64(0) {
		t.Fatalf("handoffs(%d) failed", iterations)
	}
	if err := <-workerErr; err != nil {
		t.Fatal(err)
	}
}

func testFailedHandoff(t *testing.T, reset func(), handoffs func(int, int) uint64) {
	t.Helper()
	reset()
	if got := handoffs(1, -1); got != ^uint64(0) {
		t.Fatalf("handoffs with invalid descriptor = %d, want failure", got)
	}
}

func testRunnableHandoffs(t *testing.T, handoffs func(int) uint64) {
	t.Helper()
	if got := handoffs(8); got == ^uint64(0) {
		t.Fatal("runnable handoff failed")
	}
	if got := handoffs(0); got != 0 {
		t.Fatalf("zero runnable handoffs = %d, want 0", got)
	}
	if got := handoffs(-1); got != ^uint64(0) {
		t.Fatalf("negative runnable handoffs = %d, want failure", got)
	}
}

// Run these benchmarks with:
//
//	GOEXPERIMENT=coro go test internal/runtime/cgobench \
//		-run=^$ \
//		-bench='(Ordinary|Direct|NoBlock)Cgo(CallsSteady|CallEntry|BlockingHandoff|RunnableHandoff)$' \
//		-gcflags=internal/runtime/cgobench='-l -d=coro=4'
//
// The Steady benchmarks batch calls within one coroutine root and isolate the
// foreign boundary. The Entry benchmarks include root setup on every call.
// The blocking benchmarks use an ordinary goroutine and a pipe. The runnable
// benchmarks use an atomic-only C gate and one sibling per iteration: an
// ordinary Go goroutine for cgo and a logical task for the direct path. Both
// report time from C publication until sibling progress. The benchmark time
// also includes sibling creation and the complete call return.
func BenchmarkOrdinaryCgoCallsSteady(b *testing.B) {
	cgobench.CgoCalls(b.N)
}

func BenchmarkDirectCgoCallsSteady(b *testing.B) {
	cgobench.DirectCalls(b.N)
}

func BenchmarkNoBlockCgoCallsSteady(b *testing.B) {
	if got := cgobench.NoBlockCalls(b.N); got != uint64(b.N) {
		b.Fatalf("result = %d, want %d", got, b.N)
	}
}

func BenchmarkOrdinaryCgoCallEntry(b *testing.B) {
	for i := 0; i < b.N; i++ {
		cgobench.CgoCall()
	}
}

func BenchmarkDirectCgoCallEntry(b *testing.B) {
	for i := 0; i < b.N; i++ {
		cgobench.DirectCall()
	}
}

func BenchmarkNoBlockCgoCallEntry(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if got := cgobench.NoBlockCall(); got != 1 {
			b.Fatalf("result = %d, want 1", got)
		}
	}
}

func BenchmarkOrdinaryCgoBlockingHandoff(b *testing.B) {
	benchmarkHandoffs(b,
		cgobench.ResetCgoHandoff,
		cgobench.ReleaseCgoHandoff,
		cgobench.CgoHandoffs)
}

func BenchmarkDirectCgoBlockingHandoff(b *testing.B) {
	benchmarkHandoffs(b,
		cgobench.ResetDirectHandoff,
		cgobench.ReleaseDirectHandoff,
		cgobench.DirectHandoffs)
}

func BenchmarkOrdinaryCgoRunnableHandoff(b *testing.B) {
	benchmarkRunnableHandoffs(b, cgobench.CgoRunnableHandoffs)
}

func BenchmarkDirectCgoRunnableHandoff(b *testing.B) {
	benchmarkRunnableHandoffs(b, cgobench.DirectRunnableHandoffs)
}

func benchmarkHandoffs(b *testing.B, reset func(), release func(uint32),
	handoffs func(int, int) uint64) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)
	reset()
	readFile, writeFile, err := os.Pipe()
	if err != nil {
		b.Fatal(err)
	}
	defer readFile.Close()
	defer writeFile.Close()
	workerErr := make(chan error, 1)
	go func() {
		var token [1]byte
		for epoch := uint32(1); epoch <= uint32(b.N); epoch++ {
			if _, err := io.ReadFull(readFile, token[:]); err != nil {
				workerErr <- err
				return
			}
			release(epoch)
		}
		workerErr <- nil
	}()
	b.ReportAllocs()
	b.ResetTimer()
	elapsed := handoffs(b.N, int(writeFile.Fd()))
	b.StopTimer()
	if elapsed == ^uint64(0) {
		b.Fatal("handoff failed")
	}
	if err := <-workerErr; err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(elapsed)/float64(b.N), "ns/progress")
}

func benchmarkRunnableHandoffs(b *testing.B, handoffs func(int) uint64) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)
	b.ReportAllocs()
	b.ResetTimer()
	elapsed := handoffs(b.N)
	b.StopTimer()
	if elapsed == ^uint64(0) {
		b.Fatal("runnable handoff failed")
	}
	b.ReportMetric(float64(elapsed)/float64(b.N), "ns/progress")
}
