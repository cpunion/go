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

func TestSourceIndependentHostEnumeration(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import("fmt";"os";"runtime";"strconv";"strings";"time")
var opaqueWait func(int,chan int,chan struct{},int)
//go:noinline
func hold(id int,ready chan int,gate chan struct{},mode int){goto body;body:ready<-id;<-gate;if mode==1{panic("host")};if mode==2{runtime.Goexit()}}
//go:noinline
func recursive(n,id int,ready chan int,gate chan struct{},mode int){runtime.Gosched();if n>0{recursive(n-1,id,ready,gate,mode);return};opaqueWait(id,ready,gate,mode)}
//go:noinline
func child(depth,id int,ready chan int,gate chan struct{},done chan int,mode int){defer func(){recover();done<-id}();recursive(depth,id,ready,gate,mode)}
//go:noinline
func launch(count,depth int,ready chan int,gate chan struct{},done chan int,mode int){for id:=0;id<count;id++{go child(depth,id,ready,gate,done,mode)}}
var entry=launch
func main(){goto body;body:
 depth,err:=strconv.Atoi(os.Args[1]);if err!=nil{panic(err)};mode,err:=strconv.Atoi(os.Args[2]);if err!=nil{panic(err)}
 opaqueWait=hold;const children=16;baseline:=runtime.NumGoroutine();ready:=make(chan int);gate:=make(chan struct{});done:=make(chan int)
 entry(children,depth,ready,gate,done,mode);for i:=0;i<children;i++{<-ready}
 for iteration:=0;iteration<5;iteration++{
  runtime.GC()
  if n:=runtime.NumGoroutine();n!=baseline+children{panic(fmt.Sprintf("goroutines %d want %d",n,baseline+children))}
  buf:=make([]byte,1<<20);n:=runtime.Stack(buf,true);text:=string(buf[:n]);hosts:=0
  for _,stack:=range strings.Split(text,"\n\n"){
   if !strings.Contains(stack,"main.hold("){continue};hosts++
   want:=depth+1;if depth>100{want=98}
   if count:=strings.Count(stack,"main.recursive(");count!=want{panic(fmt.Sprintf("source frames %d want %d\n%s",count,want,stack))}
   if strings.Contains(stack,".coro.")||strings.Contains(stack,"runtime.coroRunFrame(")||strings.Contains(stack,"main.launch("){panic("host stack leaked compiler driver or creator frames")}
  }
  if hosts!=children{panic(fmt.Sprintf("text hosts %d want %d",hosts,children))}
  records:=make([]runtime.StackRecord,baseline+children+16);n,ok:=runtime.GoroutineProfile(records);if !ok||n!=baseline+children{panic(fmt.Sprintf("profile count %d ok %v",n,ok))};hosts=0
  for _,record:=range records[:n]{held,source:=false,false;frames:=runtime.CallersFrames(record.Stack());for{f,more:=frames.Next();if f.Function=="main.hold"{held=true};if f.Function=="main.recursive"{source=true};if !more{break}};if held{hosts++;if !source{panic("profile lost logical source chain")}}}
  if hosts!=children{panic(fmt.Sprintf("profile hosts %d want %d",hosts,children))}
  small:=make([]runtime.StackRecord,baseline+children-1);for i:=range small{small[i].Stack0[0]=1};saved:=append([]runtime.StackRecord(nil),small...);n,ok=runtime.GoroutineProfile(small);if ok||n!=baseline+children{panic("undersized profile accepted or miscounted")};for i:=range small{if small[i]!=saved[i]{panic("undersized profile modified caller records")}}
 }
 close(gate);for i:=0;i<children;i++{<-done};end:=time.Now().Add(10*time.Second);for runtime.NumGoroutine()!=baseline{if time.Now().After(end){panic("completed hosts retained user goroutine count")};runtime.Gosched()}
 fmt.Println("host-enumeration-ok",depth,mode)
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
			if config.experiment == "coro" && config.extra == "" {
				for _, owner := range []string{"recursive", "child", "launch"} {
					if linkedCoroStackUnsafe(t, exe, "main."+owner+".coro.stackgate") {
						t.Fatalf("%s lost native admission", owner)
					}
				}
			}
			for _, depth := range []string{"16", "220"} {
				for _, mode := range []string{"0", "1", "2"} {
					for _, p := range []string{"1", "4"} {
						run := testenv.Command(t, exe, depth, mode)
						run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=0")
						data, err := run.CombinedOutput()
						if want := "host-enumeration-ok " + depth + " " + mode + "\n"; err != nil || string(data) != want {
							t.Fatalf("depth %s mode %s P%s: %v\n%s", depth, mode, p, err, data)
						}
					}
				}
			}
		})
	}
}
