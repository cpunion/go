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

func TestRepeatedNestedDeferCaptures(t *testing.T) {
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main
import "runtime"
type box struct { n int; pad [2048]byte }
//go:noinline
func repeatedCells()(trace []int) {
 for i:=0;i<4;i++ { x:=i; defer func(){trace=append(trace,func()int{return func()int{return x}()}())}(); x+=10 }
 runtime.Gosched(); return
}
//go:noinline
func sharedCells()(trace []int) {
 x:=0; for i:=0;i<4;i++ { defer func(){trace=append(trace,func()int{return x}())}(); x++ }
 runtime.Gosched(); return
}
//go:noinline
func escapedCells()(saved []func()int) {
 for i:=0;i<4;i++ { p:=&box{n:i}; p.pad[2047]=byte(i+1); defer func(){saved=append(saved,func()int{return func()int{return p.n+int(p.pad[2047])}()})}(); p.n+=10 }
 runtime.GC(); runtime.Gosched(); return
}
//go:noinline
func typedCapture()(trace []int) {
 for i:=0;i<4;i++ { x:=box{n:i}; x.pad[2047]=byte(i+1); defer func(){trace=append(trace,func()int{return x.n+int(x.pad[2047])}())}(); x.n+=10 }
 runtime.GC(); runtime.Gosched(); return
}
//go:noinline
func aliasAddresses()(pointers []*int) {
 for i:=0;i<4;i++ { x:=i; pointers=append(pointers,&x); defer func(){*func()*int{return &x}()+=10}() }
 runtime.GC(); runtime.Gosched(); return
}
//go:noinline
func panicOwner()(caught any,trace []int) {
 defer func(){caught=recover()}()
 for i:=0;i<4;i++ { x:=i; defer func(){v:=func()int{return x}();trace=append(trace,v);if v!=0{panic(v)}}() }
 runtime.Gosched(); panic("body")
}
//go:noinline
func recoveredLoop()(caught any,trace []int) {
 for i:=0;i<4;i++ { x:=i; defer func(){trace=append(trace,func()int{return x}());if v:=recover();v!=nil{caught=v}}() }
 runtime.Gosched(); panic("body")
}
//go:noinline
func indirectRecover()(caught any,trace []int) {
 for i:=0;i<4;i++ { x:=i; defer func(){if func()any{return recover()}()!=nil{panic("nested normal recover stole panic")};trace=append(trace,func()int{return x}());if v:=recover();v!=nil{caught=v}}() }
 runtime.Gosched(); panic("body")
}
//go:noinline
func exitOwner(done chan []int) {
 var trace []int; defer func(){done<-trace}()
 for i:=0;i<4;i++ { x:=i; defer func(){trace=append(trace,func()int{return x}())}() }
 runtime.Gosched(); runtime.Goexit()
}
//go:noinline
func values(y func(int)bool) {for i:=1;i<=3;i++ {if !y(i){return}}}
//go:noinline
func rangeOwner()(trace []int) {
 for v:=range values {defer func(){trace=append(trace,func()int{return v}())}();v+=10}
 runtime.GC();runtime.Gosched();return
}
//go:noinline
func parked(v int,ready,gate chan struct{})(n int) {
 for j:=0;j<4;j++ {x:=v+j;defer func(){n+=func()int{return x}()}()}
 ready<-struct{}{};<-gate;return
}
//go:noinline
func worker(v int,ready,gate chan struct{},done chan int) {n:=parked(v,ready,gate);done<-n}
func check(got []int,want ...int) {if len(got)!=len(want){panic("trace length")};for i,n:=range want{if got[i]!=n{panic("trace value")}}}
func controls() {
 check(repeatedCells(),13,12,11,10);check(sharedCells(),4,4,4,4);check(typedCapture(),17,15,13,11)
 saved:=escapedCells();runtime.GC();for i,f:=range saved{if f()!=17-2*i{panic("escaped nested capture cell")}}
 ps:=aliasAddresses();runtime.GC();for i,p:=range ps{if *p!=i+10{panic("nested capture address")};for j:=0;j<i;j++{if ps[j]==p{panic("aliased iteration cell")}}}
 caught,trace:=panicOwner();if caught!=1{panic("replacement panic")};check(trace,3,2,1,0)
 caught,trace=recoveredLoop();if caught!="body"{panic("direct repeated recover")};check(trace,3,2,1,0)
 caught,trace=indirectRecover();if caught!="body"{panic("indirect recover isolation")};check(trace,3,2,1,0)
 done:=make(chan []int,1);go exitOwner(done);check(<-done,3,2,1,0);check(rangeOwner(),13,12,11)
}
func main() {
 controls();runtime.LockOSThread();controls();runtime.UnlockOSThread()
 ready:=make(chan struct{},32);gate:=make(chan struct{});done:=make(chan int,32)
 for i:=0;i<32;i++ {go worker(i,ready,gate,done)};for i:=0;i<32;i++{<-ready}
 runtime.GC();close(gate);sum:=0;for i:=0;i<32;i++{sum+=<-done};if sum!=2176{panic("parked nested capture roots")}
 println("repeated-nested-defer-captures-ok")
}
`
	for _, config := range []struct{ name, experiment, flags string }{
		{"native", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2"},
		{"native-inline", "coro", "-d=coro=4"},
		{"checkptr-inline", "coro", "-d=coro=4 -d=checkptr=2"},
		{"native-no-dwarf", "coro", "-d=coro=4 -dwarf=false -gendwarfinl=0"},
		{"ordinary", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			dir := t.TempDir()
			file, exe := filepath.Join(dir, "main.go"), filepath.Join(dir, "program")
			if err := os.WriteFile(file, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			build := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+config.flags, "-o", exe, file)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			if config.experiment == "coro" {
				for _, name := range []string{"repeatedCells", "sharedCells", "escapedCells", "typedCapture", "aliasAddresses", "panicOwner", "recoveredLoop", "indirectRecover", "exitOwner", "rangeOwner", "parked", "worker"} {
					t.Run(name, func(t *testing.T) {
						if linkedCoroStackUnsafe(t, exe, "main."+name+".coro.stackgate") {
							t.Fatal("repeated nested defer capture lost native admission")
						}
					})
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				if out, err := run.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "repeated-nested-defer-captures-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
