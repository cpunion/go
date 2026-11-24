// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package suitev2

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// Phase attribution probes isolate the major runtime cost centers on e277e2c6e3:
//   1. Root entry/teardown (PhaseRootStart)
//   2. Spawn + source registration + ready queue + recycle (PhaseSpawnComplete)
//   3. Suspend + re-enqueue + resume dispatch delta (PhaseSpawnYield - PhaseSpawnComplete)
//   4. Channel waiter registration + wake broadcast + drain (PhaseParkWake - PhaseSpawnComplete)
//   5. Steady-state channel wake/handoff without root start/teardown (PhaseChannelPingPong)

func phaseRootEntry(x uint64) uint64 {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleRoot)
	}
	runtime.Gosched()
	return x + 1
}

var neverYield bool

func phaseNoWorkWorker(done *atomic.Uint64) {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleWorker)
	}
	if neverYield {
		runtime.Gosched()
	}
	done.Add(1)
}

func runPhaseSpawnComplete(rounds, tasks int, latencies []int64) uint64 {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleRoot)
	}
	var total uint64
	var done atomic.Uint64
	for r := 0; r < rounds; r++ {
		done.Store(0)
		start := time.Now()
		for i := 0; i < tasks; i++ {
			go phaseNoWorkWorker(&done)
		}
		for done.Load() < uint64(tasks) {
			runtime.Gosched()
		}
		latencies[r] = time.Since(start).Nanoseconds()
		total += done.Load()
	}
	return total
}

func phaseYieldWorker(done *atomic.Uint64) {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleWorker)
	}
	runtime.Gosched()
	done.Add(1)
}

func runPhaseSpawnYield(rounds, tasks int, latencies []int64) uint64 {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleRoot)
	}
	var total uint64
	var done atomic.Uint64
	for r := 0; r < rounds; r++ {
		done.Store(0)
		start := time.Now()
		for i := 0; i < tasks; i++ {
			go phaseYieldWorker(&done)
		}
		for done.Load() < uint64(tasks) {
			runtime.Gosched()
		}
		latencies[r] = time.Since(start).Nanoseconds()
		total += done.Load()
	}
	return total
}

func phaseParkWorker(gate chan struct{}, ready, done *atomic.Uint64) {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleWorker)
	}
	ready.Add(1)
	<-gate
	done.Add(1)
}

func runPhaseParkWake(rounds, tasks int, latencies []int64) uint64 {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleRoot)
	}
	var total uint64
	var ready, done atomic.Uint64
	for r := 0; r < rounds; r++ {
		gate := make(chan struct{})
		ready.Store(0)
		done.Store(0)
		start := time.Now()
		for i := 0; i < tasks; i++ {
			go phaseParkWorker(gate, &ready, &done)
		}
		for ready.Load() < uint64(tasks) {
			runtime.Gosched()
		}
		close(gate)
		for done.Load() < uint64(tasks) {
			runtime.Gosched()
		}
		latencies[r] = time.Since(start).Nanoseconds()
		total += done.Load()
	}
	return total
}

func phasePingPongWorker(ping, pong chan int, iterations int) {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleWorker)
	}
	for i := 0; i < iterations; i++ {
		v := <-ping
		pong <- v + 1
	}
}

func runPhaseChannelPingPong(iterations int) int {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleRoot)
	}
	ping := make(chan int)
	pong := make(chan int)
	go phasePingPongWorker(ping, pong, iterations)
	sum := 0
	for i := 0; i < iterations; i++ {
		ping <- i
		sum += <-pong
	}
	return sum
}

func BenchmarkPhaseRootStart(b *testing.B) {
	rssBefore, _ := processRSS()
	b.ReportAllocs()
	b.ResetTimer()
	var sum uint64
	for i := 0; i < b.N; i++ {
		sum += phaseRootEntry(uint64(i))
	}
	b.StopTimer()
	sinkUint64 = sum
	rssAfter, rssPeak := processRSS()
	reportBenchMetrics(b, nil, b.N, rssBefore, rssAfter, rssPeak)
}

func BenchmarkPhaseSpawnComplete100(b *testing.B) {
	latencies := make([]int64, b.N)
	rssBefore, _ := processRSS()
	b.ReportAllocs()
	b.ResetTimer()
	sinkUint64 = runPhaseSpawnComplete(b.N, 100, latencies)
	b.StopTimer()
	rssAfter, rssPeak := processRSS()
	reportBenchMetrics(b, latencies, 100*b.N, rssBefore, rssAfter, rssPeak)
}

func BenchmarkPhaseSpawnComplete1000(b *testing.B) {
	latencies := make([]int64, b.N)
	rssBefore, _ := processRSS()
	b.ReportAllocs()
	b.ResetTimer()
	sinkUint64 = runPhaseSpawnComplete(b.N, 1000, latencies)
	b.StopTimer()
	rssAfter, rssPeak := processRSS()
	reportBenchMetrics(b, latencies, 1000*b.N, rssBefore, rssAfter, rssPeak)
}

func BenchmarkPhaseSpawnYield100(b *testing.B) {
	latencies := make([]int64, b.N)
	rssBefore, _ := processRSS()
	b.ReportAllocs()
	b.ResetTimer()
	sinkUint64 = runPhaseSpawnYield(b.N, 100, latencies)
	b.StopTimer()
	rssAfter, rssPeak := processRSS()
	reportBenchMetrics(b, latencies, 100*b.N, rssBefore, rssAfter, rssPeak)
}

func BenchmarkPhaseSpawnYield1000(b *testing.B) {
	latencies := make([]int64, b.N)
	rssBefore, _ := processRSS()
	b.ReportAllocs()
	b.ResetTimer()
	sinkUint64 = runPhaseSpawnYield(b.N, 1000, latencies)
	b.StopTimer()
	rssAfter, rssPeak := processRSS()
	reportBenchMetrics(b, latencies, 1000*b.N, rssBefore, rssAfter, rssPeak)
}

func BenchmarkPhaseParkWake100(b *testing.B) {
	latencies := make([]int64, b.N)
	rssBefore, _ := processRSS()
	b.ReportAllocs()
	b.ResetTimer()
	sinkUint64 = runPhaseParkWake(b.N, 100, latencies)
	b.StopTimer()
	rssAfter, rssPeak := processRSS()
	reportBenchMetrics(b, latencies, 100*b.N, rssBefore, rssAfter, rssPeak)
}

func BenchmarkPhaseParkWake1000(b *testing.B) {
	latencies := make([]int64, b.N)
	rssBefore, _ := processRSS()
	b.ReportAllocs()
	b.ResetTimer()
	sinkUint64 = runPhaseParkWake(b.N, 1000, latencies)
	b.StopTimer()
	rssAfter, rssPeak := processRSS()
	reportBenchMetrics(b, latencies, 1000*b.N, rssBefore, rssAfter, rssPeak)
}

func BenchmarkPhaseChannelPingPong(b *testing.B) {
	rssBefore, _ := processRSS()
	b.ReportAllocs()
	b.ResetTimer()
	sinkInt = runPhaseChannelPingPong(b.N)
	b.StopTimer()
	rssAfter, rssPeak := processRSS()
	reportBenchMetrics(b, nil, b.N, rssBefore, rssAfter, rssPeak)
}
