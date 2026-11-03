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

func TestSuspendingDeferLiterals(t *testing.T) {
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main
import "runtime"
//go:noinline
func returning(trace *[]int){defer func(){*trace=append(*trace,3)}();defer func(){*trace=append(*trace,2);runtime.Gosched();*trace=append(*trace,4)}();runtime.Gosched();*trace=append(*trace,1)}
//go:noinline
func panicking()(out any){defer func(){out=recover()}();defer func(){runtime.Gosched()}();runtime.Gosched();panic("outer")}
//go:noinline
func replacing()(out any){defer func(){out=recover()}();defer func(){runtime.Gosched();panic("replacement")}();runtime.Gosched();panic("outer")}
//go:noinline
func bounds()(out any){defer func(){out=recover()}();defer func(){runtime.Gosched();xs:=[]int{};_ =xs[1]}();runtime.Gosched();panic("outer")}
//go:noinline
func closed()(out any){defer func(){out=recover()}();ch:=make(chan int);close(ch);defer func(){ch<-1}();runtime.Gosched();panic("outer")}
//go:noinline
func exiting(done chan int){defer func(){done<-1}();defer func(){runtime.Gosched()}();runtime.Goexit()}
//go:noinline
func exitPanic(done chan int){defer func(){done<-2}();defer func(){recover()}();defer func(){runtime.Gosched();panic("cleanup")}();runtime.Goexit()}
//go:noinline
func walk(n int,p *int){if n>0{walk(n-1,p)};runtime.Gosched();*p++}
//go:noinline
func walkPanic(n int){if n>0{walkPanic(n-1)};runtime.Gosched();panic("deep")}
//go:noinline
func structuredCleanup()(n int){defer func(){walk(32,&n)}();runtime.Gosched();return}
//go:noinline
func structuredPanic()(out any){defer func(){out=recover()}();defer func(){walkPanic(32)}();runtime.Gosched();panic("outer")}
//go:noinline
func nestedPanic()(out any,n int){defer func(){out=recover()}();defer func(){defer func(){runtime.Gosched();n++}();runtime.Gosched();panic("inner")}();runtime.Gosched();panic("outer")}
//go:noinline
func repeated(done chan int){for i:=0;i<2;i++{defer func(){done<-1}()};runtime.Gosched()}
//go:noinline
func directAsync()(out any){defer func(){runtime.Gosched();out=recover()}();runtime.Gosched();panic("direct")}
func controls(){var trace []int;returning(&trace);if len(trace)!=4||trace[0]!=1||trace[1]!=2||trace[2]!=4||trace[3]!=3{panic("LIFO cleanup continuation")};if panicking()!="outer"{panic("owner panic lost")};if replacing()!="replacement"{panic("replacement lost")};if _,ok:=bounds().(runtime.Error);!ok{panic("cleanup bounds panic lost")};if _,ok:=closed().(runtime.Error);!ok{panic("cleanup closed channel panic lost")};done:=make(chan int);go exiting(done);if <-done!=1{panic("exit cleanup")};go exitPanic(done);if <-done!=2{panic("sticky exit cleanup")};go repeated(done);if <-done!=1||<-done!=1{panic("repeated fallback cleanup")};if directAsync()!="direct"{panic("direct recover fallback")};if structuredCleanup()!=33{panic("structured cleanup frames")};if structuredPanic()!="deep"{panic("structured cleanup replacement")};if out,n:=nestedPanic();out!="inner"||n!=1{panic("nested cleanup identity")}}
func main(){controls();runtime.LockOSThread();controls();runtime.UnlockOSThread();println("suspending-defer-literals-ok")}
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
			path := filepath.Join(dir, "main.go")
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "program")
			build := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+config.flags, "-o", binary, path)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			buildOut, err := build.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, buildOut)
			}
			if config.experiment == "coro" {
				for _, name := range []string{"returning", "panicking", "replacing", "bounds", "closed", "exiting", "exitPanic", "walk", "walkPanic", "structuredCleanup", "structuredPanic", "nestedPanic", "directAsync", "repeated"} {
					t.Run(name, func(t *testing.T) {
						if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
							t.Fatal("suspending defer literal lost native admission")
						}
					})
				}

			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "suspending-defer-literals-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
