// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/buildcfg"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCapturedCallableEnvironments(t *testing.T) {
	if !buildcfg.Experiment.Coro {
		t.Skip("requires coroutine experiment")
	}
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main
import "runtime"
func closed(c chan int) int {
 seed:=3
 f:=func(delta int)int{seed+=delta;runtime.Gosched();v:=<-c;seed+=v;return seed}
 first:=f(2);second:=f(4)
 return first*100+second
}
func choose(which bool,c chan int)int {
 seed:=4;text:="ab"
 f:=func(x int)int{v:=<-c;return seed+x+v}
 if which {f=func(x int)int{runtime.Gosched();v:=<-c;return len(text)*10+x+v}}
 return f(3)
}
func shared(gate,ready chan int)(func()int,func()) {
 seed:=2
 read:=func()int{ready<-1;v:=<-gate;return seed+v}
 write:=func(){seed=30}
 return read,write
}
func exits(done chan struct{},result *int) {
 seed:=41
 go func(){defer func(){*result=seed;close(done)}();runtime.Gosched();runtime.Goexit()}()
}
func counter(seed int,c chan int)func(int)int {
 return func(delta int)int {seed+=delta;runtime.Gosched();v:=<-c;seed+=v;return seed}
}
func instances(c chan int) int {
 a,b:=counter(10,c),counter(100,c)
 x:=a(1);y:=b(2);z:=a(3)
 return x+y+z
}
func spawn(gate chan int,ready,done chan int) {
 for range 64 {seed:=7;go func(){ready<-1;v:=<-gate;seed+=v;done<-seed}()}
}
func recovery(seed int)func()int {
 return func()(result int){defer func(){if recover()!=nil {result=seed+9};seed++}();runtime.Gosched();panic("capture")}
}
func nesting(seed int)func()func()int {
 return func()func()int{runtime.Gosched();return func()int{runtime.Gosched();seed++;return seed}}
}
func order(which bool)(result int) {
 calls:=0;base:=8
 var f func(int)int
 if which {f=func(x int)int{runtime.Gosched();return base+x}}
 defer func(){if recover()!=nil {if calls!=1 {panic("nil operand order")};result=91}}()
 arg:=func()int{calls++;base=19;runtime.Gosched();return 2}
 result=f(arg());return result
}
type object struct {data [2048]byte}
func kept(gate chan int,ready chan int,dead chan int)func()int {
 p:=new(object);p.data[0]=23
 runtime.SetFinalizer(p,func(*object){dead<-1})
 return func()int{ready<-1;v:=<-gate;return int(p.data[0])+v}
}
func main(){
 c:=make(chan int,16);for range 16 {c<-1}
 if closed(c)!=611 || instances(c)!=131 || choose(false,c)!=8 || choose(true,c)!=24 {panic("shared capture or instance isolation")}
 gate,ready,done:=make(chan int),make(chan int),make(chan int)
 before:=runtime.NumGoroutine()
 spawn(gate,ready,done)
 for range 64 {<-ready}
 if NATIVE && runtime.NumGoroutine()>before+20 {panic("ordinary go per captured child")}
 runtime.GC();for range 64 {gate<-5};for range 64 {if <-done!=12 {panic("captured spawn result")}}
 f:=recovery(20);if f()!=29 || f()!=30 {panic("captured direct recover")}
 first:=nesting(30);nested:=first();if nested()!=31 || nested()!=32 {panic("nested shared cell")}
 if order(false)!=91 || order(true)!=21 {panic("callee or operand snapshot")}
 read,write:=shared(gate,ready)
 go func(){done<-read()}();<-ready;runtime.GC();write();gate<-4
 if <-done!=34 {panic("shared cell changed while parked")}
 exited:=make(chan struct{});exitResult:=0;exits(exited,&exitResult);<-exited;if exitResult!=41 {panic("captured Goexit cleanup")}
 locked:=counter(4,c);runtime.LockOSThread();if locked(1)!=6 {panic("captured locked ordinary body")};runtime.UnlockOSThread();if locked(1)!=8 {panic("captured unlocked native body")}
 dead:=make(chan int,1);fn:=kept(gate,ready,dead)
 go func(){<-ready;runtime.GC();select{case <-dead:panic("premature captured object collection");default:};gate<-4}()
 if fn()!=27 {panic("captured GC result")};fn=nil
 released:=false
 for range 64 {runtime.GC();runtime.Gosched();select {case <-dead:released=true;default:};if released {break}}
 if !released {panic("completed frame retained captured object")}
 println("captured-callables-ok")
}
`
	for _, config := range []struct{ name, experiment, flags string }{
		{"native", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2"},
		{"ordinary", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "main.go")
			if err := os.WriteFile(path, []byte(strings.ReplaceAll(source, "NATIVE", map[bool]string{true: "true", false: "false"}[config.experiment == "coro"])), 0600); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "program")
			build := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+config.flags, "-o", binary, path)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			out, err := build.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			if config.experiment == "coro" {
				for _, name := range []string{"closed", "instances", "counter.func1", "spawn", "spawn.func1", "recovery.func1", "nesting.func1", "nesting.func1.1", "choose", "choose.func1", "choose.func2", "shared.func1", "exits", "exits.func1", "kept.func2"} {
					if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission\n%s", name, out)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "captured-callables-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
