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

func TestSourceIdleHostStacks(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main

import (
 "fmt"
 "os"
 "runtime"
 "strconv"
 "strings"
 "time"
)

const children = 16
type object struct { id int; padding [16]uintptr }
var finalized = make(chan int, children)
var opaqueWait func()

//go:noinline
func hold() { goto body; body: runtime.Gosched() }

//go:noinline
func recursive(n int, obj *object, ready chan int, gate chan struct{}, mode, operation int) {
 if n > 0 { recursive(n-1, obj, ready, gate, mode, operation); return }
 opaqueWait()
 ready <- obj.id
 if operation == 0 { <-gate } else { gate <- struct{}{} }
 runtime.KeepAlive(obj)
 if mode == 1 { panic("idle host") }
 if mode == 2 { runtime.Goexit() }
}

//go:noinline
func child(depth int, obj *object, ready chan int, gate chan struct{}, done chan int, mode, operation int) {
 defer func() { recover(); done <- obj.id }()
 recursive(depth, obj, ready, gate, mode, operation)
}

//go:noinline
func launch(count, depth int, objects *[children]*object, ready chan int, gate chan struct{}, done chan int, mode, operation int) {
 for id := 0; id < count; id++ { go child(depth, objects[id], ready, gate, done, mode, operation) }
}
var entry = launch
var rootEntry = child

//go:noinline
func publicChild(depth int, obj *object, ready chan int, gate chan struct{}, done chan int, mode, operation int) {
 goto body
 body: rootEntry(depth, obj, ready, gate, done, mode, operation)
}

func textHosts(text string, depth, kind int) (int, string) {
 hosts := 0
 for _, stack := range strings.Split(text, "\n\n") {
  if !strings.Contains(stack, "main.recursive(") { continue }
  hosts++
  want := depth+1
  if depth > 100 { want = 99-kind }
  if count := strings.Count(stack, "main.recursive("); count != want { return hosts, fmt.Sprintf("recursive text frames %d want %d\n%s", count, want, stack) }
  if strings.Count(stack, "main.child(") != 1 { return hosts, "lost child source boundary\n"+stack }
  if strings.Count(stack, "main.publicChild(") != kind { return hosts, "incorrect public caller suffix\n"+stack }
  for _, forbidden := range []string{".coro.", "runtime.coroRunFrame(", "runtime.(*stacklessCoroHost).drive(", "main.hold(", "main.launch("} {
   if strings.Contains(stack, forbidden) { return hosts, "private/completed/creator frame in source stack\n"+stack }
  }
 }
 return hosts, ""
}

func main() {
 goto body
 body:
 values := [4]int{}
 for i := range values { value, err := strconv.Atoi(os.Args[i+1]); if err != nil { panic(err) }; values[i] = value }
 depth, mode, operation, kind := values[0], values[1], values[2], values[3]
 leafOnly := os.Getenv("CORO_PROFILE_LEAF_ONLY") == "1"
 opaqueWait = hold
 baseline := runtime.NumGoroutine()
 ready, done := make(chan int), make(chan int)
 gate := make(chan struct{})
 var objects [children]*object
 for id := range objects {
  obj := &object{id:id}; objects[id] = obj
  runtime.SetFinalizer(obj, func(obj *object) { finalized <- obj.id })
 }
 if kind == 0 { entry(children, depth, &objects, ready, gate, done, mode, operation) } else {
  for _, obj := range objects { go publicChild(depth, obj, ready, gate, done, mode, operation) }
 }
 objects = [children]*object{}
 for i := 0; i < children; i++ { <-ready }
 // Ready publication precedes the native Wait return. Wait until every host
 // has reached its stable suspension before comparing its source stack.
 deadline := time.Now().Add(10*time.Second)
 for {
  buf := make([]byte,1<<20); n := runtime.Stack(buf,true)
  hosts, failure := textHosts(string(buf[:n]), depth, kind)
  if failure != "" { panic(failure) }
  if hosts == children { break }
  if time.Now().After(deadline) { panic(fmt.Sprintf("idle source hosts %d want %d\n%s", hosts, children, buf[:n])) }
  runtime.Gosched()
 }
 for iteration := 0; iteration < 5; iteration++ {
  runtime.GC()
  if len(finalized) != 0 { panic("suspended source frame lost its object") }
  if n := runtime.NumGoroutine(); n != baseline+children { panic(fmt.Sprintf("goroutines %d want %d",n,baseline+children)) }
  buf := make([]byte,1<<20); n := runtime.Stack(buf,true)
  hosts, failure := textHosts(string(buf[:n]),depth,kind)
  if failure != "" || hosts != children { panic(fmt.Sprintf("text hosts %d want %d: %s",hosts,children,failure)) }
  records := make([]runtime.StackRecord,baseline+children+16)
  n,ok := runtime.GoroutineProfile(records)
  if !ok || n != baseline+children { panic(fmt.Sprintf("profile count %d ok %v",n,ok)) }
  hosts = 0
  for _, record := range records[:n] {
   recursiveFrames, childFrames, publicFrames := 0,0,0
   private := false
   frames := runtime.CallersFrames(record.Stack())
   for {
    f,more := frames.Next()
    if f.Function == "main.recursive" { recursiveFrames++ }
    if f.Function == "main.child" { childFrames++ }
    if f.Function == "main.publicChild" { publicFrames++ }
    if strings.Contains(f.Function,".coro.") || f.Function == "runtime.coroRunFrame" || f.Function == "runtime.(*stacklessCoroHost).drive" || f.Function == "main.hold" || f.Function == "main.launch" { private=true }
    if !more { break }
   }
   if recursiveFrames == 0 { continue }
   hosts++
   if private { panic("profile exposed private/completed/creator frames") }
   if leafOnly && (recursiveFrames != 1 || len(record.Stack()) != 1) { panic("one-PC profile did not start at source leaf") }
   if !leafOnly && depth < 32 && (recursiveFrames != depth+1 || childFrames != 1 || publicFrames != kind) { panic("profile lost shallow source chain or public suffix") }
  }
  if hosts != children { panic(fmt.Sprintf("profile source hosts %d want %d",hosts,children)) }
  small := make([]runtime.StackRecord,baseline+children-1)
  for i := range small { small[i].Stack0[0]=1 }
  saved := append([]runtime.StackRecord(nil),small...)
  n,ok = runtime.GoroutineProfile(small)
  if ok || n != baseline+children { panic("undersized profile accepted or miscounted") }
  for i := range small { if small[i] != saved[i] { panic("undersized profile modified caller records") } }
 }
 if operation == 0 { close(gate) } else { for i:=0;i<children;i++ { <-gate } }
 for i:=0;i<children;i++ { <-done }
 deadline = time.Now().Add(10*time.Second)
 for runtime.NumGoroutine() != baseline {
  if time.Now().After(deadline) { panic("completed idle hosts retained goroutine count") }
  runtime.Gosched()
 }
 seen := [children]bool{}; count:=0
 for count < children {
  runtime.GC()
  for len(finalized)>0 { id:=<-finalized; if seen[id] { panic("duplicate finalizer") };seen[id]=true;count++ }
  if time.Now().After(deadline) { panic(fmt.Sprintf("completed hosts retain objects: finalized %d",count)) }
  runtime.Gosched()
 }
 fmt.Println("idle-host-source-ok",depth,mode,operation,kind)
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
		{"ordinary-race", "nocoro", "-l", "-race"},
		{"coro-race", "coro", "-l -d=coro=4", "-race"},
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
			native := config.experiment == "coro" && config.extra == ""
			if native {
				for _, owner := range []string{"recursive", "child", "launch"} {
					if linkedCoroStackUnsafe(t, exe, "main."+owner+".coro.stackgate") {
						t.Fatalf("%s lost native admission", owner)
					}
				}
			}
			runCase := func(depth, mode, operation, kind, p string, leafOnly bool) {
				t.Helper()
				run := testenv.Command(t, exe, depth, mode, operation, kind)
				debug, leaf := "asyncpreemptoff=0", ""
				if leafOnly {
					debug += ",profstackdepth=1"
					leaf = "1"
				}
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG="+debug, "CORO_PROFILE_LEAF_ONLY="+leaf)
				data, err := run.CombinedOutput()
				want := "idle-host-source-ok " + depth + " " + mode + " " + operation + " " + kind + "\n"
				if err != nil || string(data) != want {
					t.Fatalf("depth %s mode %s operation %s kind %s P%s leaf-only %v: %v\n%s", depth, mode, operation, kind, p, leafOnly, err, data)
				}
			}
			for _, depth := range []string{"16", "220"} {
				for _, mode := range []string{"0", "1", "2"} {
					for _, operation := range []string{"0", "1"} {
						for _, kind := range []string{"0", "1"} {
							for _, p := range []string{"1", "4"} {
								runCase(depth, mode, operation, kind, p, false)
							}
						}
					}
				}
			}
			if native {
				for _, operation := range []string{"0", "1"} {
					for _, kind := range []string{"0", "1"} {
						for _, p := range []string{"1", "4"} {
							runCase("220", "0", operation, kind, p, true)
						}
					}
				}
			}
		})
	}
}
