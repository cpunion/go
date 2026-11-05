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

func TestSourcePublicRootEnumeration(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main

import (
 "bytes"
 "context"
 "fmt"
 "os"
 "runtime"
 "runtime/metrics"
 "runtime/pprof"
 "strconv"
 "strings"
 "sync/atomic"
 "time"
)

const roots=16
type payload struct { id int; pad [64]byte }
var finalized atomic.Int32
var opaque func()

//go:noinline
func hold() {
 goto body
 body:
 pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(),pprof.Labels("public-root","promoted")))
 runtime.Gosched()
}

//go:noinline
func recursive(depth int,ready chan int,first,second chan struct{},mode int) {
 runtime.Gosched()
 if depth>0 { recursive(depth-1,ready,first,second,mode);return }
 ready<-0
 <-first // PARK_FIRST
 opaque()
 ready<-1
 <-second // PARK_SECOND
 if mode==1 { panic("public root") }
 if mode==2 { runtime.Goexit() }
}

//go:noinline
func root(p *payload,depth int,ready chan int,first,second chan struct{},done chan int,mode int) {
 defer func(){ recover();done<-p.id }()
 recursive(depth,ready,first,second,mode)
}
var entry=root
//go:noinline
func makeEntry(bias int)func(*payload,int,chan int,chan struct{},chan struct{},chan int,int) {
 return func(p *payload,depth int,ready chan int,first,second chan struct{},done chan int,mode int) {
  if bias!=42 {panic("lost public closure capture")}
  root(p,depth,ready,first,second,done,mode)
 }
}

func gid(stack string) uint64 {
 fields:=strings.Fields(stack)
 if len(fields)<2 || fields[0]!="goroutine" { panic("goroutine header") }
 id,e:=strconv.ParseUint(fields[1],10,64);if e!=nil { panic(e) };return id
}

//go:noinline
func driver(p *payload,depth int,started chan uint64,ready chan int,first,second chan struct{},done chan int,mode int) {
 goto body
 body:
 self:=make([]byte,4096)
 started<-gid(string(self[:runtime.Stack(self,false)]))
 entry(p,depth,ready,first,second,done,mode)
}

func unhosted() uint64 {
 samples:=[]metrics.Sample{{Name:"/sched/coro/goroutines/unhosted:goroutines"}}
 metrics.Read(samples)
 if samples[0].Value.Kind()==metrics.KindUint64 { return samples[0].Value.Uint64() }
 return 0
}

func main() {
 goto body
 body:
 depth,e:=strconv.Atoi(os.Args[1]);if e!=nil { panic(e) }
 mode,e:=strconv.Atoi(os.Args[2]);if e!=nil { panic(e) }
 leafOnly:=os.Getenv("LEAF_ONLY")=="1"
 closure:=0;if os.Getenv("PUBLIC_ROOT_CLOSURE")=="1" {entry=makeEntry(42);closure=1}
 opaque=hold
 baseline:=runtime.NumGoroutine()
 self:=make([]byte,4096);parent:=gid(string(self[:runtime.Stack(self,false)]))
 started,ready,done:=make(chan uint64),make(chan int),make(chan int)
 first,second:=make(chan struct{}),make(chan struct{})
 pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(),pprof.Labels("public-root","inherited")))
 for i:=0;i<roots;i++ {
  p:=&payload{id:i};runtime.SetFinalizer(p,func(*payload){finalized.Add(1)})
  go driver(p,depth,started,ready,first,second,done,mode) // SOURCE_GO
 }
 pprof.SetGoroutineLabels(context.Background())
 original:=make(map[uint64]bool)
 for i:=0;i<roots;i++ { id:=<-started;if id==parent||original[id] { panic("original identity duplicate") };original[id]=true }
 for stage:=0;stage<2;stage++ {
  for i:=0;i<roots;i++ { if <-ready!=stage { panic("phase mismatch") } }
  for i:=0;i<100;i++ { runtime.Gosched() }
  for iteration:=0;iteration<3;iteration++ {
   runtime.GC()
   if finalized.Load()!=0 { panic("live public root lost payload") }
   // NumGoroutine sums concurrently changing runtime counters. As in
   // runtime's TestNumGoroutine, allow bounded convergence; all exact
   // profile counts and source identity assertions below remain required.
   for attempt:=0;;attempt++ {
    got:=runtime.NumGoroutine()
    if got==baseline+roots {break}
    if attempt==9 {panic(fmt.Sprintf("count %d want %d after 10 observations",got,baseline+roots))}
    runtime.Gosched()
   }
   if unhosted()!=0 { panic("public root incremented independent-task count") }
   text:=make([]byte,2<<20);n:=runtime.Stack(text,true);ids:=make(map[uint64]bool)
   for _,stack:=range strings.Split(string(text[:n]),"\n\n") {
    current:=strings.Split(stack,"[originating from")[0]
    if !strings.Contains(current,"main.recursive(") { continue }
    id:=gid(stack);if !original[id]||ids[id] { panic("public identity changed or duplicated") };ids[id]=true
    header:=strings.Split(current,"\n")[0];label:="inherited";if stage==1 {label="promoted"}
    if strings.Contains(os.Getenv("GODEBUG"),"tracebacklabels=1")&&!strings.Contains(header,"{\"public-root\": "+label+"}") {panic("public text labels lost or stale: "+header)}
    if strings.Contains(os.Getenv("GODEBUG"),"tracebacklabels=0")&&strings.Contains(header,"public-root") {panic("disabled public text labels remain visible")}

    want:=depth+1;if depth>100 { want=98-closure }
    if got:=strings.Count(current,"main.recursive(");got!=want { panic(fmt.Sprintf("recursive text frames %d want %d\n%s",got,want,stack)) }
    if strings.Count(current,"main.root(")!=1||strings.Count(current,"main.driver(")!=1 { panic("public root/caller lost or duplicated:\n"+stack) }
    if strings.Count(current,"main.makeEntry.func1(")!=closure {panic("public closure alias lost or duplicated:\n"+stack)}
    if strings.Contains(current,".coro.")||strings.Contains(current,"runtime.stackless")||strings.Contains(current,"runtime.coroRun")||strings.Contains(current,"main.hold(") { panic("private/completed source frame:\n"+stack) }
    if !strings.Contains(current,fmt.Sprintf("created by main.main in goroutine %d\n",parent)) { panic("public creator changed:\n"+stack) }
    if line:=os.Getenv("EXPECTED_GO_LINE");line!=""&&!strings.Contains(strings.Split(current,"created by ")[1],"main.go:"+line+" ") { panic("wrong creator line:\n"+current) }
    if line:=os.Getenv("EXPECTED_PARK_"+strconv.Itoa(stage));line!=""&&!strings.Contains(strings.Split(current,"main.recursive(")[1],"main.go:"+line) { panic("wrong suspended source line:\n"+current) }
    if strings.Contains(os.Getenv("GODEBUG"),"tracebackancestors=2")&&!strings.Contains(stack,fmt.Sprintf("[originating from goroutine %d]",parent)) { panic("public ancestry lost") }
   }
   if len(ids)!=roots { panic(fmt.Sprintf("public source stacks %d want %d\n%s",len(ids),roots,text[:n])) }
   records:=make([]runtime.StackRecord,baseline+roots+8)
   n,ok:=runtime.GoroutineProfile(records);if !ok||n!=baseline+roots { panic("profile count") }
   sources:=0
   for _,record:=range records[:n] {
    frames:=runtime.CallersFrames(record.Stack());recursiveFrames,rootFrames,driverFrames:=0,0,0
    for { f,more:=frames.Next();switch f.Function {case "main.recursive":recursiveFrames++;case "main.root":rootFrames++;case "main.driver":driverFrames++};if !more {break} }
    if recursiveFrames==0 {continue};sources++
    if leafOnly&&(recursiveFrames!=1||len(record.Stack())!=1) { panic("one-PC public profile lost leaf") }
    if !leafOnly&&depth<32&&(recursiveFrames!=depth+1||rootFrames!=1||driverFrames!=1) { panic("public shallow profile chain") }
   }
   if sources!=roots { panic(fmt.Sprintf("public profile sources %d",sources)) }
   small:=make([]runtime.StackRecord,baseline+roots-1);for i:=range small {small[i].Stack0[0]=1};saved:=append([]runtime.StackRecord(nil),small...)
   n,ok=runtime.GoroutineProfile(small);if ok||n!=baseline+roots {panic("undersized profile count")};for i:=range small {if small[i]!=saved[i] {panic("undersized profile wrote records")}}
   var profile bytes.Buffer
   if e:=pprof.Lookup("goroutine").WriteTo(&profile,1);e!=nil {panic(e)}
   label:="inherited";if stage==1 {label="promoted"};counted:=0
   for _,group:=range strings.Split(profile.String(),"\n\n") {
    if !strings.Contains(group,"main.recursive+") {continue}
    header:="";for _,line:=range strings.Split(group,"\n") {if strings.Contains(line," @ ") {header=line;break}}
    var count int;if _,e:=fmt.Sscanf(header,"%d @",&count);e!=nil {panic(e)};counted+=count
    if !strings.Contains(group,"# labels: {\"public-root\":\""+label+"\"}") {panic("public labels lost or stale:\n"+group)}
    want:=depth+1;if leafOnly {want=1}
    if got:=strings.Count(group,"main.recursive+");got!=want {panic(fmt.Sprintf("full profile recursive frames %d want %d\n%s",got,want,group))}
    if !leafOnly&&(strings.Count(group,"main.root+")!=1||strings.Count(group,"main.driver+")!=1) {panic("full profile public suffix lost or duplicated")}
    wantClosure:=closure;if leafOnly {wantClosure=0};if strings.Count(group,"main.makeEntry.func1+")!=wantClosure {panic("public closure profile alias lost or duplicated")}
    if strings.Contains(group,".coro.")||strings.Contains(group,"runtime.stackless")||strings.Contains(group,"runtime.coroRun") {panic("private full profile frame")}
   }
   if counted!=roots {panic(fmt.Sprintf("labelled public profile count %d\n%s",counted,profile.String()))}
  }
  if stage==0 {close(first)} else {close(second)}
 }
 var seen [roots]bool
 for i:=0;i<roots;i++ {id:=<-done;if id<0||id>=roots||seen[id] {panic("cleanup result")};seen[id]=true}
 deadline:=time.Now().Add(10*time.Second)
 for runtime.NumGoroutine()!=baseline||finalized.Load()!=roots||unhosted()!=0 {
  if time.Now().After(deadline) {panic(fmt.Sprintf("public drain users=%d finalized=%d native=%d",runtime.NumGoroutine(),finalized.Load(),unhosted()))}
  runtime.GC();runtime.Gosched()
 }
 fmt.Println("public-root-source-ok",depth,mode)
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
	if e := os.WriteFile(file, []byte(program), 0600); e != nil {
		t.Fatal(e)
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
			exe := filepath.Join(dir, config.name)
			args := []string{"build", "-gcflags=" + config.flags, "-o", exe}
			if config.extra != "" {
				args = append(args, config.extra)
			}
			build := testenv.Command(t, testenv.GoToolPath(t), append(args, file)...)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if data, e := build.CombinedOutput(); e != nil {
				t.Fatalf("build: %v\n%s", e, data)
			}
			native := strings.Contains(config.experiment, "coro") && config.experiment != "nocoro" && config.extra == ""
			if native {
				for _, owner := range []string{"root", "recursive", "makeEntry.func1"} {
					if linkedCoroStackUnsafe(t, exe, "main."+owner+".coro.stackgate") {
						t.Fatalf("%s lost native admission", owner)
					}
				}
			}
			runCase := func(depth, mode, p string, leafOnly, closure bool) {
				t.Helper()
				run := testenv.Command(t, exe, depth, mode)
				debug, leaf := "asyncpreemptoff=0,tracebackancestors=2,tracebacklabels=1,profstackdepth=1024", "0"
				if leafOnly {
					debug = "asyncpreemptoff=0,tracebackancestors=2,tracebacklabels=0,profstackdepth=1"
					leaf = "1"
				}
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG="+debug, "LEAF_ONLY="+leaf)
				kind := "0"
				if closure {
					kind = "1"
				}
				run.Env = append(run.Env, "PUBLIC_ROOT_CLOSURE="+kind)
				for key, value := range lines {
					run.Env = append(run.Env, key+"="+value)
				}
				data, e := run.CombinedOutput()
				want := "public-root-source-ok " + depth + " " + mode + "\n"
				if e != nil || string(data) != want {
					t.Fatalf("depth %s mode %s P%s leaf-only %v closure %v: %v\n%s", depth, mode, p, leafOnly, closure, e, data)
				}
			}
			for _, depth := range []string{"8", "220"} {
				for _, mode := range []string{"0", "1", "2"} {
					for _, p := range []string{"1", "4"} {
						runCase(depth, mode, p, false, false)
					}
				}
			}
			if native {
				for _, p := range []string{"1", "4"} {
					runCase("220", "0", p, true, false)
				}
			}
			if config.extra == "" {
				for _, mode := range []string{"0", "1", "2"} {
					for _, p := range []string{"1", "4"} {
						runCase("220", mode, p, false, true)
					}
				}
				if native {
					for _, p := range []string{"1", "4"} {
						runCase("220", "0", p, true, true)
					}
				}
			}
		})
	}
}
