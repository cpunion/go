// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && (darwin || linux)

package runtime_test

import (
	"context"
	"fmt"
	"internal/testenv"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestStacklessCoroUncachedFusedCompletion(t *testing.T) {
	const helper = "GO_RUNTIME_CORO_FUSED_COMPLETE_LOCKED"
	locked := os.Getenv(helper) == "1"
	for mode := 0; mode < 32; mode++ {
		runnable := mode&1 != 0
		heterogeneous := mode&2 != 0
		fusedParent := mode&4 != 0
		shortChunk := mode&8 != 0
		chunk := mode&16 != 0
		t.Run(fmt.Sprintf("mode%d", mode), func(t *testing.T) {
			if !runtime.CheckStacklessCoroUncachedFusedCompletionForTest(
				runnable, heterogeneous, fusedParent, shortChunk, chunk, locked) {
				t.Fatal("completion changed parent, resume, queue or ownership state")
			}
		})
	}
	if locked {
		return
	}
	t.Run("scheduler-locked", func(t *testing.T) {
		testenv.MustHaveExec(t)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := testenv.CommandContext(t, ctx, os.Args[0],
			"-test.run=^TestStacklessCoroUncachedFusedCompletion$")
		cmd.Env = append(os.Environ(), helper+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("uncached completion acquired the scheduler lock: %v\n%s", err, out)
		}
	})
}
