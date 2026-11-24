// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package suitev2

import (
	"encoding/json"
	"flag"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

var probeBenchFlag = flag.String("suitev2.probe-bench", "ALL", "comma-separated benchmark names to probe runtime execution paths for, or ALL")

// TestSuiteV2RuntimePath executes every requested benchmark workload outside
// timing at the current GOMAXPROCS and records the actual hardware execution
// path (native-coro / managed-coro / ordinary-fallback) for both root and
// worker invocations.
func TestSuiteV2RuntimePath(t *testing.T) {
	all := []string{
		"RealIOPipeRoundTrip16",
		"RealIOTCPEcho16",
		"RealIOTCPFanout8",
		"FanoutArgs16",
		"FanoutArgs100",
		"FanoutArgs1000",
		"FanoutCapture16",
		"FanoutCapture100",
		"FanoutCapture1000",
		"FanoutDynamicOpen64",
		"MassivePark10k",
		"MassivePark30k",
		"MassivePark100k",
		"PhaseRootStart",
		"PhaseSpawnComplete100",
		"PhaseSpawnComplete1000",
		"PhaseSpawnYield100",
		"PhaseSpawnYield1000",
		"PhaseParkWake100",
		"PhaseParkWake1000",
		"PhaseChannelPingPong",
		"FallbackOrdinaryHost16",
	}

	var targets []string
	if *probeBenchFlag == "" || *probeBenchFlag == "ALL" {
		targets = all
	} else {
		for _, tok := range strings.Split(*probeBenchFlag, ",") {
			tok = strings.TrimSpace(tok)
			if tok != "" {
				targets = append(targets, tok)
			}
		}
	}

	cpu := runtime.GOMAXPROCS(0)
	cfg := defaultFanoutConfig()

	runtimePathProbeEnabled = true
	defer func() { runtimePathProbeEnabled = false }()

	for _, name := range targets {
		resetRuntimePathProbe()
		switch name {
		case "RealIOPipeRoundTrip16":
			pairs, cleanup := newPipePairs(t, 16)
			latGrid := make([][]int64, 16)
			for i := range latGrid {
				latGrid[i] = make([]int64, 2)
			}
			err := runPipeRoundTrips(pairs, 2, latGrid)
			cleanup()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		case "RealIOTCPEcho16":
			pairs, cleanup := newTCPPairs(t, 16)
			latGrid := make([][]int64, 16)
			for i := range latGrid {
				latGrid[i] = make([]int64, 2)
			}
			err := runTCPEchoRounds(pairs, 2, latGrid)
			cleanup()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		case "RealIOTCPFanout8":
			pairs, cleanup := newTCPPairs(t, 8)
			_, err := runTCPFanoutBatches(pairs, 2, make([]int64, 2))
			cleanup()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		case "FanoutArgs16":
			_ = runFanoutArgs(1, 16, cfg, make([]int64, 1))
		case "FanoutArgs100":
			_ = runFanoutArgs(1, 100, cfg, make([]int64, 1))
		case "FanoutArgs1000":
			_ = runFanoutArgs(1, 1000, cfg, make([]int64, 1))
		case "FanoutCapture16":
			_ = runFanoutCapture(1, 16, cfg, make([]int64, 1))
		case "FanoutCapture100":
			_ = runFanoutCapture(1, 100, cfg, make([]int64, 1))
		case "FanoutCapture1000":
			_ = runFanoutCapture(1, 1000, cfg, make([]int64, 1))
		case "FanoutDynamicOpen64":
			runners := make([]openWorker, 64)
			for i := range runners {
				if i%2 == 0 {
					runners[i] = &openAddWorker{cfg: cfg}
				} else {
					runners[i] = &openXorWorker{cfg: cfg}
				}
			}
			_ = runFanoutDynamicOpen(1, runners, make([]int64, 1))
		case "MassivePark10k", "MassivePark30k", "MassivePark100k":
			const probeTasks = 256
			sess := &massiveParkSession{
				gate:          make(chan struct{}),
				wakeLatencies: make([]int64, probeTasks),
			}
			done := make(chan uint64, 1)
			go func() { done <- runMassivePark(probeTasks, sess) }()
			for sess.ready.Load() < probeTasks {
				runtime.Gosched()
			}
			sess.release.Store(1)
			if got := <-done; got != probeTasks {
				t.Fatalf("%s: completed %d, want %d", name, got, probeTasks)
			}
		case "PhaseRootStart":
			for i := uint64(0); i < 8; i++ {
				_ = phaseRootEntry(i)
			}
		case "PhaseSpawnComplete100":
			_ = runPhaseSpawnComplete(1, 100, make([]int64, 1))
		case "PhaseSpawnComplete1000":
			_ = runPhaseSpawnComplete(1, 1000, make([]int64, 1))
		case "PhaseSpawnYield100":
			_ = runPhaseSpawnYield(1, 100, make([]int64, 1))
		case "PhaseSpawnYield1000":
			_ = runPhaseSpawnYield(1, 1000, make([]int64, 1))
		case "PhaseParkWake100":
			_ = runPhaseParkWake(1, 100, make([]int64, 1))
		case "PhaseParkWake1000":
			_ = runPhaseParkWake(1, 1000, make([]int64, 1))
		case "PhaseChannelPingPong":
			_ = runPhaseChannelPingPong(8)
		case "FallbackOrdinaryHost16":
			_ = runFallbackOrdinaryFanout(1, 16, cfg, make([]int64, 1))
		default:
			t.Fatalf("unknown benchmark probe target: %s", name)
		}

		rep := summarizeRuntimePathProbe(name, cpu)
		if rep.RootSamples == 0 {
			t.Fatalf("%s (P=%d): captured 0 root runtime path samples", name, cpu)
		}
		if name != "PhaseRootStart" && rep.WorkerSamples == 0 {
			t.Fatalf("%s (P=%d): captured 0 worker runtime path samples", name, cpu)
		}
		raw, err := json.Marshal(rep)
		if err != nil {
			t.Fatalf("marshal report: %v", err)
		}
		fmt.Printf("SUITEV2_RUNTIME_PATH %s\n", raw)
	}
}
