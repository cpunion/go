// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && !coropullcompare && (darwin || linux)

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

func TestStacklessCoroLocalFusedRequest(t *testing.T) {
	const helper = "GO_RUNTIME_CORO_FUSED_REQUEST_LOCKED"
	locked := os.Getenv(helper) == "1"
	for mode := 0; mode < 8; mode++ {
		t.Run(fmt.Sprintf("mode%d", mode), func(t *testing.T) {
			if !runtime.CheckStacklessCoroLocalFusedRequestForTest(
				mode&1 != 0, mode&2 != 0, mode&4 != 0, locked) {
				t.Fatal("request changed task ownership, existing flags or queue state")
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
			"-test.run=^TestStacklessCoroLocalFusedRequest$")
		cmd.Env = append(os.Environ(), helper+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("local request acquired the scheduler lock: %v\n%s", err, out)
		}
	})
}
