// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestSourceIndependentTaskEnumeration(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import("bytes";"context";"fmt";"os";"runtime";"runtime/metrics";"runtime/pprof";"strconv";"strings";"sync/atomic";"time")
const children=32
type payload struct{id int;pad [64]byte}
var finalized atomic.Int32
var opaque func()
func hold(){goto body;body:runtime.Gosched()}
//go:noinline
func recursive(depth int,ready chan int,first,second chan struct{},mode int){
 runtime.Gosched()
 if depth>0{recursive(depth-1,ready,first,second,mode);return}
 ready<-0
 <-first // PARK_FIRST
 opaque()
 ready<-1
 <-second // PARK_SECOND
 if mode==1{panic("task")};if mode==2{runtime.Goexit()}
}
//go:noinline
func child(p *payload,depth int,ready chan int,first,second chan struct{},done chan int,mode int){defer func(){recover();done<-p.id}();recursive(depth,ready,first,second,mode)}
//go:noinline
func launch(ps []*payload,depth int,ready chan int,first,second chan struct{},done chan int,mode int){
 for _,p:=range ps{
  go child(p,depth,ready,first,second,done,mode) // SOURCE_GO
 }
}
var entry=launch
func unhosted()int{samples:=[]metrics.Sample{{Name:"/sched/coro/goroutines/unhosted:goroutines"}};metrics.Read(samples);if samples[0].Value.Kind()==metrics.KindUint64{return int(samples[0].Value.Uint64())};return 0}
func gid(stack string)uint64{fields:=strings.Fields(stack);if len(fields)<2||fields[0]!="goroutine"{panic("goroutine header")};n,e:=strconv.ParseUint(fields[1],10,64);if e!=nil{panic(e)};return n}
func main(){goto body;body:
 depth,e:=strconv.Atoi(os.Args[1]);if e!=nil{panic(e)};mode,e:=strconv.Atoi(os.Args[2]);if e!=nil{panic(e)};native:=os.Getenv("EXPECT_NATIVE")=="1";opaque=hold
 baseline:=runtime.NumGoroutine();self:=make([]byte,4096);parent:=gid(string(self[:runtime.Stack(self,false)]));ready,done:=make(chan int),make(chan int);first,second:=make(chan struct{}),make(chan struct{})
 ps:=make([]*payload,children);for i:=range ps{ps[i]=&payload{id:i};runtime.SetFinalizer(ps[i],func(*payload){finalized.Add(1)})}
 ctx:=pprof.WithLabels(context.Background(),pprof.Labels("coro-enumeration","inherited"));pprof.SetGoroutineLabels(ctx)
 entry(ps,depth,ready,first,second,done,mode);ps=nil;pprof.SetGoroutineLabels(context.Background());var original map[uint64]bool
 for stage:=0;stage<2;stage++{
  for i:=0;i<children;i++{if <-ready!=stage{panic("phase mismatch")}};for i:=0;i<100;i++{runtime.Gosched()};runtime.GC();runtime.GC()
  if finalized.Load()!=0{panic("live task lost payload")};if n:=runtime.NumGoroutine();n!=baseline+children{panic(fmt.Sprintf("count %d want %d",n,baseline+children))}
  wantNative:=0;if native&&stage==0{wantNative=children};if n:=unhosted();n!=wantNative{panic(fmt.Sprintf("unhosted %d want %d",n,wantNative))}
  text:=make([]byte,2<<20);n:=runtime.Stack(text,true);ids:=make(map[uint64]bool)
  for _,stack:=range strings.Split(string(text[:n]),"\n\n"){
   current:=strings.Split(stack,"[originating from")[0];if !strings.Contains(current,"main.recursive("){continue};id:=gid(stack);if id==parent||ids[id]{panic("logical identity reused")};ids[id]=true
   want:=depth+1;if depth>100{want=99};if got:=strings.Count(current,"main.recursive(");got!=want{panic(fmt.Sprintf("source count %d want %d\n%s",got,want,stack))}
   if !strings.Contains(current,fmt.Sprintf("created by main.launch in goroutine %d\n",parent)){panic("wrong source creator:\n"+stack)}
   if line:=os.Getenv("EXPECTED_GO_LINE");line!=""&&!strings.Contains(strings.Split(current,"created by ")[1],"main.go:"+line+" "){panic("wrong creator line:\n"+current)}
   if line:=os.Getenv("EXPECTED_PARK_"+strconv.Itoa(stage));line!=""&&!strings.Contains(strings.Split(current,"main.recursive(")[1],"main.go:"+line){panic("wrong suspended source line:\n"+current)}
   if strings.Contains(current,".coro.")||strings.Contains(current,"runtime.stackless")||strings.Contains(current,"main.launch("){panic("private driver or creator leaked into stack")}
   if !strings.Contains(strings.Split(current,"\n")[0],"{\"coro-enumeration\": inherited}"){panic("independent text labels lost")}
   if strings.Contains(os.Getenv("GODEBUG"),"tracebackancestors=2")&&!strings.Contains(stack,fmt.Sprintf("[originating from goroutine %d]",parent)){panic("missing source ancestor")};if strings.Contains(os.Getenv("GODEBUG"),"tracebackancestors=2")&&!strings.Contains(stack,"main.main("){panic("missing public caller in ancestry") }
  }
  if len(ids)!=children{panic(fmt.Sprintf("source stacks %d",len(ids)))};if stage==0{original=ids}else{for id:=range ids{if !original[id]{panic("host promotion changed logical goid")}}}
  records:=make([]runtime.StackRecord,baseline+children+8);n,ok:=runtime.GoroutineProfile(records);if !ok||n!=baseline+children{panic("profile count")};sources:=0
  for _,r:=range records[:n]{frames:=runtime.CallersFrames(r.Stack());for{f,more:=frames.Next();if f.Function=="main.recursive"{sources++;break};if !more{break}}};if sources!=children{panic(fmt.Sprintf("profile sources %d",sources))}
  small:=make([]runtime.StackRecord,baseline+children-1);for i:=range small{small[i].Stack0[0]=1};saved:=append([]runtime.StackRecord(nil),small...);n,ok=runtime.GoroutineProfile(small);if ok||n!=baseline+children{panic("undersized profile count")};for i:=range small{if small[i]!=saved[i]{panic("undersized profile wrote records")}}
  var profile bytes.Buffer;if err:=pprof.Lookup("goroutine").WriteTo(&profile,1);err!=nil{panic(err)};labelCount,count:=0,0
  for _,line:=range strings.Split(profile.String(),"\n"){if strings.Contains(line," @ "){fmt.Sscanf(line,"%d @",&count)};if strings.Contains(line,"# labels: {\"coro-enumeration\":\"inherited\"}"){labelCount+=count}}
  if labelCount!=children{panic(fmt.Sprintf("inherited labels %d want %d\n%s",labelCount,children,profile.String()))}
  if stage==0{close(first)}else{close(second)}
 }
 var seen [children]bool;for i:=0;i<children;i++{id:=<-done;if id<0||id>=children||seen[id]{panic("cleanup result")};seen[id]=true}
 deadline:=time.Now().Add(10*time.Second);for runtime.NumGoroutine()!=baseline||finalized.Load()!=children||unhosted()!=0{if time.Now().After(deadline){panic(fmt.Sprintf("lifetime drain users=%d finalized=%d native=%d",runtime.NumGoroutine(),finalized.Load(),unhosted()))};runtime.GC();runtime.Gosched()}
 fmt.Println("task-enumeration-ok",depth,mode)
}
`
	lines := map[string]string{}
	for i, line := range strings.Split(program, "\n") {
		for marker, env := range map[string]string{"SOURCE_GO": "EXPECTED_GO_LINE", "PARK_FIRST": "EXPECTED_PARK_0", "PARK_SECOND": "EXPECTED_PARK_1"} {
			if strings.Contains(line, "// "+marker) {
				lines[env] = strconv.Itoa(i + 1)
			}
		}
	}
	if len(lines) != 3 {
		t.Fatal("missing source markers")
	}
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
			for _, depth := range []string{"8", "220"} {
				for _, mode := range []string{"0", "1", "2"} {
					for _, p := range []string{"1", "4"} {
						run := testenv.Command(t, exe, depth, mode)
						expect := "0"
						if config.experiment == "coro" && config.extra == "" {
							expect = "1"
						}
						run.Env = append(run.Environ(), "EXPECT_NATIVE="+expect, "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=0,tracebackancestors=2,tracebacklabels=1")
						for key, value := range lines {
							run.Env = append(run.Env, key+"="+value)
						}
						data, err := run.CombinedOutput()
						if want := "task-enumeration-ok " + depth + " " + mode + "\n"; err != nil || string(data) != want {
							t.Fatalf("depth %s mode %s P%s: %v\n%s", depth, mode, p, err, data)
						}
					}
				}
			}
		})
	}
}
