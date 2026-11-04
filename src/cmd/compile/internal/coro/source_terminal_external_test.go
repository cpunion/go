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
		{"recover-immediate", `panic("unused")`, `if n==16{defer func(){if recover()!=nil{panic("original")}}()}`, `panic("original")`, "entry(16,replacement)"},
		{"recover-bounds", `panic("unused")`, `if n==16{defer func(){if recover()!=nil{var a []int;println(a[0])}}()}`, `panic("original")`, "entry(16,replacement)"},
		{"double-replacement", `runtime.Gosched();panic("original")`, "if n==16{defer f();defer f()}", `panic("original")`, "entry(16,replacement)"},
		{"deep-replacement", `runtime.Gosched();panic("replacement")`, "if n==220{defer f()}", `panic("original")`, "entry(220,replacement)"},
		{"nil-defer", `panic("unused")`, "if n==16{defer f()}", `panic("original")`, "entry(16,nil)"},
		{"goexit-replacement", `defer func(){runtime.Gosched();panic("replacement")}();runtime.Goexit()`, "if n==16{defer f()}", `panic("original")`, "entry(16,replacement)"},
		{"nested-recovery", "goto body;body:nested()", "if n==16{defer f()}", `panic("original")`, "entry(16,replacement)"},
		{"nil", `panic("unused")`, "", "panic(nil)", "entry(16,replacement)"},
		{"deep", `panic("unused")`, "", `panic("original")`, "entry(220,replacement)"},
		{"deep-host", `goto body;body:panic("original")`, "", "f()", "entry(220,replacement)"},
		{"deep-host-goexit", `goto body;body:defer func(){panic("original")}();runtime.Goexit()`, "", "f()", "entry(220,replacement)"},
		{"native-goexit", `runtime.Gosched();panic("replacement")`, "if n==16{defer f()}", "runtime.Goexit()", "entry(16,replacement)"},
		{"deep-native-goexit", `runtime.Gosched();panic("replacement")`, "if n==220{defer f()}", "runtime.Goexit()", "entry(220,replacement)"},
		{"outer-replacement", `panic("unused")`, "", `panic("original")`, `defer func(){panic("outer")}();entry(220,replacement)`},
		{"outer-goexit", `panic("unused")`, "", "runtime.Goexit()", `defer func(){panic("outer")}();entry(220,replacement)`},
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

func TestTerminalSourceStackDuringDefer(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import("runtime";"os")
//go:noinline
func recursive(n int){runtime.Gosched();if n>0{recursive(n-1);return};panic("original")}
var entry=recursive
//go:noinline
func inspect(){goto body;body:b:=make([]byte,1<<20);n:=runtime.Stack(b,false);print(string(b[:n]))}
func main(){goto body;body:n:=16;if os.Args[1]=="deep"{n=220};defer func(){inspect();if recover()!="original"{panic("missing original")}}();entry(n)}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	want := make(map[string]terminalSourceStack)
	for _, config := range []struct{ name, experiment, flags string }{
		{"ordinary", "nocoro", "-l"},
		{"native", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2"},
		{"inline", "coro", "-d=coro=4"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(dir, config.name)
			build := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+config.flags, "-o", exe, file)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if data, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			if config.experiment == "coro" && linkedCoroStackUnsafe(t, exe, "main.recursive.coro.stackgate") {
				t.Fatal("terminal source owner lost native admission")
			}
			for _, depth := range []string{"shallow", "deep"} {
				for _, level := range []string{"single", "system"} {
					for _, p := range []string{"1", "4"} {
						run := testenv.Command(t, exe, depth)
						run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=0", "GOTRACEBACK="+level)
						data, err := run.CombinedOutput()
						text := string(data)
						if err != nil || strings.Contains(text, ".coro.") || strings.Contains(text, "runtime.coroPanic") || strings.Contains(text, "source frame omitted") {
							t.Fatalf("%s %s P%s: %v\n%s", depth, level, p, err, text)
						}
						got := parseTerminalSourceStack(t, text)
						if len(got.frames) == 0 || (depth == "deep") != (got.elided > 0) {
							t.Fatalf("missing bounded source frames: %v", got)
						}
						if config.name == "ordinary" && want[depth].frames == nil {
							want[depth] = got
						}
						if !reflect.DeepEqual(got, want[depth]) {
							t.Fatalf("%s %s P%s: source stack %v, want %v\n%s", depth, level, p, got, want[depth], text)
						}
					}
				}
			}
		})
	}
}

func TestTerminalSourceLifetime(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import("runtime";"sync/atomic";"time")
type token struct {id int;pad [64]byte}
var finalized, recovered atomic.Int32
//go:noinline
func allocate(id int)*token {goto body;body:p:=&token{id:id};runtime.SetFinalizer(p,func(*token){finalized.Add(1)});return p}
//go:noinline
func take(){goto body;body:v:=recover();if v==nil{panic("missing value")};if v.(*token).id<0{panic("bad token")};recovered.Add(1)}
//go:noinline
func recursive(n int,p *token){runtime.Gosched();if n>0{recursive(n-1,p);return};panic(p)}
//go:noinline
func inside(i int,maker func(int)*token,cleanup func()){defer cleanup();p:=maker(i);recursive(8,p)}
//go:noinline
func propagated(i int,maker func(int)*token){p:=maker(i);recursive(8,p)}
var insideEntry=inside
var propagatedEntry=propagated
//go:noinline
func outside(i int){goto body;body:defer take();propagatedEntry(i,allocate)}
//go:noinline
func exitLeaf(p *token){goto body;body:defer func(){panic(p)}();runtime.Goexit()}
//go:noinline
func exitRoot(i int,maker func(int)*token,leaf func(*token),cleanup func(),done chan struct{}){defer close(done);defer cleanup();p:=maker(i);runtime.Gosched();leaf(p);panic("Goexit returned")}
//go:noinline
func launch(i int,maker func(int)*token,leaf func(*token),cleanup func(),done chan struct{}){go exitRoot(i,maker,leaf,cleanup,done)}
var launchEntry=launch
func main(){goto body;body:
 const cycles=1024
 for i:=0;i<cycles;i++ {insideEntry(i,allocate,take);outside(i)}
 for i:=0;i<cycles/2;i++ {done:=make(chan struct{});launchEntry(i,allocate,exitLeaf,take,done);<-done}
 const want=cycles*2+cycles/2
 if recovered.Load()!=want{println(recovered.Load(),want);panic("recovery count")}
 deadline:=time.Now().Add(20*time.Second)
 for finalized.Load()!=want&&time.Now().Before(deadline){runtime.GC();runtime.Gosched();time.Sleep(time.Millisecond)}
 if finalized.Load()!=want{println(finalized.Load(),want);panic("retained terminal user objects")}
 println("terminal-lifetime-ok")
}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, experiment, flags string }{
		{"ordinary", "nocoro", "-l"},
		{"native", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2"},
		{"inline", "coro", "-d=coro=4"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(dir, config.name)
			build := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+config.flags, "-o", exe, file)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if data, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			if config.experiment == "coro" {
				for _, name := range []string{"recursive", "inside", "propagated", "exitRoot", "launch"} {
					if linkedCoroStackUnsafe(t, exe, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission", name)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=0")
				data, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(data)) != "terminal-lifetime-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, data)
				}
			}
		})
	}
}
