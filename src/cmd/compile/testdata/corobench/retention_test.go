// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package corobench

import (
	"runtime"
	"testing"
)

type retainedFrameSnapshot struct {
	heapBytes, objects, stackBytes int64
}

func retainedFrameDelta(before, after *runtime.MemStats) retainedFrameSnapshot {
	return retainedFrameSnapshot{
		int64(after.HeapAlloc) - int64(before.HeapAlloc),
		int64(after.HeapObjects) - int64(before.HeapObjects),
		int64(after.StackInuse) - int64(before.StackInuse),
	}
}

func TestRetainedFrameDelta(t *testing.T) {
	before := runtime.MemStats{HeapAlloc: 20, HeapObjects: 4, StackInuse: 40}
	for _, test := range []struct {
		after runtime.MemStats
		want  retainedFrameSnapshot
	}{
		{before, retainedFrameSnapshot{}},
		{runtime.MemStats{HeapAlloc: 25, HeapObjects: 6, StackInuse: 48}, retainedFrameSnapshot{5, 2, 8}},
		{runtime.MemStats{HeapAlloc: 15, HeapObjects: 3, StackInuse: 32}, retainedFrameSnapshot{-5, -1, -8}},
	} {
		if got := retainedFrameDelta(&before, &test.after); got != test.want {
			t.Errorf("retainedFrameDelta = %+v, want %+v", got, test.want)
		}
	}
}

// benchmarkRetainedFrames measures completed-call retention, not throughput.
// Each probe needs a fresh process; prior frame shapes change cache admission.
func benchmarkRetainedFrames(b *testing.B, kind string) {
	if b.N != 1 {
		b.Skip("retention snapshots require -benchtime=1x")
	}
	// Keep the measurement controller ordinary, as in the footprint probes.
	for range 1 {
		oldProcs := runtime.GOMAXPROCS(1)
		defer runtime.GOMAXPROCS(oldProcs)
		intSink = yieldEntry()
		runtime.GC()
		runtime.GC()
		var before runtime.MemStats
		var samples [4]runtime.MemStats
		runtime.ReadMemStats(&before)
		want := 4097
		switch kind {
		case "control":
			intSink, want = 1, 1
		case "self":
			intSink = recursiveYield(4096)
		case "mutual":
			intSink = mutualYieldA(4096)
		case "large":
			intSink, want = recursiveLargeFrame(512), 769
		default:
			b.Fatal("unknown retention probe")
		}
		if intSink != want {
			b.Fatal("incorrect recursive result")
		}
		runtime.ReadMemStats(&samples[0])
		for cycle := 1; cycle <= 4; cycle++ {
			runtime.GC()
			switch cycle {
			case 1:
				runtime.ReadMemStats(&samples[1])
			case 2:
				runtime.ReadMemStats(&samples[2])
			case 4:
				runtime.ReadMemStats(&samples[3])
			}
		}
		// Finish every snapshot before ReportMetric can allocate maps or labels.
		for i, stage := range [...]string{"returned", "gc1", "gc2", "gc4"} {
			delta := retainedFrameDelta(&before, &samples[i])
			b.ReportMetric(float64(delta.heapBytes), stage+"-heap-B")
			b.ReportMetric(float64(delta.objects), stage+"-objects")
			b.ReportMetric(float64(delta.stackBytes), stage+"-Go-stack-B")
		}
		b.ReportMetric(float64(samples[0].TotalAlloc-before.TotalAlloc), "call-total-B")
		b.ReportMetric(float64(samples[0].Mallocs-before.Mallocs), "call-allocs")
	}
}

func BenchmarkRetainedControl(b *testing.B)    { benchmarkRetainedFrames(b, "control") }
func BenchmarkRetainedSelf4096(b *testing.B)   { benchmarkRetainedFrames(b, "self") }
func BenchmarkRetainedMutual4096(b *testing.B) { benchmarkRetainedFrames(b, "mutual") }
func BenchmarkRetainedLarge512(b *testing.B)   { benchmarkRetainedFrames(b, "large") }
