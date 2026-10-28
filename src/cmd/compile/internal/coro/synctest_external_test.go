// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"context"
	"internal/testenv"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSynctestFallback(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const pkg = "cmd/compile/testdata/corosynctest"
	for _, config := range []struct{ name, experiment, flags string }{
		{"inline", "coro", "-d=coro=4"},
		{"noinline", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4,checkptr=2"},
		{"disabled", "nocoro", "-l"},
		{"race", "coro", "-l -d=coro=4"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "synctest.test")
			args := []string{"test", "-c", "-gcflags=" + pkg + "=" + config.flags, "-o", exe}
			if config.name == "race" {
				testenv.MustHaveCGO(t)
				args = append(args, "-race")
			}
			build := testenv.Command(t, testenv.GoToolPath(t), append(args, pkg)...)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			out, err := build.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
			symbols, err := nm.CombinedOutput()
			if err != nil {
				t.Fatalf("nm: %v\n%s", err, symbols)
			}
			native := config.experiment == "coro" && config.name != "race"
			names := []string{"sleep", "send", "exchange", "recursive", "arrayRootRecursive", "arrayRootCaller", "variadic", "recoverValue", "counter.result", "exit"}
			if testenv.HasCGO() {
				names = append(names, "foreign")
			}
			for _, name := range names {
				if got := strings.Contains(string(symbols), pkg+"."+name+".coro"); got != native {
					t.Errorf("%s native factory = %v, want %v\n%s", name, got, native, out)
				}
			}
			if got := strings.Contains(string(symbols), pkg+".arrayRootRecursive.coro.array"); got != native {
				t.Errorf("array root implementation = %v, want %v\n%s", got, native, out)
			}
			for _, procs := range []int{1, 4} {
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				run := testenv.CommandContext(t, ctx, exe, "-test.count=3", "-test.timeout=45s")
				run.Env = append(run.Environ(), "GOMAXPROCS="+strconv.Itoa(procs))
				out, err := run.CombinedOutput()
				cancel()
				if err != nil || string(out) != "PASS\n" {
					t.Errorf("P%d: %v\n%s", procs, err, out)
				}
				misuse := testenv.Command(t, exe, "-test.run=^TestCrossBubbleChannel$", "-test.timeout=45s")
				misuse.Env = append(misuse.Environ(), "CORO_CROSS_BUBBLE=1", "GOMAXPROCS="+strconv.Itoa(procs))
				out, err = misuse.CombinedOutput()
				if err == nil || !strings.Contains(string(out), "send on synctest channel from outside bubble") {
					t.Errorf("P%d: cross-bubble channel was not rejected: %v\n%s", procs, err, out)
				}
			}
		})
	}
}
