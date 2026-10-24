// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package corobench

import (
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
)

func TestPackedWorkloads(t *testing.T) {
	value := 5
	if packedSelf(true, &value, true, &value, true, 32) != 32*17+10 {
		t.Fatal("self")
	}
	if packedMutualA(true, &value, true, &value, true, 32) != 32*17+10 {
		t.Fatal("mutual")
	}
	if packedTasks(2, 17, false) != 2*17*20 {
		t.Fatal("tasks")
	}
	want := 33*(0x7501+0x81+value) + 32*33/2
	if packedBoundary432A(0x7501, 0x81, &value, 32) != want {
		t.Fatal("432")
	}
	if packedBoundary440A(0x7501, 0x81, &value, 32) != want {
		t.Fatal("440")
	}
	if packedBoundary448A(0x7501, 0x81, &value, 32) != want {
		t.Fatal("448")
	}
}
func BenchmarkPackedTasks100000(b *testing.B) {
	b.ReportAllocs()
	if packedTasks(b.N, 100000, false) != uint64(b.N)*100000*20 {
		b.Fatal("tasks")
	}
}

func BenchmarkPackedSelf4096(b *testing.B) {
	value := 5
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		intSink = packedSelf(true, &value, true, &value, true, 4096)
	}
	if intSink != 4096*17+10 {
		b.Fatal("recursive")
	}
}

func BenchmarkPackedMutual4096(b *testing.B) {
	value := 5
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		intSink = packedMutualA(true, &value, true, &value, true, 4096)
	}
	if intSink != 4096*17+10 {
		b.Fatal("recursive")
	}
}

func BenchmarkPackedBoundary432(b *testing.B) {
	value := 5
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		intSink = packedBoundary432A(0x7501, 0x81, &value, 512)
	}
	if intSink != 513*(0x7501+0x81+value)+2*32640 {
		b.Fatal("boundary")
	}
}

func BenchmarkPackedBoundary440(b *testing.B) {
	value := 5
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		intSink = packedBoundary440A(0x7501, 0x81, &value, 512)
	}
	if intSink != 513*(0x7501+0x81+value)+2*32640 {
		b.Fatal("boundary")
	}
}

func BenchmarkPackedBoundary448(b *testing.B) {
	value := 5
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		intSink = packedBoundary448A(0x7501, 0x81, &value, 512)
	}
	if intSink != 513*(0x7501+0x81+value)+2*32640 {
		b.Fatal("boundary")
	}
}
func benchmarkPackedRetention(b *testing.B, kind string) {
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
		case "self":
			intSink = packedSelf(true, &paddingValue, true, &paddingValue, true, 4096)
			want = 4096*17 + 10
		case "mutual":
			intSink = packedMutualA(true, &paddingValue, true, &paddingValue, true, 4096)
			want = 4096*17 + 10
		case "432":
			intSink = packedBoundary432A(0x7501, 0x81, &paddingValue, 512)
			want = 513*(0x7501+0x81+5) + 2*32640
		case "440":
			intSink = packedBoundary440A(0x7501, 0x81, &paddingValue, 512)
			want = 513*(0x7501+0x81+5) + 2*32640
		case "448":
			intSink = packedBoundary448A(0x7501, 0x81, &paddingValue, 512)
			want = 513*(0x7501+0x81+5) + 2*32640
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

func BenchmarkPackedRetainedSelf(b *testing.B) { benchmarkPackedRetention(b, "self") }

func BenchmarkPackedRetainedMutual(b *testing.B) { benchmarkPackedRetention(b, "mutual") }

func BenchmarkPackedRetained432(b *testing.B) { benchmarkPackedRetention(b, "432") }

func BenchmarkPackedRetained440(b *testing.B) { benchmarkPackedRetention(b, "440") }

func BenchmarkPackedRetained448(b *testing.B) { benchmarkPackedRetention(b, "448") }
func benchmarkPackedFootprint(b *testing.B, tasks int) {
	if b.N != 1 {
		b.Skip("requires -benchtime=1x")
	}
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)
	oldGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(oldGC)
	runtime.GC()
	beforeThreads, _ := runtime.ThreadCreateProfile(nil)
	beforeGoroutines := runtime.NumGoroutine()
	var before, live runtime.MemStats
	runtime.ReadMemStats(&before)
	b.ReportAllocs()
	b.ResetTimer()
	atomic.StoreUint64(&packedReady, 0)
	atomic.StoreUint64(&packedRelease, 0)
	done := make(chan uint64, 1)
	go func() { done <- packedTasks(1, tasks, true) }()
	for atomic.LoadUint64(&packedReady) < uint64(tasks) {
		runtime.Gosched()
	}
	runtime.Gosched()
	liveThreads, _ := runtime.ThreadCreateProfile(nil)
	liveGoroutines := runtime.NumGoroutine()
	runtime.ReadMemStats(&live)
	atomic.StoreUint64(&packedRelease, 1)
	completed := <-done
	b.StopTimer()
	if completed != uint64(20*tasks) {
		b.Fatalf("completed %d tasks, want %d", completed, 20*tasks)
	}
	b.ReportMetric(float64(int64(live.HeapAlloc)-int64(before.HeapAlloc))/float64(tasks), "live-heap-B/task")
	b.ReportMetric(float64(int64(live.HeapObjects)-int64(before.HeapObjects))/float64(tasks), "live-objects/task")
	b.ReportMetric(float64(int64(live.StackInuse)-int64(before.StackInuse))/float64(tasks), "live-Go-stack-B/task")
	b.ReportMetric(float64(liveGoroutines-beforeGoroutines), "Go-goroutine-delta")
	b.ReportMetric(float64(liveGoroutines), "Go-goroutines")
	b.ReportMetric(float64(liveThreads-beforeThreads), "thread-record-delta")
	b.ReportMetric(float64(liveThreads), "thread-records")
	b.ReportMetric(float64(tasks), "tasks")
}

func BenchmarkPackedFootprint1(b *testing.B) { benchmarkPackedFootprint(b, 1) }

func BenchmarkPackedFootprint1000(b *testing.B) { benchmarkPackedFootprint(b, 1000) }

func BenchmarkPackedFootprint100000(b *testing.B) { benchmarkPackedFootprint(b, 100000) }
