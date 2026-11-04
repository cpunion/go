// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"fmt"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestTerminalSourceCallers(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import("runtime";"fmt";"path/filepath";"os")
//go:noinline
func recursive(n int){runtime.Gosched();if n>0{recursive(n-1);return};panic("original")}
var entry=recursive
//go:noinline
func inspect(skip,size int){goto body;body:pcs:=make([]uintptr,size);n:=runtime.Callers(skip,pcs);fmt.Printf("query %d %d %d\n",skip,size,n);if n==0{return};frames:=runtime.CallersFrames(pcs[:n]);for{f,more:=frames.Next();fmt.Printf("%s %s:%d\n",f.Function,filepath.Base(f.File),f.Line);if !more{break}}}
func main(){goto body;body:n:=16;if os.Args[1]=="deep"{n=220};defer func(){for _,skip:=range []int{0,1,2,3,4,49,50,100,219,220,222,224,225,226,227,228,1000}{for _,size:=range []int{0,1,2,17,50,100,512}{inspect(skip,size)}};if recover()!="original"{panic("missing original")}}();entry(n)}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	want := make(map[string]string)
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
				t.Fatal("terminal PC source owner lost native admission")
			}
			for _, depth := range []string{"shallow", "deep"} {
				for _, p := range []string{"1", "4"} {
					run := testenv.Command(t, exe, depth)
					run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=0")
					data, err := run.CombinedOutput()
					text := string(data)
					if err != nil || strings.Contains(text, ".coro.") || strings.Contains(text, "runtime.coroPanic") {
						t.Fatalf("%s P%s: %v\n%s", depth, p, err, text)
					}
					if config.name == "ordinary" && want[depth] == "" {
						want[depth] = text
					}
					if text != want[depth] {
						t.Fatalf("%s P%s: terminal PC oracle differs\ngot:\n%s\nwant:\n%s", depth, p, text, want[depth])
					}
				}
			}
		})
	}
}

func TestTerminalSourceCallersOutcomes(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import("runtime";"fmt";"path/filepath";"strings")
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
func worker(){goto body;body:defer func(){inspect();recover()}();MAIN}

type T struct{}
type I interface{Boom()}
//go:noinline
func(T)Boom(){goto body;body:panic("method")}
//go:noinline
func(T)Native(){runtime.Gosched();panic("method")}
//go:noinline
func inspect(){goto body;body:for _,q:=range [][2]int{{0,512},{1,512},{3,512},{49,7},{100,512},{219,7},{225,512},{1000,512}}{pcs:=make([]uintptr,q[1]);n:=runtime.Callers(q[0],pcs);fmt.Printf("query %d %d %d\n",q[0],q[1],n);if n==0{continue};frames:=runtime.CallersFrames(pcs[:n]);for{f,more:=frames.Next();if strings.HasPrefix(f.Function,"main."){fmt.Printf("%s %s:%d\n",f.Function,filepath.Base(f.File),f.Line)}else{fmt.Println(f.Function)};if !more{break}}}}
func main(){goto body;body:done:=make(chan struct{});go func(){defer close(done);worker()}();<-done}
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
		{"bounds", `panic("unused")`, "", "var a []int;println(a[n])", "entry(16,replacement)"},
		{"nil-pointer", `panic("unused")`, "", "var p *int;println(*p)", "entry(220,replacement)"},
		{"divide-zero", `panic("unused")`, "", "println(1/n)", "entry(220,replacement)"},
		{"negative-make", `panic("unused")`, "", "println(len(make([]int,n-1)))", "entry(220,replacement)"},
		{"type-assert", `panic("unused")`, "", `var x any="text";println(x.(int))`, "entry(220,replacement)"},
		{"nil-call", `panic("unused")`, "", "f()", "entry(220,nil)"},
		{"channel", `panic("unused")`, "", "c:=make(chan int);close(c);c<-1", "entry(16,replacement)"},
		{"bound-host", `panic("unused")`, "", "f()", "entry(220,T{}.Boom)"},
		{"bound-native", `panic("unused")`, "", "f()", "entry(220,T{}.Native)"},
		{"interface", `panic("unused")`, "", "f()", "var x I=T{};entry(220,x.Boom)"},
		{"nil-wrapper", `panic("unused")`, "", "f()", "var x I=(*T)(nil);entry(220,x.Boom)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := strings.NewReplacer("REPLACEMENT", test.replacement, "DEFER", test.deferred, "PANIC", test.body, "MAIN", test.main).Replace(program)
			testTerminalSourcePCProgram(t, source, []string{"recursive"})
		})
	}
}

func TestTerminalSourceCallersSpawn(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import("runtime";"fmt";"path/filepath";"strings")
//go:noinline
func recursive(n int){runtime.Gosched();if n>0{recursive(n-1);return};panic("original")}
//go:noinline
func take(){goto body;body:pcs:=make([]uintptr,512);n:=runtime.Callers(0,pcs);fmt.Println("frames",n);frames:=runtime.CallersFrames(pcs[:n]);for{f,more:=frames.Next();if strings.HasPrefix(f.Function,"main."){fmt.Printf("%s %s:%d\n",f.Function,filepath.Base(f.File),f.Line)}else{fmt.Println(f.Function)};if !more{break}};if recover()!="original"{panic("missing original")}}
//go:noinline
func child(cleanup func(),done chan struct{}){defer close(done);defer cleanup();recursive(220)}
//go:noinline
func launch(f func(func(),chan struct{}),cleanup func(),done chan struct{}){go f(cleanup,done)}
var entry=launch
func main(){goto body;body:done:=make(chan struct{});entry(child,take,done);<-done}
`
	testTerminalSourcePCProgram(t, program, []string{"recursive", "child", "launch"})
}

func TestTerminalSourceCallersBlockedChannel(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import("runtime";"fmt";"path/filepath";"strings";"time")
var sendChannel chan int
var sendReady chan struct{}
//go:noinline
func recursive(n int){runtime.Gosched();if n>0{recursive(n-1);return};close(sendReady);sendChannel<-1}
var entry=recursive
//go:noinline
func inspect(){goto body;body:for _,q:=range [][2]int{{0,512},{1,512},{3,512},{49,7},{100,512},{219,7},{225,512},{1000,512}}{pcs:=make([]uintptr,q[1]);n:=runtime.Callers(q[0],pcs);fmt.Printf("query %d %d %d\n",q[0],q[1],n);if n==0{continue};frames:=runtime.CallersFrames(pcs[:n]);for{f,more:=frames.Next();if strings.HasPrefix(f.Function,"main."){fmt.Printf("%s %s:%d\n",f.Function,filepath.Base(f.File),f.Line)}else{fmt.Println(f.Function)};if !more{break}}}}
func worker(){goto body;body:defer func(){inspect();if value:=recover();fmt.Sprint(value)!="send on closed channel"{panic("missing send panic")}}();sendChannel=make(chan int);sendReady=make(chan struct{});go func(){<-sendReady;runtime.GC();time.Sleep(20*time.Millisecond);close(sendChannel)}();entry(220)}
func main(){goto body;body:done:=make(chan struct{});go func(){defer close(done);worker()}();<-done}
`
	testTerminalSourcePCProgramWithOracle(t, program, []string{"recursive"}, func(t *testing.T, want, got string) {
		ordinary := terminalSourcePCQueries(t, want)
		native := terminalSourcePCQueries(t, got)
		// A blocked native send finishes as a pending operation. Its next
		// task owner captures the source chain without a physical chansend
		// activation. Preserve those real PCs instead of inventing the two
		// private ordinary channel-send frames. Every other frame must match.
		// If closing wins before the send parks, its synchronous private
		// frames are valid too; scheduling must not change the source checks.
		var expected []string
		for _, frame := range ordinary {
			if frame != "runtime.chansend" && frame != "runtime.<channel-send-entry>" {
				expected = append(expected, frame)
			}
		}
		if len(ordinary)-len(expected) != 2 || !slices.Equal(native, expected) && !slices.Equal(native, ordinary) {
			t.Fatalf("blocked channel source chain differs\ngot:\n%s\nwant:\n%s", strings.Join(native, "\n"), strings.Join(expected, "\n"))
		}
		recursive := 0
		for _, frame := range native {
			if strings.HasPrefix(frame, "main.recursive ") {
				recursive++
			}
		}
		if recursive != 221 {
			t.Fatalf("blocked channel retained %d recursive frames, want 221", recursive)
		}
	})
}

// Independently check skip and buffer limits against this execution's complete
// stack. Private runtime activations can differ between ordinary synchronous
// calls and native operation completion, while each query must remain exact.
func terminalSourcePCQueries(t *testing.T, text string) []string {
	t.Helper()
	type query struct {
		skip, size, count int
		frames            []string
	}
	var queries []query
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if strings.HasPrefix(line, "query ") {
			var q query
			if n, err := fmt.Sscanf(line, "query %d %d %d", &q.skip, &q.size, &q.count); err != nil || n != 3 {
				t.Fatalf("invalid query %q: %v", line, err)
			}
			queries = append(queries, q)
		} else {
			if len(queries) == 0 || line == "" {
				t.Fatalf("invalid frame %q", line)
			}
			q := &queries[len(queries)-1]
			q.frames = append(q.frames, line)
		}
	}
	if len(queries) == 0 || queries[0].skip != 0 || queries[0].size != 512 {
		t.Fatal("missing complete terminal PC query")
	}
	full := queries[0].frames
	for _, q := range queries {
		if q.skip < 0 || q.size < 0 {
			t.Fatal("negative query argument")
		}
		lo := min(q.skip, len(full))
		hi := min(lo+q.size, len(full))
		if q.count != len(q.frames) || !slices.Equal(q.frames, full[lo:hi]) {
			t.Fatalf("skip %d size %d: count or complete-stack slice differs", q.skip, q.size)
		}
	}
	return full
}

// Runtime channel sends have distinct ordinary and native private entry
// functions. Keep that real implementation frame and its effect on skip and
// buffer counts, while comparing all user function names and positions exactly.
func normalizeTerminalChannelPC(text string) string {
	text = strings.ReplaceAll(text, "\nruntime.chansend1\n", "\nruntime.<channel-send-entry>\n")
	return strings.ReplaceAll(text, "\nruntime.tryStacklessCoroChanSend\n", "\nruntime.<channel-send-entry>\n")
}

func testTerminalSourcePCProgram(t *testing.T, source string, nativeOwners []string) {
	t.Helper()
	testTerminalSourcePCProgramWithOracle(t, source, nativeOwners, nil)
}

func testTerminalSourcePCProgramWithOracle(t *testing.T, source string, nativeOwners []string, oracle func(*testing.T, string, string)) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	var want string
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
				for _, name := range nativeOwners {
					if linkedCoroStackUnsafe(t, exe, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission", name)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=0")
				data, err := run.CombinedOutput()
				text := string(data)
				if err != nil || strings.Contains(text, ".coro.") || strings.Contains(text, "runtime.coroPanic") {
					t.Fatalf("P%s: %v\n%s", p, err, text)
				}
				text = normalizeTerminalChannelPC(text)
				if config.name == "ordinary" && want == "" {
					want = text
				}
				if oracle != nil && config.name != "ordinary" {
					oracle(t, want, text)
				} else if text != want {
					t.Fatalf("P%s: terminal PC source and skip oracle differs\ngot:\n%s\nwant:\n%s", p, text, want)
				}
			}
		})
	}
}
