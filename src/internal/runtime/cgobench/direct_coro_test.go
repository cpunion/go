// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package cgobench_test

import (
	"internal/runtime/cgobench"
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
}

// Run these benchmarks with:
//
//	GOEXPERIMENT=coro go test internal/runtime/cgobench \
//		-run=^$ -bench='Cgo(CallsSteady|CallEntry)$' \
//		-gcflags=internal/runtime/cgobench='-l -d=coro=4'
//
// The Steady benchmarks batch calls within one coroutine root and isolate the
// foreign boundary. The Entry benchmarks include root setup on every call.
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
