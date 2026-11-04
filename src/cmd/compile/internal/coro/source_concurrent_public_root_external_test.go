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

func TestSourceConcurrentPublicRootEnumeration(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	// Keep enumerating during active native resumes, frame transitions, host
	// promotion and completion of original public goroutines. These roots have
	// no independent go tasks; public identity and count must remain unchanged.
	// Drain each cohort before launching the next: an asynchronous result send
	// can finish before its public driver returns, so receiving all results
	// alone does not impose a bound on the number of still-live goroutines.
	const program = `package main
import("bytes";"context";"fmt";"os";"runtime";"runtime/metrics";"runtime/pprof";"runtime/trace";"strconv";"strings";"sync/atomic";"time")
const children=32
var opaque func()
var completed atomic.Bool
func hold(){goto body;body:runtime.Gosched()}
//go:noinline
func left(n int,p *int)int{runtime.Gosched();if n==0{return *p};return right(n-1,p)+1}
//go:noinline
func right(n int,p *int)int{runtime.Gosched();if n==0{return *p};return left(n-1,p)+1}
//go:noinline
func child(p *int,done chan int){if n:=left(260,p);n!=*p+260{panic("left result")};opaque();if n:=right(260,p);n!=*p+260{panic("right result")};done<-*p}
//go:noinline
func launch(done chan int){goto body;body:for i:=0;i<children;i++{p:=new(int);*p=i;go driver(p,done)}}
var entry=child
func driver(p *int,done chan int){goto body;body:entry(p,done)}
func producer(){goto body;body:baseline:=runtime.NumGoroutine();done:=make(chan int);for batch:=0;batch<24;batch++{launch(done);var seen [children]bool;for i:=0;i<children;i++{n:=<-done;if n<0||n>=children||seen[n]{panic("batch duplicate")};seen[n]=true};deadline:=time.Now().Add(10*time.Second);for runtime.NumGoroutine()!=baseline{if time.Now().After(deadline){panic("public cohort did not drain")};runtime.Gosched()}};completed.Store(true)}
func unhosted()int{samples:=[]metrics.Sample{{Name:"/sched/coro/goroutines/unhosted:goroutines"}};metrics.Read(samples);if samples[0].Value.Kind()==metrics.KindUint64{return int(samples[0].Value.Uint64())};return 0}
func main(){goto body;body:
 if path:=os.Getenv("TRACE_FILE");path!=""{f,e:=os.Create(path);if e!=nil{panic(e)};if e=trace.Start(f);e!=nil{panic(e)};defer f.Close();defer trace.Stop()}
 opaque=hold;baseline:=runtime.NumGoroutine();ctx:=pprof.WithLabels(context.Background(),pprof.Labels("coro-stress","inherited"));pprof.SetGoroutineLabels(ctx);go producer();pprof.SetGoroutineLabels(context.Background())
 records:=make([]runtime.StackRecord,256);buf:=make([]byte,2<<20);samples:=0;sawSource:=false;sawTextSource:=false;requireProfileSource:=os.Getenv("PUBLIC_ROOT_PROFILE_SOURCE")=="1";deadline:=time.Now().Add(90*time.Second)
 for !completed.Load(){if time.Now().After(deadline){panic("concurrent enumeration timeout")};if unhosted()!=0{panic("public root counted as independent task")}
  n,ok:=runtime.GoroutineProfile(records);if !ok||n<baseline||n>children+baseline+4{panic(fmt.Sprintf("profile count n=%d ok=%v",n,ok))};for _,r:=range records[:n]{if len(r.Stack())==0{panic("empty logical profile")};frames:=runtime.CallersFrames(r.Stack());for{f,more:=frames.Next();if f.Function=="main.left"||f.Function=="main.right"{sawSource=true};if strings.Contains(f.Function,".coro."){panic("private resume in profile: "+f.Function)};if !more{break}}}
  size:=runtime.Stack(buf,true);if size==len(buf){panic("stack capacity")};text:=string(buf[:size]);if strings.Contains(text,"main.left(")||strings.Contains(text,"main.right("){sawTextSource=true};ids:=make(map[string]bool);for _,line:=range strings.Split(string(buf[:size]),"\n"){if !strings.HasPrefix(line,"goroutine "){continue};fields:=strings.Fields(line);id:=fields[1];if _,err:=strconv.ParseUint(id,10,64);err!=nil{panic(err)};if ids[id]{panic("duplicate public ID during promotion")};ids[id]=true}
  if samples%8==0{runtime.GC();var p bytes.Buffer;if err:=pprof.Lookup("goroutine").WriteTo(&p,1);err!=nil{panic(err)}};samples++;runtime.Gosched()
 }
 if !sawTextSource||requireProfileSource&&!sawSource{panic(fmt.Sprintf("never observed public source activations: text=%v profile=%v requireProfile=%v",sawTextSource,sawSource,requireProfileSource))};deadline=time.Now().Add(10*time.Second);for runtime.NumGoroutine()!=baseline||unhosted()!=0{if time.Now().After(deadline){panic("registry did not drain")};runtime.GC();runtime.Gosched()};fmt.Println("concurrent-public-root-ok",samples)
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
			native := config.experiment != "nocoro" && config.extra == ""
			if native {
				for _, owner := range []string{"left", "right", "child"} {
					if linkedCoroStackUnsafe(t, exe, "main."+owner+".coro.stackgate") {
						t.Fatalf("%s lost native admission", owner)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				for _, depth := range []string{"1", "128"} {
					// Ordinary Go may sample a runtime frame when only one PC fits.
					// Native roots must expose the source leaf even at that depth.
					requireProfileSource := "0"
					if native || depth != "1" {
						requireProfileSource = "1"
					}
					run := testenv.Command(t, exe)
					run.Env = append(run.Environ(), "TRACE_FILE=", "PUBLIC_ROOT_PROFILE_SOURCE="+requireProfileSource, "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=0,profstackdepth="+depth)
					data, err := run.CombinedOutput()
					if err != nil || !strings.HasPrefix(string(data), "concurrent-public-root-ok ") {
						t.Fatalf("P%s profile depth %s: %v\n%s", p, depth, err, data)
					}
				}
			}
		})
	}
}
