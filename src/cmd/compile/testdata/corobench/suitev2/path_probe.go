// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package suitev2

import (
	"corobench.internal/suitev2/fpprobe"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
)

const (
	probeRoleRoot   uint8 = 1
	probeRoleWorker uint8 = 2

	maxProbeFrames  = 16
	maxProbeSamples = 256
)

// runtimePathProbeEnabled is false during all timed benchmark runs (single
// predicted-not-taken branch). TestSuiteV2RuntimePath sets it to true outside
// timing to capture actual hardware frame stacks on every benchmark and P.
var runtimePathProbeEnabled bool

type rawPathProbeSample struct {
	role uint8
	n    uint8
	pcs  [maxProbeFrames]uintptr
}

var (
	pathProbeCursor  atomic.Uint32
	pathProbeSamples [maxProbeSamples]rawPathProbeSample
)

func resetRuntimePathProbe() {
	pathProbeCursor.Store(0)
	for i := range pathProbeSamples {
		pathProbeSamples[i] = rawPathProbeSample{}
	}
}

// recordRuntimePathProbe is lock-free, allocation-free, and uses no runtime
// calls that could perturb coroutine lowering or fixed-stack admission.
//
//go:noinline
func recordRuntimePathProbe(role uint8) {
	idx := pathProbeCursor.Add(1) - 1
	if idx >= maxProbeSamples {
		return
	}
	s := &pathProbeSamples[idx]
	s.role = role
	n := fpprobe.CaptureHardwareFrames(&s.pcs[0], maxProbeFrames)
	if n < 0 {
		n = 0
	}
	if n > maxProbeFrames {
		n = maxProbeFrames
	}
	s.n = uint8(n)
}

func classifyHardwareFrameNames(names []string) string {
	hasNative := false
	hasScheduler := false
	hasCoroFunc := false
	for _, name := range names {
		if strings.Contains(name, "runtime.coroNativeMain") ||
			strings.Contains(name, "runtime.coroNativeStart") ||
			strings.Contains(name, "runtime.(*stacklessCoroNativeDriver).") {
			hasNative = true
		}
		if strings.Contains(name, "runtime.(*stacklessCoroScheduler).") ||
			strings.Contains(name, "runtime.coroRunFrame") ||
			strings.Contains(name, "runtime.coroRun") {
			hasScheduler = true
		}
		if strings.Contains(name, ".coro.func") {
			hasCoroFunc = true
		}
	}
	if hasNative {
		return "native-coro"
	}
	if hasScheduler || hasCoroFunc {
		return "managed-coro"
	}
	return "ordinary-fallback"
}

type runtimePathReport struct {
	Benchmark     string              `json:"benchmark"`
	CPU           int                 `json:"cpu"`
	RootPaths     []string            `json:"root_paths"`
	WorkerPaths   []string            `json:"worker_paths"`
	ObservedPaths []string            `json:"observed_paths"`
	RootSamples   int                 `json:"root_samples"`
	WorkerSamples int                 `json:"worker_samples"`
	SampleFrames  map[string][]string `json:"sample_frames"`
}

func summarizeRuntimePathProbe(bench string, cpu int) runtimePathReport {
	count := int(pathProbeCursor.Load())
	if count > maxProbeSamples {
		count = maxProbeSamples
	}
	rootSet := make(map[string]struct{})
	workerSet := make(map[string]struct{})
	allSet := make(map[string]struct{})
	sampleFrames := make(map[string][]string)
	rootSamples := 0
	workerSamples := 0

	for i := 0; i < count; i++ {
		s := &pathProbeSamples[i]
		if s.n == 0 {
			continue
		}
		names := make([]string, 0, int(s.n))
		for j := 0; j < int(s.n); j++ {
			if fn := runtime.FuncForPC(s.pcs[j]); fn != nil {
				names = append(names, fn.Name())
			}
		}
		cls := classifyHardwareFrameNames(names)
		allSet[cls] = struct{}{}
		roleName := "root"
		if s.role == probeRoleRoot {
			rootSet[cls] = struct{}{}
			rootSamples++
		} else {
			roleName = "worker"
			workerSet[cls] = struct{}{}
			workerSamples++
		}
		key := roleName + ":" + cls
		if _, exists := sampleFrames[key]; !exists {
			sampleFrames[key] = names
		}
	}

	sortedKeys := func(m map[string]struct{}) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}

	return runtimePathReport{
		Benchmark:     bench,
		CPU:           cpu,
		RootPaths:     sortedKeys(rootSet),
		WorkerPaths:   sortedKeys(workerSet),
		ObservedPaths: sortedKeys(allSet),
		RootSamples:   rootSamples,
		WorkerSamples: workerSamples,
		SampleFrames:  sampleFrames,
	}
}
