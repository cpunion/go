// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package corobench

import (
	"runtime"
	"testing"
)

func BenchmarkMutualAfterSelf4096(b *testing.B) {
	if recursiveYield(4096) != 4097 {
		b.Fatal("self prelude")
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		intSink = mutualYieldA(4096)
	}
	if intSink != 4097 {
		b.Fatal("mutual result")
	}
}

func BenchmarkSelfAfterMutual4096(b *testing.B) {
	if mutualYieldA(4096) != 4097 {
		b.Fatal("mutual prelude")
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		intSink = recursiveYield(4096)
	}
	if intSink != 4097 {
		b.Fatal("self result")
	}
}

// Include cache repopulation after idle collections, separately from the
// allocation-free warm path. GC cost is part of this benchmark's workload.
func BenchmarkSelfAfterIdleGC(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		runtime.GC()
		runtime.GC()
		intSink = recursiveYield(4096)
	}
	if intSink != 4097 {
		b.Fatal("self after GC")
	}
}

func TestCachePhaseReuse(t *testing.T) {
	for round := 0; round < 4; round++ {
		if recursiveYield(4096) != 4097 {
			t.Fatal("self")
		}
		for i := 0; i < 4; i++ {
			if mutualYieldA(4096) != 4097 {
				t.Fatal("mutual phase")
			}
		}
		if recursiveYield(4096) != 4097 {
			t.Fatal("self phase")
		}
		runtime.GC()
		runtime.GC()
		if mutualYieldA(4096) != 4097 {
			t.Fatal("mutual after idle GC")
		}
		if recursiveLargeFrame(512) != 769 {
			t.Fatal("large after GC")
		}
	}
}
