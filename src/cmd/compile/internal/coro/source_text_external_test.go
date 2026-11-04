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

func sourceTextFixture() string {
	source := strings.Replace(sourceFrameProgram, `"path/filepath"`, `"path/filepath";"strings";"strconv"`, 1)
	start := strings.Index(source, "func inspect(){")
	end := strings.Index(source[start:], "//go:noinline\nfunc root") + start
	source = source[:start] + `func inspect(){
 for _,all:=range []bool{false,true} {
  b:=make([]byte,1<<20)
  n:=runtime.Stack(b,all)
  text:=string(b[:n])
  if at:=strings.Index(text,"\ngoroutine ");at>=0 {text=text[:at]}
  lines:=strings.Split(text,"\n")
  got:=[]frame{}
  elided:=0
  for i,line:=range lines {
   if strings.HasPrefix(line,"...")&&strings.HasSuffix(line," frames elided...") {
    var err error
    elided,err=strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(line,"...")," frames elided..."))
    if err!=nil {panic(err)}
   }
   if !strings.HasPrefix(line,"main."){continue}
   at:=strings.IndexByte(line,'(')
   if at<0||i+1>=len(lines){panic("malformed source frame")}
   location:=strings.Fields(lines[i+1])[0]
   colon:=strings.LastIndexByte(location,':')
   if colon<0||filepath.Base(location[:colon])!="main.go" {panic("lost source file")}
   lineNumber,err:=strconv.Atoi(location[colon+1:])
   if err!=nil {panic(err)}
   got=append(got,frame{line[:at],lineNumber})
  }
  want:=expected
  wantElided:=0
  if len(want)>100 {
   wantElided=len(want)-100
   want=append(append([]frame{},want[:50]...),want[len(want)-50:]...)
  }
  if elided!=wantElided||len(got)!=len(want) {
   print(text);println("counts",elided,len(got),"want",wantElided,len(want));panic("source text frame count")
  }
  for i,w:=range want {
   if got[i].name!=w.name||w.line!=0&&got[i].line!=w.line {
    print(text);println("frame",i,got[i].name,got[i].line,"want",w.name,w.line);panic("source text frame")
   }
  }
 }
}
` + source[end:]
	// A native public entry reached from an opaque host selects its ordinary
	// source body. Its physical frames must precede the outer logical chain.
	source = strings.Replace(source, "var inlineEntry=inlineOwner", `
//go:noinline
func reenter() {
 goto body
body:
 innerEntry(inspect)
}
//go:noinline
func detached(f func(),done chan struct{}) {
 runtime.Gosched()
 f()
 close(done)
}
//go:noinline
func launch(f func(),done chan struct{}) {go detached(f,done)}
var launchEntry=launch
var innerEntry=inner
var inlineEntry=inlineOwner`, 1)
	// The bridge expands past both halves of traceback's 50/50 frame limit.
	// Its count/print cursor must retain the same physical-frame accounting.
	source = strings.Replace(source, "i<8", "i<220", 1)
	source = strings.Replace(source, "recursiveEntry(8,inspect)", "recursiveEntry(220,inspect)", 1)
	source = strings.Replace(source, `println("source-frames-ok")`, `
 expected=[]frame{{"main.inspect",0},{"main.left",LEFT_LEAF_LINE}}
 for i:=0;i<110;i++ {expected=append(expected,frame{"main.right",RIGHT_CALL_LINE},frame{"main.left",LEFT_CALL_LINE})}
 expected=append(expected,frame{"main.main",0})
 leftEntry(220,inspect)
 expected=[]frame{{"main.inspect",0},{"main.inner",INNER_LINE},{"main.reenter",0},{"main.root",ROOT_LINE},{"main.main",0}}
 rootEntry(reenter)
 expected=[]frame{{"main.inspect",0},{"main.detached",0}}
 done:=make(chan struct{})
 launchEntry(inspect,done)
 <-done
 println("source-text-ok")`, 1)
	return sourceFrameFixtureFor(source)
}

func TestSourceTextAcrossOrdinaryHost(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte(sourceTextFixture()), 0600); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, experiment, flags, extra string }{
		{"native", "coro", "-l -d=coro=4", ""},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2", ""},
		{"inline", "coro", "-d=coro=4", ""},
		{"checkptr-inline", "coro", "-d=coro=4 -d=checkptr=2", ""},
		{"no-dwarf", "coro", "-d=coro=4", "-ldflags=-w"},
		{"pie", "coro", "-l -d=coro=4", "-buildmode=pie"},
		{"ordinary", "nocoro", "-l", ""},
		{"ordinary-inline", "nocoro", "", ""},
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
			if config.experiment == "coro" {
				for _, name := range []string{"root", "inner", "outer", "recursive", "left", "right", "inlineOwner", "closureSource", "launch", "detached"} {
					if linkedCoroStackUnsafe(t, exe, "main."+name+".coro.stackgate") {
						t.Errorf("source text lost native admission: %s", name)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=0")
				if data, err := run.CombinedOutput(); err != nil || strings.TrimSpace(string(data)) != "source-text-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, data)
				}
			}
		})
	}
}

// A runtime failure needs the physical bridge and runtime frames even when
// ordinary user tracebacks prefer the source chain. Force a stack overflow on
// an opaque host so that a runtime throw occurs while that chain is active.
func TestSourceTextRuntimeFailure(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const source = `package main
import ("runtime";"runtime/debug")
var sink int
//go:noinline
func recurse(n int) {
 goto body
body:
 var buffer [1024]byte
 buffer[n%len(buffer)]=byte(n)
 recurse(n+1)
 sink+=int(buffer[(n+1)%len(buffer)])
}
//go:noinline
func crash() {
 goto body
body:
 debug.SetMaxStack(32<<10)
 recurse(0)
}
//go:noinline
func root(f func()) {runtime.Gosched();f()}
var entry=root
func main(){goto body;body:entry(crash)}
`
	dir := t.TempDir()
	file, exe := filepath.Join(dir, "main.go"), filepath.Join(dir, "overflow")
	if err := os.WriteFile(file, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	build := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags=-l -d=coro=4", "-o", exe, file)
	build.Env = append(build.Environ(), "GOEXPERIMENT=coro", "GOWORK=off")
	if data, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, data)
	}
	if linkedCoroStackUnsafe(t, exe, "main.root.coro.stackgate") {
		t.Fatal("overflow owner lost native admission")
	}
	for _, p := range []string{"1", "4"} {
		run := testenv.Command(t, exe)
		run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOTRACEBACK=single")
		data, err := run.CombinedOutput()
		if err == nil {
			t.Fatal("stack overflow unexpectedly returned")
		}
		for _, want := range []string{"fatal error: stack overflow", "main.root.coro.", "runtime.coroRunFrame(", " fp=", " sp="} {
			if !strings.Contains(string(data), want) {
				t.Fatalf("P%s: physical runtime traceback missing %q:\n%s", p, want, data)
			}
		}
	}
}
