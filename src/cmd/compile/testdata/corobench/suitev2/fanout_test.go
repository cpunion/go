// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package suitev2

import (
	"runtime"
	"testing"
	"time"
)

type fanoutConfig struct {
	salt    uint64
	rounds  int
	weights []uint64
}

type fanoutResult struct {
	id       int
	checksum uint64
}

func mixWork(seed uint64, rounds int, weights []uint64) uint64 {
	v := seed
	n := len(weights)
	for i := 0; i < rounds; i++ {
		v = v*6364136223846793005 + weights[i%n]
	}
	return v
}

func fanoutArgsWorker(id int, cfg *fanoutConfig, results chan fanoutResult) {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleWorker)
	}
	v := mixWork(cfg.salt^uint64(id+1), cfg.rounds, cfg.weights)
	runtime.Gosched()
	v = mixWork(v^uint64(id+17), cfg.rounds, cfg.weights)
	results <- fanoutResult{id: id, checksum: v}
}

func runFanoutArgs(batches, workers int, cfg *fanoutConfig, latencies []int64) uint64 {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleRoot)
	}
	results := make(chan fanoutResult, workers)
	var total uint64
	for b := 0; b < batches; b++ {
		start := time.Now()
		for i := 0; i < workers; i++ {
			go fanoutArgsWorker(i, cfg, results)
		}
		for i := 0; i < workers; i++ {
			r := <-results
			total += r.checksum ^ uint64(r.id)
		}
		latencies[b] = time.Since(start).Nanoseconds()
	}
	return total
}

func runFanoutCapture(batches, workers int, cfg *fanoutConfig, latencies []int64) uint64 {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleRoot)
	}
	results := make(chan fanoutResult, workers)
	weights := cfg.weights
	salt := cfg.salt
	rounds := cfg.rounds
	var total uint64
	for b := 0; b < batches; b++ {
		start := time.Now()
		for i := 0; i < workers; i++ {
			id := i
			seed := salt ^ uint64(b+1) ^ uint64(id+1)
			go func() {
				if runtimePathProbeEnabled {
					recordRuntimePathProbe(probeRoleWorker)
				}
				v := mixWork(seed, rounds, weights)
				runtime.Gosched()
				v = mixWork(v^uint64(id+31), rounds, weights)
				results <- fanoutResult{id: id, checksum: v}
			}()
		}
		for i := 0; i < workers; i++ {
			r := <-results
			total += r.checksum ^ uint64(r.id)
		}
		latencies[b] = time.Since(start).Nanoseconds()
	}
	return total
}

type openWorker interface {
	Run(id int, out chan fanoutResult)
}

type openAddWorker struct {
	cfg *fanoutConfig
}

func (w *openAddWorker) Run(id int, out chan fanoutResult) {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleWorker)
	}
	v := mixWork(w.cfg.salt+uint64(id+1), w.cfg.rounds, w.cfg.weights)
	runtime.Gosched()
	out <- fanoutResult{id: id, checksum: v}
}

type openXorWorker struct {
	cfg *fanoutConfig
}

func (w *openXorWorker) Run(id int, out chan fanoutResult) {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleWorker)
	}
	v := mixWork(w.cfg.salt^uint64(id+7), w.cfg.rounds, w.cfg.weights)
	runtime.Gosched()
	out <- fanoutResult{id: id, checksum: v ^ 0x9e3779b97f4a7c15}
}

func runFanoutDynamicOpen(batches int, runners []openWorker, latencies []int64) uint64 {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleRoot)
	}
	workers := len(runners)
	results := make(chan fanoutResult, workers)
	var total uint64
	for b := 0; b < batches; b++ {
		start := time.Now()
		for i := 0; i < workers; i++ {
			runner := runners[i]
			go runner.Run(i, results)
		}
		for i := 0; i < workers; i++ {
			r := <-results
			total += r.checksum ^ uint64(r.id)
		}
		latencies[b] = time.Since(start).Nanoseconds()
	}
	return total
}

// fallbackOrdinaryWorker calls an ordinary cgo wrapper without #cgo nocallback,
// forcing ordinary Go-stack fallback. The harness segregates this benchmark
// into the ordinary-fallback table to verify automatic path classification.
func fallbackOrdinaryWorker(id int, cfg *fanoutConfig, results chan fanoutResult) {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleWorker)
	}
	v := ordinaryCgoFallbackOp(cfg.salt ^ uint64(id+1))
	v = mixWork(v, cfg.rounds, cfg.weights)
	runtime.Gosched()
	results <- fanoutResult{id: id, checksum: v}
}

func runFallbackOrdinaryFanout(batches, workers int, cfg *fanoutConfig, latencies []int64) uint64 {
	if runtimePathProbeEnabled {
		recordRuntimePathProbe(probeRoleRoot)
	}
	results := make(chan fanoutResult, workers)
	var total uint64
	for b := 0; b < batches; b++ {
		start := time.Now()
		for i := 0; i < workers; i++ {
			go fallbackOrdinaryWorker(i, cfg, results)
		}
		for i := 0; i < workers; i++ {
			r := <-results
			total += r.checksum ^ uint64(r.id)
		}
		latencies[b] = time.Since(start).Nanoseconds()
	}
	return total
}

func defaultFanoutConfig() *fanoutConfig {
	return &fanoutConfig{
		salt:    0x243f6a8885a308d3,
		rounds:  16,
		weights: []uint64{1442695040888963407, 2862933555777941757, 3202034522624059733, 3935559000370003845},
	}
}

func benchmarkFanoutArgs(b *testing.B, workers int) {
	cfg := defaultFanoutConfig()
	latencies := make([]int64, b.N)
	rssBefore, _ := processRSS()
	b.ReportAllocs()
	b.ResetTimer()
	sinkUint64 = runFanoutArgs(b.N, workers, cfg, latencies)
	b.StopTimer()
	rssAfter, rssPeak := processRSS()
	reportBenchMetrics(b, latencies, workers*b.N, rssBefore, rssAfter, rssPeak)
}

func benchmarkFanoutCapture(b *testing.B, workers int) {
	cfg := defaultFanoutConfig()
	latencies := make([]int64, b.N)
	rssBefore, _ := processRSS()
	b.ReportAllocs()
	b.ResetTimer()
	sinkUint64 = runFanoutCapture(b.N, workers, cfg, latencies)
	b.StopTimer()
	rssAfter, rssPeak := processRSS()
	reportBenchMetrics(b, latencies, workers*b.N, rssBefore, rssAfter, rssPeak)
}

func BenchmarkFanoutArgs16(b *testing.B)      { benchmarkFanoutArgs(b, 16) }
func BenchmarkFanoutArgs100(b *testing.B)     { benchmarkFanoutArgs(b, 100) }
func BenchmarkFanoutArgs1000(b *testing.B)    { benchmarkFanoutArgs(b, 1000) }
func BenchmarkFanoutCapture16(b *testing.B)   { benchmarkFanoutCapture(b, 16) }
func BenchmarkFanoutCapture100(b *testing.B)  { benchmarkFanoutCapture(b, 100) }
func BenchmarkFanoutCapture1000(b *testing.B) { benchmarkFanoutCapture(b, 1000) }

func BenchmarkFanoutDynamicOpen64(b *testing.B) {
	cfg := defaultFanoutConfig()
	runners := make([]openWorker, 64)
	for i := range runners {
		if i%2 == 0 {
			runners[i] = &openAddWorker{cfg: cfg}
		} else {
			runners[i] = &openXorWorker{cfg: cfg}
		}
	}
	latencies := make([]int64, b.N)
	rssBefore, _ := processRSS()
	b.ReportAllocs()
	b.ResetTimer()
	sinkUint64 = runFanoutDynamicOpen(b.N, runners, latencies)
	b.StopTimer()
	rssAfter, rssPeak := processRSS()
	reportBenchMetrics(b, latencies, len(runners)*b.N, rssBefore, rssAfter, rssPeak)
}

func BenchmarkFallbackOrdinaryHost16(b *testing.B) {
	cfg := defaultFanoutConfig()
	latencies := make([]int64, b.N)
	rssBefore, _ := processRSS()
	b.ReportAllocs()
	b.ResetTimer()
	sinkUint64 = runFallbackOrdinaryFanout(b.N, 16, cfg, latencies)
	b.StopTimer()
	rssAfter, rssPeak := processRSS()
	reportBenchMetrics(b, latencies, 16*b.N, rssBefore, rssAfter, rssPeak)
}
