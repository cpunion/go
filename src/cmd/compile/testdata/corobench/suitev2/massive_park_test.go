// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package suitev2

import (
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"
)

type parkPayload struct {
	id   uint64
	tag  uint64
	data [2]uint64
}

type massiveParkSession struct {
	gate          chan struct{}
	ready         atomic.Uint64
	done          atomic.Uint64
	release       atomic.Uint32
	releaseStart  atomic.Int64
	wakeLatencies []int64
}

func massiveParkWorker(id int, payload *parkPayload, sess *massiveParkSession) {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleWorker)
	}
	sess.ready.Add(1)
	<-sess.gate
	elapsed := time.Now().UnixNano() - sess.releaseStart.Load()
	if elapsed < 0 {
		elapsed = 0
	}
	sess.wakeLatencies[id] = elapsed + int64(payload.id&0)
	sess.done.Add(1)
}

func runMassivePark(tasks int, sess *massiveParkSession) uint64 {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleRoot)
	}
	payloads := make([]parkPayload, tasks)
	for i := 0; i < tasks; i++ {
		payloads[i] = parkPayload{id: uint64(i), tag: uint64(i * 31)}
		go massiveParkWorker(i, &payloads[i], sess)
	}
	for sess.ready.Load() < uint64(tasks) {
		runtime.Gosched()
	}
	for sess.release.Load() == 0 {
		runtime.Gosched()
	}
	sess.releaseStart.Store(time.Now().UnixNano())
	close(sess.gate)
	for sess.done.Load() < uint64(tasks) {
		runtime.Gosched()
	}
	return sess.done.Load()
}

// benchmarkMassivePark measures massive simultaneous suspension in a single
// fresh process (-benchtime=1x): spawn-to-park rate, parked Heap/Go-stack/OS-RSS,
// wake tail latency (p50/p95/p99), and GC/FreeOSMemory reclamation behavior.
func benchmarkMassivePark(b *testing.B, tasks int) {
	if b.N != 1 {
		b.Skip("requires -benchtime=1x")
	}
	runtime.GC()
	runtime.GC()

	var beforeMem, parkedMem, drainedMem, gc1Mem, gc2Mem, gc4Mem runtime.MemStats
	runtime.ReadMemStats(&beforeMem)
	rssBefore, _ := processRSS()
	beforeGoroutines := runtime.NumGoroutine()
	beforeUnhosted := unhostedCoroCount()

	sess := &massiveParkSession{
		gate:          make(chan struct{}),
		wakeLatencies: make([]int64, tasks),
	}

	b.ReportAllocs()
	b.ResetTimer()
	spawnStart := time.Now()
	sessionDone := make(chan uint64, 1)
	go func() {
		sessionDone <- runMassivePark(tasks, sess)
	}()
	for sess.ready.Load() < uint64(tasks) {
		runtime.Gosched()
	}
	runtime.Gosched()
	spawnToPark := time.Since(spawnStart)

	runtime.ReadMemStats(&parkedMem)
	rssParked, rssPeakParked := processRSS()
	parkedGoroutines := runtime.NumGoroutine()
	parkedUnhosted := unhostedCoroCount()

	releaseWallStart := time.Now()
	sess.release.Store(1)
	completed := <-sessionDone
	releaseToDrain := time.Since(releaseWallStart)
	b.StopTimer()

	if completed != uint64(tasks) {
		b.Fatalf("completed %d tasks, want %d", completed, tasks)
	}

	runtime.ReadMemStats(&drainedMem)
	rssDrained, rssPeakDrained := processRSS()

	// Measure reclamation behavior across GC cycles + FreeOSMemory.
	// Clear latency slice reference only after copying percentiles.
	p50 := percentileNS(sess.wakeLatencies, 0.50)
	p95 := percentileNS(sess.wakeLatencies, 0.95)
	p99 := percentileNS(sess.wakeLatencies, 0.99)
	sess.wakeLatencies = nil
	sess = nil

	runtime.GC()
	runtime.ReadMemStats(&gc1Mem)
	rssGC1, _ := processRSS()

	runtime.GC()
	runtime.ReadMemStats(&gc2Mem)

	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&gc4Mem)
	rssGC4, _ := processRSS()

	debug.FreeOSMemory()
	rssFreeOS, rssPeakFinal := processRSS()
	if rssPeakFinal < rssPeakDrained {
		rssPeakFinal = rssPeakDrained
	}
	if rssPeakFinal < rssPeakParked {
		rssPeakFinal = rssPeakParked
	}

	parkedHeapDelta := int64(parkedMem.HeapAlloc) - int64(beforeMem.HeapAlloc)
	parkedRSSDelta := int64(rssParked) - int64(rssBefore)
	gc4HeapDelta := int64(gc4Mem.HeapAlloc) - int64(beforeMem.HeapAlloc)
	retainedPct := 0.0
	if parkedHeapDelta > 0 {
		retainedPct = 100.0 * float64(gc4HeapDelta) / float64(parkedHeapDelta)
	}

	b.ReportMetric(float64(tasks), "tasks")
	b.ReportMetric(float64(spawnToPark.Nanoseconds())/float64(tasks), "spawn-park-ns/task")
	b.ReportMetric(float64(releaseToDrain.Nanoseconds())/float64(tasks), "wake-drain-ns/task")
	if totalSec := (spawnToPark + releaseToDrain).Seconds(); totalSec > 0 {
		b.ReportMetric(float64(tasks)/totalSec, "ops/s")
	}
	b.ReportMetric(p50, "p50-ns")
	b.ReportMetric(p95, "p95-ns")
	b.ReportMetric(p99, "p99-ns")

	b.ReportMetric(float64(parkedHeapDelta)/float64(tasks), "parked-heap-B/task")
	b.ReportMetric(float64(int64(parkedMem.HeapObjects)-int64(beforeMem.HeapObjects))/float64(tasks), "parked-objects/task")
	b.ReportMetric(float64(int64(parkedMem.StackInuse)-int64(beforeMem.StackInuse))/float64(tasks), "parked-Go-stack-B/task")
	b.ReportMetric(float64(rssParked), "rss-live-B")
	b.ReportMetric(float64(parkedRSSDelta), "rss-delta-B")
	b.ReportMetric(float64(parkedRSSDelta)/float64(tasks), "parked-rss-B/task")
	b.ReportMetric(float64(rssPeakFinal), "rss-peak-B")

	b.ReportMetric(float64(parkedGoroutines-beforeGoroutines), "go-goroutine-delta")
	b.ReportMetric(float64(int64(parkedUnhosted)-int64(beforeUnhosted))/float64(tasks), "unhosted-coro/task")

	b.ReportMetric(float64(int64(drainedMem.HeapAlloc)-int64(beforeMem.HeapAlloc)), "drained-heap-delta-B")
	b.ReportMetric(float64(int64(rssDrained)-int64(rssBefore)), "drained-rss-delta-B")
	b.ReportMetric(float64(int64(gc1Mem.HeapAlloc)-int64(beforeMem.HeapAlloc)), "gc1-heap-delta-B")
	b.ReportMetric(float64(int64(rssGC1)-int64(rssBefore)), "gc1-rss-delta-B")
	b.ReportMetric(float64(int64(gc2Mem.HeapAlloc)-int64(beforeMem.HeapAlloc)), "gc2-heap-delta-B")
	b.ReportMetric(float64(gc4HeapDelta), "gc4-heap-delta-B")
	b.ReportMetric(float64(int64(rssGC4)-int64(rssBefore)), "gc4-rss-delta-B")
	b.ReportMetric(float64(int64(rssFreeOS)-int64(rssBefore)), "freeos-rss-delta-B")
	b.ReportMetric(retainedPct, "gc4-retained-pct")
}

func BenchmarkMassivePark10k(b *testing.B)  { benchmarkMassivePark(b, 10000) }
func BenchmarkMassivePark30k(b *testing.B)  { benchmarkMassivePark(b, 30000) }
func BenchmarkMassivePark100k(b *testing.B) { benchmarkMassivePark(b, 100000) }
