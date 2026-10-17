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

func TestCgoFallback(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	testenv.MustHaveCGO(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const pkg = "cmd/compile/testdata/corocgo"
	for _, config := range []struct{ name, experiment, flags string }{
		{"inline", "coro", "-d=coro=4"},
		{"noinline", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4,checkptr=2"},
		{"cgocheck2", "coro,cgocheck2", "-l -d=coro=4"},
		{"disabled", "nocoro", "-l"},
		{"race", "coro", "-l -d=coro=4"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "cgo.test")
			args := []string{"test", "-c", "-gcflags=" + pkg + "=" + config.flags, "-o", exe}
			if config.name == "race" {
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
			native := config.experiment != "nocoro" && config.name != "race"
			for _, name := range []string{"scalar", "locked"} {
				if got := strings.Contains(string(symbols), pkg+"."+name+".coro"); got != native {
					t.Errorf("%s native factory = %v, want %v\n%s", name, got, native, out)
				}
			}
			if strings.Contains(string(symbols), "_Cdirect_call_callback") {
				t.Error("callback-capable C call acquired a direct entry")
			}
			if native {
				dump := testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump", "-s", pkg+"\\.scalar$", exe)
				out, err := dump.CombinedOutput()
				if err != nil || !strings.Contains(string(out), "runtime.coroUseOrdinary") ||
					!strings.Contains(string(out), "_Cfunc_add") {
					t.Errorf("public entry lost ordinary cgo fallback: %v\n%s", err, out)
				}
				dump = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump", "-s", pkg+"\\.scalar\\.coro", exe)
				out, err = dump.CombinedOutput()
				if err != nil || !strings.Contains(string(out), "_Cdirect_add") ||
					strings.Contains(string(out), "_Cfunc_add") {
					t.Errorf("native entry lost direct C call: %v\n%s", err, out)
				}
			}
			for _, procs := range []int{1, 4} {
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				run := testenv.CommandContext(t, ctx, exe, "-test.count=3", "-test.timeout=45s")
				run.Env = append(run.Environ(), "GOMAXPROCS="+strconv.Itoa(procs), "GOGC=1", "GODEBUG=cgocheck=1")
				out, err := run.CombinedOutput()
				cancel()
				if err != nil || string(out) != "PASS\n" {
					t.Errorf("P%d: %v\n%s", procs, err, out)
				}
			}
		})
	}
}
