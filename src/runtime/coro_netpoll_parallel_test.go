// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"internal/testenv"
	"os"
	"runtime"
	"testing"
)

func TestStacklessCoroNetpollParallelDrain(t *testing.T) {
	const child = "GO_TEST_CORO_NETPOLL_PARALLEL"
	if os.Getenv(child) != "1" {
		cmd := testenv.CleanCmdEnv(testenv.Command(t, testenv.Executable(t),
			"-test.run=^TestStacklessCoroNetpollParallelDrain$"))
		cmd.Env = append(cmd.Env, child+"=1")
		if debug := os.Getenv("GODEBUG"); debug != "" {
			cmd.Env = append(cmd.Env, "GODEBUG="+debug)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("netpoll parallel drain subprocess: %v\n%s", err, out)
		}
		return
	}
	if msg := runtime.CheckStacklessCoroNetpollPopOneForTest(); msg != "" {
		t.Fatal(msg)
	}
}
