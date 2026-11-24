// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTerminalLogicalPanicLabelLifetime(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import("context";"fmt";"runtime";"runtime/pprof";"sync/atomic";"time";"unsafe")
type labelBacking [4096]byte
var finalized, recovered, printerExits atomic.Int32
// The finalizer belongs to the string's backing allocation itself, rather
// than a wrapper whose lifetime would not prove label metadata was released.
//go:noinline
func installLabel() {goto body;body:
 p:=new(labelBacking);p[0]='x'
 runtime.SetFinalizer(p,func(*labelBacking){finalized.Add(1)})
 value:=unsafe.String(&p[0],len(p))
 pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(),pprof.Labels("terminal-label",value)))
 runtime.KeepAlive(p)
}
//go:noinline
func clearLabel(){goto body;body:pprof.SetGoroutineLabels(context.Background())}
//go:noinline
func recoverValue(){goto body;body:
 if recover()==nil{panic("missing terminal value")}
 clearLabel();recovered.Add(1)
}
//go:noinline
func recursive(n int,exit bool){
 runtime.Gosched()
 if n>0{recursive(n-1,exit);return}
 if exit{var fn func();defer fn();runtime.Goexit()}
 panic("label-lifetime")
}
//go:noinline
func root(done chan struct{},exit bool,cleanup func()){
 if done!=nil{defer close(done)}
 defer cleanup()
 recursive(8,exit)
}
//go:noinline
func launch(done chan struct{},exit bool,cleanup func()){go root(done,exit,cleanup)}
type terminalValue struct {done chan struct{}}
//go:noinline
func (v terminalValue) Error() string {goto body;body:
 defer close(v.done)
 runtime.GC()
 printerExits.Add(1)
 runtime.Goexit()
 return "unreachable"
}
//go:noinline
func terminalRoot(done chan struct{}) {runtime.Gosched();panic(terminalValue{done})}
//go:noinline
func launchTerminal(done chan struct{}) {go terminalRoot(done)}
var terminalEntry=launchTerminal
var entry=root
var launchEntry=launch
func main(){goto body;body:
 const cycles=128
 for i:=0;i<cycles;i++{
  installLabel();entry(nil,false,recoverValue);clearLabel()
  for _,exit:=range []bool{false,true}{
   installLabel();done:=make(chan struct{});launchEntry(done,exit,recoverValue);clearLabel();<-done
  }
  // No recovery or label clearing in this child: the terminal host must
  // release its inherited label after Error calls Goexit.
  installLabel();done:=make(chan struct{});terminalEntry(done);clearLabel();<-done
  if i%16==0{runtime.GC()}
 }
 if printerExits.Load()!=cycles{panic(fmt.Sprintf("printer exits %d",printerExits.Load()))}
 if recovered.Load()!=3*cycles{panic(fmt.Sprintf("recovered %d",recovered.Load()))}
 deadline:=time.Now().Add(10*time.Second)
 for finalized.Load()!=4*cycles&&time.Now().Before(deadline){runtime.GC();runtime.Gosched();time.Sleep(time.Millisecond)}
 if finalized.Load()!=4*cycles{panic(fmt.Sprintf("terminal metadata retained label allocations: %d/%d",finalized.Load(),4*cycles))}
 println("PASS terminal label allocations released",finalized.Load())
}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, experiment, flags, extra string }{
		{"ordinary", "nocoro", "-l", ""},
		{"native", "coro", "-l -d=coro=4", ""},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2", ""},
		{"inline", "coro", "-d=coro=4", ""},
		{"lockrank", "coro,staticlockranking", "-l -d=coro=4", ""},
		{"ordinary-race", "nocoro", "-l", "-race"},
		{"coro-race", "coro", "-l -d=coro=4", "-race"},
	} {
		t.Run(config.name, func(t *testing.T) {
			if config.extra == "-race" {
				testenv.MustHaveCGO(t)
			}
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
			if config.experiment != "nocoro" && config.extra == "" {
				for _, name := range []string{"root", "recursive", "launch", "terminalRoot", "launchTerminal"} {
					if linkedCoroStackUnsafe(t, exe, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission", name)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := terminalPanicCommand(t, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=0,tracebackancestors=3")
				if data, err := run.CombinedOutput(); err != nil {
					t.Fatalf("P%s: %v\n%s", p, err, data)
				}
			}
		})
	}
}
