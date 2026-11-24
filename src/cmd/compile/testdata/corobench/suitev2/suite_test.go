// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package suitev2

import (
	"runtime"
	"testing"
)

func TestSuiteV2Probes(t *testing.T) {
	if live, peak := processRSS(); live == 0 || peak == 0 {
		t.Fatalf("processRSS() = (%d, %d), want positive values", live, peak)
	}

	pairs, cleanup := newPipePairs(t, 4)
	latGrid := make([][]int64, 4)
	for i := range latGrid {
		latGrid[i] = make([]int64, 3)
	}
	if err := runPipeRoundTrips(pairs, 3, latGrid); err != nil {
		cleanup()
		t.Fatalf("runPipeRoundTrips: %v", err)
	}
	cleanup()

	tcpPairs, tcpCleanup := newTCPPairs(t, 4)
	if err := runTCPEchoRounds(tcpPairs, 3, latGrid); err != nil {
		tcpCleanup()
		t.Fatalf("runTCPEchoRounds: %v", err)
	}
	tcpCleanup()

	fanPairs, fanCleanup := newTCPPairs(t, 4)
	batchLats := make([]int64, 3)
	if sum, err := runTCPFanoutBatches(fanPairs, 3, batchLats); err != nil || sum == 0 {
		fanCleanup()
		t.Fatalf("runTCPFanoutBatches = (%d, %v)", sum, err)
	}
	fanCleanup()

	cfg := defaultFanoutConfig()
	if got := runFanoutArgs(2, 8, cfg, make([]int64, 2)); got == 0 {
		t.Fatal("runFanoutArgs returned zero")
	}
	if got := runFanoutCapture(2, 8, cfg, make([]int64, 2)); got == 0 {
		t.Fatal("runFanoutCapture returned zero")
	}
	runners := []openWorker{&openAddWorker{cfg: cfg}, &openXorWorker{cfg: cfg}}
	if got := runFanoutDynamicOpen(2, runners, make([]int64, 2)); got == 0 {
		t.Fatal("runFanoutDynamicOpen returned zero")
	}
	if got := runFallbackOrdinaryFanout(2, 4, cfg, make([]int64, 2)); got == 0 {
		t.Fatal("runFallbackOrdinaryFanout returned zero")
	}

	sess := &massiveParkSession{
		gate:          make(chan struct{}),
		wakeLatencies: make([]int64, 32),
	}
	done := make(chan uint64, 1)
	go func() { done <- runMassivePark(32, sess) }()
	for sess.ready.Load() < 32 {
		runtime.Gosched()
	}
	sess.release.Store(1)
	if got := <-done; got != 32 {
		t.Fatalf("runMassivePark(32) = %d, want 32", got)
	}

	if got := phaseRootEntry(41); got != 42 {
		t.Fatalf("phaseRootEntry(41) = %d, want 42", got)
	}
	if got := runPhaseSpawnComplete(2, 16, make([]int64, 2)); got != 32 {
		t.Fatalf("runPhaseSpawnComplete = %d, want 32", got)
	}
	if got := runPhaseSpawnYield(2, 16, make([]int64, 2)); got != 32 {
		t.Fatalf("runPhaseSpawnYield = %d, want 32", got)
	}
	if got := runPhaseParkWake(2, 16, make([]int64, 2)); got != 32 {
		t.Fatalf("runPhaseParkWake = %d, want 32", got)
	}
	if got := runPhaseChannelPingPong(4); got != 10 {
		t.Fatalf("runPhaseChannelPingPong(4) = %d, want 10", got)
	}
}
