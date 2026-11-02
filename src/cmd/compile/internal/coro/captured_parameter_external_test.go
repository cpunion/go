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

func TestCapturedCallbackParameters(t *testing.T) {
	if !buildcfg.Experiment.Coro {
		t.Skip("requires coroutine experiment")
	}
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main
import "runtime"
//go:noinline
func invoke(f func(int)int,x int)int {return f(x)}
//go:noinline
func hop(f func(int)int,x int)int {return invoke(f,x)+1}
//go:noinline
func recurse(f func(int)int,n int)int {if n==0 {return f(2)};return recurse(f,n-1)+1}
//go:noinline
func launch(f func()) {go f()}
//go:noinline
func invokeVoid(f func()) {f()}
func pure(seed int)func(int)int {return func(x int)int{seed+=x;return seed}}
func readonly(seed int)func(int)int {return func(x int)int{return seed+x}}
func direct(seed int)int {return invoke(func(x int)int{runtime.Gosched();seed+=x;return seed},2)}
func pureDriver()int {f:=pure(10);a:=invoke(f,2);b:=invoke(f,3);return a+b}
func instances()int {f,g:=pure(1),pure(100);a:=invoke(f,2);b:=invoke(g,5);c:=invoke(f,3);return a+b+c}
func readonlyDriver()int {f:=readonly(10);g:=readonly(100);return invoke(f,2)+invoke(g,3)}
func selected(which bool)int {seed:=10;text:="abcd";f:=func(x int)int{runtime.Gosched();seed+=x;return seed};if which {f=func(x int)int{return len(text)+x}};return hop(f,2)}
func outer(seed int,c chan int)func(int)int {return func(x int)int{return invoke(func(y int)int{seed+=y;v:=<-c;return seed+v},x)}}
var operands int
func argument()int {operands++;runtime.Gosched();return 2}
func nilDriver(isNil bool)(result any) {seed:=10;f:=func(x int)int{runtime.Gosched();return seed+x};if isNil {f=nil};defer func(){if v:=recover();v!=nil {result=v}}();return invoke(f,argument())}
func recursiveDriver()int {seed:=10;return recurse(func(x int)int{seed+=x;runtime.Gosched();return seed},32)}
func spawn(ready,release,done chan int) {for v:=range 64 {launch(func(){ready<-v;<-release;runtime.Gosched();done<-v})}}
func panicDriver(seed int)int {f:=func(x int)(result int){defer func(){if recover()!=nil {result=seed+x};seed++}();runtime.Gosched();panic("callback")};return invoke(f,2)+invoke(f,3)}
func exitDriver(done chan struct{},result *int) {seed:=41;invokeVoid(func(){defer func(){*result=seed;close(done)}();runtime.Gosched();runtime.Goexit()});panic("continued after callback Goexit")}
type object struct {n int;pad [2048]byte}
func held(p *object,ready,release chan int)int {return invoke(func(x int)int{ready<-p.n;<-release;runtime.Gosched();return p.n+x},2)}
func main(){
 if direct(10)!=12||pureDriver()!=27||instances()!=114||readonlyDriver()!=115||selected(false)!=13||selected(true)!=7||recursiveDriver()!=44||panicDriver(10)!=26 {panic("captured callback result or environment")}
 c:=make(chan int,2);c<-1;c<-1;f:=outer(10,c);if f(2)!=13||f(3)!=16 {panic("captured caller callback specialization")}
 if nilDriver(false)!=12||operands!=1 {panic("callback argument order")};if _,ok:=nilDriver(true).(runtime.Error);!ok||operands!=2 {panic("nil callback operand snapshot")}
 ready,release,done:=make(chan int),make(chan int),make(chan int);before:=runtime.NumGoroutine();spawn(ready,release,done);total:=0;for range 64 {total+=<-ready};if total!=2016 {panic("callback spawned cell input")};if NATIVE && runtime.NumGoroutine()>before+16 {panic("ordinary goroutine per callback child")};runtime.GC();for range 64 {release<-1};total=0;for range 64 {total+=<-done};if total!=2016 {panic("callback spawned cell output")}
 go func(){if <-ready!=23 {panic("captured callback GC input")};runtime.GC();release<-1}();if held(&object{n:23},ready,release)!=25 {panic("captured callback GC result")}
 exited:=make(chan struct{});result:=0;go exitDriver(exited,&result);<-exited;if result!=41 {panic("callback Goexit cleanup")}
 runtime.LockOSThread();if selected(true)!=7||pureDriver()!=27 {panic("ordinary callback fallback on locked thread")};runtime.UnlockOSThread()
 println("captured-parameters-ok")
}`
	for _, config := range []struct{ name, experiment, flags string }{
		{"native", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2"},
		{"ordinary", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "main.go")
			program := strings.ReplaceAll(source, "NATIVE", map[bool]string{true: "true", false: "false"}[config.experiment == "coro"])
			if err := os.WriteFile(path, []byte(program), 0600); err != nil {
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
				for _, name := range []string{"direct", "pureDriver", "pure.func1", "instances", "readonlyDriver", "readonly.func1", "selected", "outer.func1", "nilDriver", "recursiveDriver", "spawn", "spawn.func1", "panicDriver", "exitDriver", "held"} {
					if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission\n%s", name, out)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "captured-parameters-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
