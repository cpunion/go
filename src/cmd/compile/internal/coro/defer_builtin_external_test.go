// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDeferBuiltin(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import "runtime"
func run() {
	defer func() {
		value := recover()
		print("defer-")
		println("builtin-ok")
		if value != 42 { panic("recover value changed") }
	}()
	runtime.Gosched()
	panic(42)
}
func main() { run() }
`
	source := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(source, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, experiment, flags string }{
		{"coro/inline", "coro", "-d=coro=4"},
		{"coro/noinline", "coro", "-l -d=coro=4"},
		{"coro/checkptr", "coro", "-l -d=coro=4,checkptr=2"},
		{"nocoro", "nocoro", "-l"},
	} {
		t.Run(test.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "defer.exe")
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+test.flags, "-o", exe, source)
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+test.experiment, "GOWORK=off")
			out, err := cmd.CombinedOutput()
			if err != nil || test.experiment == "coro" && strings.Contains(string(out), "coro: skip main.run:") {
				t.Fatalf("build failed or native path fell back: %v\n%s", err, out)
			}
			cmd = testenv.Command(t, exe)
			cmd.Env = append(cmd.Environ(), "GOMAXPROCS=1", "GOGC=1")
			if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "defer-builtin-ok" {
				t.Fatalf("execution = (%v, %q), want defer-builtin-ok", err, out)
			}
			if test.experiment == "coro" {
				cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump", "-s", `^main\.run\.coro(\.func[0-9]+)?$`, exe)
				if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "TEXT main.run.coro") {
					t.Fatalf("missing native factory: %v\n%s", err, out)
				}
			}
		})
	}
}
