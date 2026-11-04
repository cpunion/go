// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestTerminalSourceTraceback(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import "runtime"
//go:noinline
func replacement(){REPLACEMENT}
//go:noinline
func nested(){defer func(){recover()}();panic("ignored")}
//go:noinline
func recursive(n int,f func()) {
 DEFER
 runtime.Gosched()
 if n>0 {recursive(n-1,f);return}
 PANIC
}
var entry=recursive
//go:noinline
func launch(){go recursive(16,replacement)}
var launchEntry=launch
func main(){goto body;body:MAIN}
`
	for _, test := range []struct{ name, replacement, deferred, body, main string }{
		{"direct", `panic("unused")`, "", `panic("original")`, "entry(16,replacement)"},
		{"host", `goto body;body:panic("original")`, "", "f()", "entry(16,replacement)"},
		{"goexit", `goto body;body:defer func(){panic("original")}();runtime.Goexit()`, "", "f()", "entry(16,replacement)"},
		{"native-replacement", `runtime.Gosched();panic("replacement")`, "if n==16{defer f()}", `panic("original")`, "entry(16,replacement)"},
		{"ordinary-replacement", `goto body;body:panic("replacement")`, "if n==16{defer f()}", `panic("original")`, "entry(16,replacement)"},
		{"recover-repanic", `panic("unused")`, `if n==16{defer func(){if recover()!=nil{runtime.Gosched();panic("replacement")}}()}`, `panic("original")`, "entry(16,replacement)"},
		{"nested-recovery", "goto body;body:nested()", "if n==16{defer f()}", `panic("original")`, "entry(16,replacement)"},
		{"nil", `panic("unused")`, "", "panic(nil)", "entry(16,replacement)"},
		{"deep", `panic("unused")`, "", `panic("original")`, "entry(220,replacement)"},
		{"independent-go", `panic("unused")`, "", `panic("original")`, "launchEntry();select{}"},
		{"bounds", `panic("unused")`, "", "var a []int;println(a[n])", "entry(16,replacement)"},
		{"channel", `panic("unused")`, "", "c:=make(chan int);close(c);c<-1", "entry(16,replacement)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "main.go")
			source := strings.NewReplacer("REPLACEMENT", test.replacement, "DEFER", test.deferred, "PANIC", test.body, "MAIN", test.main).Replace(program)
			if err := os.WriteFile(file, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			var want terminalSourceStack
			for _, config := range []struct{ name, experiment, flags, extra string }{
				{"ordinary", "nocoro", "-l", ""},
				{"native", "coro", "-l -d=coro=4", ""},
				{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2", ""},
				{"inline", "coro", "-d=coro=4", ""},
				{"no-dwarf", "coro", "-d=coro=4", "-ldflags=-w"},
				{"pie", "coro", "-l -d=coro=4", "-buildmode=pie"},
			} {
				t.Run(config.name, func(t *testing.T) {
					exe := filepath.Join(dir, config.name)
					args := []string{"build", "-gcflags=" + config.flags, "-o", exe}
					if config.extra != "" {
						args = append(args, config.extra)
					}
					build := testenv.Command(t, testenv.GoToolPath(t), append(args, file)...)
					build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
					if data, err := build.CombinedOutput(); err != nil {
						t.Fatalf("build: %v\n%s", err, data)
					}
					if config.experiment == "coro" && linkedCoroStackUnsafe(t, exe, "main.recursive.coro.stackgate") {
						t.Fatal("terminal source owner lost native admission")
					}
					for _, p := range []string{"1", "4"} {
						debugs := []string{"asyncpreemptoff=0"}
						if test.name == "nil" {
							debugs = append(debugs, "asyncpreemptoff=0,panicnil=1")
						}
						for _, debug := range debugs {
							run := testenv.Command(t, exe)
							run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG="+debug, "GOTRACEBACK=single")
							data, err := run.CombinedOutput()
							exit, ok := err.(*exec.ExitError)
							text := string(data)
							if !ok || exit.ExitCode() != 2 || !strings.Contains(text, "panic:") || strings.Contains(text, "fatal error:") || strings.Contains(text, "bad recovery") || strings.Contains(text, ".coro.") || strings.Contains(text, "source frame omitted") {
								t.Fatalf("P%s %s: expected ordinary uncaught panic: %v\n%s", p, debug, err, text)
							}
							got := parseTerminalSourceStack(t, text)
							if config.name == "ordinary" && want.frames == nil {
								want = got
							}
							if !reflect.DeepEqual(got, want) {
								t.Fatalf("P%s %s: source stack %v, want %v\n%s", p, debug, got, want, text)
							}
							if len(got.frames) == 0 {
								t.Fatal("missing source frames")
							}
						}
					}
				})
			}
		})
	}
}

type terminalSourceStack struct {
	frames []string
	elided int
}

func parseTerminalSourceStack(t *testing.T, text string) terminalSourceStack {
	t.Helper()
	var result terminalSourceStack
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "...") && strings.HasSuffix(line, " frames elided...") {
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(line, "..."), " frames elided..."))
			if err != nil {
				t.Fatal(err)
			}
			result.elided += n
		}
		if !strings.HasPrefix(line, "main.") {
			continue
		}
		at := strings.IndexByte(line, '(')
		if at < 0 || i+1 >= len(lines) {
			t.Fatalf("malformed source frame: %q", line)
		}
		location := strings.Fields(lines[i+1])[0]
		if !strings.Contains(location, "main.go:") {
			t.Fatalf("missing source file: %q", location)
		}
		result.frames = append(result.frames, line[:at]+" "+filepath.Base(location))
	}
	return result
}
