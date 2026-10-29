// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"context"
	"internal/platform"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const spawnPressureSource = `package main
import (
 "os"
 "runtime"
 "sync/atomic"
)
var completed, sum, ready uint64
var arguments int
var panicAt = -1
type payload struct { value int }
func argument(i int) int {
 if i == panicAt { panic(42) }
 if i != arguments { panic("argument order") }; arguments++; return i
}
func worker(v *payload) {
 runtime.Gosched()
 atomic.AddUint64(&sum, uint64(v.value))
 atomic.AddUint64(&completed, 1)
}
func produce(n int, progress bool) int {
 total := 0
 for i:=0; i<n; i++ {
  go worker(&payload{argument(i)})
  total += i
  if progress && n>=1024 && i==n/2 && atomic.LoadUint64(&completed)==0 {
   panic("ready children made no progress during production")
  }
 }
 for atomic.LoadUint64(&completed)<uint64(n) { runtime.Gosched() }
 return total
}
var closureDone uint64
func closureWorker() { runtime.Gosched(); atomic.AddUint64(&closureDone, 1) }
func closureProduce(rounds, tasks int, progress bool) uint64 {
 var total uint64
 for round:=0; round<rounds; round++ {
  atomic.StoreUint64(&closureDone,0)
  for task:=0; task<tasks; task++ {
   go closureWorker()
   if progress && tasks>=1024 && task==tasks/2 && atomic.LoadUint64(&closureDone)==0 {
    panic("closure ready children made no progress during production")
   }
  }
  for atomic.LoadUint64(&closureDone)<uint64(tasks) { runtime.Gosched() }
  total+=atomic.LoadUint64(&closureDone)
 }
 return total
}
func argumentPanic() {
 defer func(){if recover()!=42 {panic("argument panic cleanup")}}()
 produce(1000,false)
 panic("missing argument panic")
}
func blockedWorker(gate <-chan struct{}) {
 atomic.AddUint64(&ready,1)
 <-gate
 atomic.AddUint64(&completed,1)
}
func blocked(n int) {
 gate:=make(chan struct{})
 for i:=0; i<n; i++ { go blockedWorker(gate) }
 for atomic.LoadUint64(&ready)<uint64(n) { runtime.Gosched() }
 close(gate)
 for atomic.LoadUint64(&completed)<uint64(n) { runtime.Gosched() }
}
func main() {
 for _,n:=range []int{0,1,127,128,129,255,256,1000,10000} {
  arguments=0;atomic.StoreUint64(&completed,0);atomic.StoreUint64(&sum,0)
  want:=n*(n-1)/2
  if produce(n,os.Getenv("EXPECT_PROGRESS")=="1")!=want || arguments!=n || atomic.LoadUint64(&sum)!=uint64(want) {
   panic("spawn values or continuation locals")
  }
 }
 for _,n:=range []int{0,1,127,128,129,255,256,1000,10000} {
  if closureProduce(3,n,os.Getenv("EXPECT_PROGRESS")=="1")!=uint64(3*n) {panic("closure continuation locals")}
 }
 arguments=0;atomic.StoreUint64(&completed,0);atomic.StoreUint64(&sum,0)
 panicAt=300;argumentPanic();panicAt=-1
 for atomic.LoadUint64(&completed)<300 {runtime.Gosched()}
 if arguments!=300 || atomic.LoadUint64(&sum)!=300*299/2 {panic("argument panic republished or lost children")}
 atomic.StoreUint64(&ready,0);atomic.StoreUint64(&completed,0)
 blocked(1000)
 if atomic.LoadUint64(&completed)!=1000 { panic("blocked task completion") }
 println("spawn-pressure-ok")
}
`

func TestSpawnReadyPressure(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if (runtime.GOOS != "darwin" && runtime.GOOS != "linux") ||
		(runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skip("requires a supported explicit-frame native target")
	}
	goTool := testenv.GoToolPath(t)
	for _, mode := range []string{"inline", "noinline", "checkptr", "nocoro", "race"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "race" {
				testenv.MustHaveCGO(t)
				if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
					t.Skip("race detector is unavailable")
				}
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(spawnPressureSource), 0o600); err != nil {
				t.Fatal(err)
			}
			program := filepath.Join(dir, "pressure")
			experiment, flags := "coro", "-d=coro=4"
			if mode != "inline" {
				flags += " -l"
			}
			if mode == "nocoro" {
				experiment, flags = "nocoro", "-l"
			}
			args := []string{"build", "-o", program, "-gcflags=" + flags}
			if mode == "checkptr" {
				args = append(args, "-gcflags=all=-d=checkptr=2", "-gcflags="+flags+" -d=checkptr=2")
			}
			if mode == "race" {
				args = append(args, "-race")
			}
			build := testenv.Command(t, goTool, append(args, filepath.Join(dir, "main.go"))...)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+experiment, "GOWORK=off")
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			native := mode != "nocoro" && mode != "race"
			out, err := testenv.Command(t, goTool, "tool", "objdump", "-s", `^main\.produce\.coro\.func1$`, program).CombinedOutput()
			if err != nil || strings.Contains(string(out), "runtime.coroSpawnFrame(SB)") != native {
				t.Fatalf("producer native path = %t: %v\n%s", native, err, out)
			}
			for _, procs := range []string{"1", "4"} {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				run := testenv.CommandContext(t, ctx, program)
				progress := "0"
				if native {
					progress = "1"
				}
				run.Env = append(run.Environ(), "GOMAXPROCS="+procs, "GOGC=1", "EXPECT_PROGRESS="+progress)
				if out, err := run.CombinedOutput(); err != nil || string(out) != "spawn-pressure-ok\n" {
					t.Fatalf("P%s: %v\n%s", procs, err, out)
				}
			}
		})
	}
}
