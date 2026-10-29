// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"context"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const affineReturnSource = `package main
import "runtime"
func alpha(n int)int {if n<=0{runtime.Gosched();return 3};return alpha(n-1)+7}
func beta(n int)int {if n<=0{runtime.Gosched();return 3};return gamma(n-1)+7}
func gamma(n int)int {if n<=0{runtime.Gosched();return 3};return beta(n-1)+7}
func negative(n int)int {if n<=0{runtime.Gosched();return -9};return negative(n-1)+(-7)}
func overflow(n int)int {if n<=0{runtime.Gosched();return 7};return overflow(n-1)+9223372036854775807}
var effects int
func effect(n int)int {effects++;if n<=0{runtime.Gosched();return 3};return effect(n-1)+7}
func variable(n int)int {if n<=0{runtime.Gosched();return 3};return variable(n-1)+n}
func bytwo(n int)int {if n<=0{runtime.Gosched();return 3};return bytwo(n-2)+7}
func unequalA(n int)int {if n<=0{runtime.Gosched();return 3};return unequalB(n-1)+7}
func unequalB(n int)int {if n<=0{runtime.Gosched();return 5};return unequalA(n-1)+7}
// Exercise the factory/resume entries independently of the public fast entry.
func nativeCaller(n int)int {runtime.Gosched();a:=alpha(n);b:=beta(n);c:=gamma(n);d:=negative(n);e:=overflow(0);return a+b+c+d+e}
func main(){
 if nativeCaller(17)!=245 {panic("native affine caller")}
 for _,n:=range []int{-19,0,1,2,3,17,64,513,4096}{
  count:=n;if count<0{count=0}
  if alpha(n)!=3+7*count||beta(n)!=3+7*count||gamma(n)!=3+7*count||negative(n)!=-9-7*count{panic("pure scalar/SCC result")}
  effects=0;if effect(n)!=3+7*count||effects!=count+1{panic("effectful body was summarized")}
  if variable(n)!=3+count*(count+1)/2||bytwo(n)!=3+7*((count+1)/2){panic("nonmatching return/argument was summarized")}
  want:=3+count*7;if count%2!=0{want+=2};if unequalA(n)!=want{panic("unequal SCC base was summarized")}
 }
 for _,n:=range []int{0,1,2,3,17}{u:=uint64(n)*uint64(9223372036854775807)+7;if overflow(n)!=int(u){panic("same-width integer overflow")}}
 runtime.LockOSThread();if alpha(513)!=3+7*513||beta(513)!=3+7*513{panic("ordinary locked-thread body")};runtime.UnlockOSThread()
 println("switch-return-ok")
}
`

func TestAffineReturnSource(t *testing.T) {
	testSwitchReturnProgram(t, affineReturnSource, nil, []string{"alpha", "beta", "gamma", "negative", "overflow", "effect", "variable", "bytwo", "unequalA", "unequalB"})
}

// Inspect real frontend output: value-only tests also pass when optimization
// is accidentally disabled or when ordinary fallback is incorrectly changed.
func TestAffineReturnNativeAndOrdinaryBodies(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" || runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	program := filepath.Join(dir, "affine.exe")
	if err := os.WriteFile(source, []byte(affineReturnSource), 0666); err != nil {
		t.Fatal(err)
	}
	goTool := testenv.GoToolPath(t)
	build := testenv.Command(t, goTool, "build", "-gcflags=-l -d=coro=4", "-o", program, source)
	build.Env = append(build.Environ(), "GOEXPERIMENT=coro", "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, name := range []string{"alpha", "beta", "gamma", "negative", "overflow", "effect", "variable", "bytwo", "unequalA", "unequalB"} {
		out, err := testenv.Command(t, goTool, "tool", "objdump", "-s", "^main[.]"+name+"[.]coro[.]func1$", program).CombinedOutput()
		if err != nil || len(out) == 0 {
			t.Fatalf("missing native body %s: %v %s", name, err, out)
		}
		recursive := false
		for _, symbol := range []string{"coroTakeSelfFrame", "coroAwaitSelfFrame", "coroTakeFusedFrame", "coroAwaitFusedFrame"} {
			recursive = recursive || strings.Contains(string(out), symbol)
		}
		want := name == "effect" || name == "variable" || name == "bytwo" || name == "unequalA" || name == "unequalB"
		if recursive != want {
			t.Fatalf("%s recursive frame operation=%v want %v\n%s", name, recursive, want, out)
		}
	}
	out, err := testenv.Command(t, goTool, "tool", "objdump", "-s", "^main[.]alpha$", program).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "CALL main.alpha(SB)") {
		t.Fatalf("ordinary recursive fallback lost: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "runtime.Gosched(SB)") || strings.Contains(string(out), "runtime.coroRunFrame(SB)") || strings.Contains(string(out), "runtime.coroTakeRootFrame(SB)") {
		t.Fatalf("proven public affine entry still constructs a native root: %s", out)
	}
	out, err = testenv.Command(t, goTool, "tool", "objdump", "-s", "^main[.]nativeCaller[.]coro[.]func1$", program).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "main.alpha.coro(SB)") || !strings.Contains(string(out), "main.beta.coro(SB)") {
		t.Fatalf("native caller bypassed affine factories: %v\n%s", err, out)
	}
	for _, procs := range []string{"1", "4"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		run := testenv.CommandContext(t, ctx, program)
		run.Env = append(run.Environ(), "GOMAXPROCS="+procs, "GOGC=1")
		out, err := run.CombinedOutput()
		cancel()
		if err != nil || strings.TrimSpace(string(out)) != "switch-return-ok" {
			t.Fatalf("P%s execution: %v %s", procs, err, out)
		}
	}
}
