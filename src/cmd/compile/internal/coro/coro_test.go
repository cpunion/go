// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"context"
	"fmt"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const testProgram = `package p

import (
	"runtime"
	"time"
)

var ch chan int

func leaf() {
	ch <- 1
}

func receive() {
	<-ch
}

func direct() {
	leaf()
}

func deferred() {
	defer leaf()
}

func launched() {
	go leaf()
}

func cycleA() {
	cycleB()
}

func cycleB() {
	if len(ch) == 0 {
		cycleA()
	}
	<-ch
}

func channelRange() {
	for range ch {
	}
}

func selecting() {
	select {
	case <-ch:
	default:
	}
}

func dynamic(f func()) {
	f()
}

func launchedDynamic(f func()) {
	go f()
}

func yielding() {
	runtime.Gosched()
}

func yieldCaller() {
	yielding()
}

func sleeping() {
	time.Sleep(0)
}

func panicking() {
	panic("panic")
}

func panicCaller() {
	panicking()
}

func panicLaunched() {
	go panicking()
}

func goexiting() {
	runtime.Gosched()
	runtime.Goexit()
}
`

func compile(t *testing.T, experiment string, debug int) (string, error) {
	t.Helper()
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "p.go")
	if err := os.WriteFile(src, []byte(testProgram), 0o666); err != nil {
		t.Fatal(err)
	}

	importcfg := filepath.Join(tmp, "importcfg")
	list := testenv.Command(t, testenv.GoToolPath(t), "list", "-export",
		"-f=packagefile {{.ImportPath}}={{.Export}}", "runtime", "time")
	list.Env = append(list.Environ(),
		"GOEXPERIMENT="+experiment,
		"GOCACHE="+filepath.Join(tmp, "gocache"),
	)
	data, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("listing imports failed: %v\n%s", err, data)
	}
	if err := os.WriteFile(importcfg, data, 0o666); err != nil {
		t.Fatal(err)
	}

	args := []string{"tool", "compile", "-l", "-p=p", "-importcfg=" + importcfg,
		"-o", filepath.Join(tmp, "p.o")}
	if debug != 0 {
		args = append(args, fmt.Sprintf("-d=coro=%d", debug))
	}
	args = append(args, src)

	cmd := testenv.Command(t, testenv.GoToolPath(t), args...)
	cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+experiment)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestAnalysis(t *testing.T) {
	out, err := compile(t, "coro", 1)
	if err != nil {
		t.Fatalf("compile failed: %v\n%s", err, out)
	}

	for _, want := range []string{
		"coro: func=p.leaf effect=may-suspend local=may-suspend recursive=false seeds=channel-send",
		"coro: func=p.receive effect=may-suspend local=may-suspend recursive=false seeds=channel-receive",
		"coro: func=p.direct effect=may-suspend local=nosuspend recursive=false seeds=-",
		"coro: func=p.deferred effect=may-suspend local=nosuspend recursive=false seeds=-",
		"coro: func=p.launched effect=nosuspend local=nosuspend recursive=false seeds=-",
		"coro: func=p.cycleA effect=may-suspend local=nosuspend recursive=true seeds=-",
		"coro: func=p.cycleB effect=may-suspend local=may-suspend recursive=true seeds=channel-receive",
		"coro: func=p.channelRange effect=may-suspend local=may-suspend recursive=false seeds=channel-range",
		"coro: func=p.selecting effect=may-suspend local=may-suspend recursive=false seeds=channel-receive,channel-select",
		"coro: func=p.dynamic effect=may-suspend local=may-suspend recursive=false seeds=unknown-call",
		"coro: func=p.launchedDynamic effect=nosuspend local=nosuspend recursive=false seeds=-",
		"coro: func=p.yielding effect=may-suspend local=may-suspend recursive=false seeds=scheduler-yield primary=coro",
		"coro: func=p.yieldCaller effect=may-suspend local=nosuspend recursive=false seeds=- primary=coro",
		"coro: func=p.sleeping effect=may-suspend local=may-suspend recursive=false seeds=timer-wait primary=coro",
		"coro: func=p.panicking effect=nosuspend local=nosuspend recursive=false seeds=- primary=coro exec=- terminal=panic local-terminal=panic",
		"coro: func=p.panicCaller effect=nosuspend local=nosuspend recursive=false seeds=- primary=coro exec=- terminal=panic local-terminal=-",
		"coro: func=p.panicLaunched effect=nosuspend local=nosuspend recursive=false seeds=- primary=plain exec=- terminal=- local-terminal=-",
		"coro: func=p.goexiting effect=may-suspend local=may-suspend recursive=false seeds=scheduler-yield primary=coro exec=- terminal=goexit local-terminal=goexit",
		"coro: site=1 func=p.yielding kind=yield foreign=-",
		"coro: site=1 func=p.yieldCaller kind=await foreign=-",
		"coro: site=1 func=p.sleeping kind=timer foreign=-",
		"coro: site=1 func=p.panicking kind=panic foreign=-",
		"coro: site=1 func=p.panicCaller kind=await foreign=-",
		"coro: site=1 func=p.panicLaunched kind=spawn foreign=-",
		"coro: edge=direct caller=p.direct callee=p.leaf unknown=false",
		"coro: edge=defer caller=p.deferred callee=p.leaf unknown=false",
		"coro: edge=go caller=p.launched callee=p.leaf unknown=false",
		"coro: edge=go caller=p.launchedDynamic callee=<dynamic> unknown=true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}
}

func TestExperimentGate(t *testing.T) {
	out, err := compile(t, "nocoro", 1)
	if err == nil {
		t.Fatalf("compile unexpectedly succeeded\n%s", out)
	}
	if want := "-d=coro requires GOEXPERIMENT=coro"; !strings.Contains(out, want) {
		t.Fatalf("output does not contain %q\n%s", want, out)
	}

	out, err = compile(t, "nocoro", 0)
	if err != nil {
		t.Fatalf("compile without experiment failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "coro:") {
		t.Fatalf("disabled experiment produced analysis output\n%s", out)
	}
}

func TestPreLowerHandoff(t *testing.T) {
	out, err := compile(t, "coro", 3)
	if err != nil {
		t.Fatalf("compile failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"coro: phase=pre-lower-ssa func=",
		" action=continue-native",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}
}

func TestYieldLowering(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import (
	"net"
	"os"
	"runtime"
	corort "runtime/coro"
	"syscall"
	"time"
)

var trace [2]int
var next int
var result int
var childStage int
var parentSaw int
var spawnStage int
var spawnSaw int
var slept bool
var fileFD int
var fileBuffer = make([]byte, 4)
var fileN int
var fileErrno uintptr
var ordinaryFile *os.File
var ordinaryFileBuffer = make([]byte, 4)
var ordinaryFileN int
var ordinaryFileErr error
var socketFD int
var socketBuffer = make([]byte, 4)
var socketN int
var socketErrno uintptr
var ordinarySocket *net.TCPConn
var ordinarySocketBuffer = make([]byte, 4)
var ordinarySocketN int
var ordinarySocketErr error
var ordinarySocketReadOK bool
var ordinarySocketDeadlineOK bool
var ordinarySocketCloseOK bool
var socketProgress int
var parameterResult int
var returnedResult int
var controlResult int
var nestedResult int
var nestedElseResult int
var nestedReturnResult int
var nestedEvaluationCount int

//go:noinline
func machine() {
	value := new(int)
	*value = 41
	trace[next] = 1
	next++
	runtime.Gosched()
	runtime.Gosched()
	result = *value + 1
	trace[next] = 2
	next++
}

//go:noinline
func child() {
	childStage = 1
	runtime.Gosched()
	childStage = 2
}

//go:noinline
func parent() {
	child()
	parentSaw = childStage
}

//go:noinline
func spawnedChild() {
	spawnStage = 1
	runtime.Gosched()
	spawnStage = 2
}

//go:noinline
func spawner() {
	go spawnedChild()
	runtime.Gosched()
	runtime.Gosched()
	spawnSaw = spawnStage
}

//go:noinline
func sleeper() {
	time.Sleep(5 * time.Millisecond)
	slept = true
}

//go:noinline
func fileReader() {
	corort.FileRead(fileFD, fileBuffer, &fileN, &fileErrno)
}

//go:noinline
func socketReader() {
	corort.SocketRead(socketFD, socketBuffer, &socketN, &socketErrno)
}

//go:noinline
func ordinaryFileReader() {
	ordinaryFileN, ordinaryFileErr = ordinaryFile.Read(ordinaryFileBuffer)
}

//go:noinline
func ordinarySocketReader() {
	go socketProgressor()
	ordinarySocketN, ordinarySocketErr = ordinarySocket.Read(ordinarySocketBuffer)
}

//go:noinline
func socketProgressor() {
	socketProgress++
	runtime.Gosched()
}

//go:noinline
func parameterized(value int) {
	runtime.Gosched()
	parameterResult = value
}

//go:noinline
func parameterCaller() {
	value := 40
	parameterized(value + 2)
}

//go:noinline
func yieldingValue(value int) int {
	runtime.Gosched()
	return value + 1
}

//go:noinline
func resultCaller() {
	returnedResult = yieldingValue(41)
}

//go:noinline
func controlled() {
	total := 0
	for i := 0; i < 4; i++ {
		if i%2 == 0 {
			total += i
		}
	}
	runtime.Gosched()
	if total == 2 {
		controlResult = 42
	}
}

//go:noinline
func nestedDelay() time.Duration {
	nestedEvaluationCount++
	return 0
}

//go:noinline
func nestedControl(limit int) int {
	total := 0
	if limit > 0 {
		runtime.Gosched()
		total++
	} else {
		runtime.Gosched()
		total--
	}
	for i := 0; i < limit; i++ {
		time.Sleep(nestedDelay())
		total += i
	}
	return total
}

//go:noinline
func nestedReturn(value int) int {
	if value < 0 {
		runtime.Gosched()
		return -1
	}
	runtime.Gosched()
	return value + 1
}

func main() {
	gcDone := make(chan struct{})
	go func() {
		runtime.GC()
		close(gcDone)
	}()
	machine()
	parent()
	spawner()
	parameterCaller()
	resultCaller()
	controlled()
	nestedResult = nestedControl(4)
	nestedElseResult = nestedControl(-1)
	nestedReturnResult = nestedReturn(41) + nestedReturn(-1)
	sleepStart := time.Now()
	sleeper()
	sleepElapsed := time.Since(sleepStart)

	file, err := os.CreateTemp("", "coro-file")
	if err != nil {
		panic(err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.WriteString("file"); err != nil {
		panic(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		panic(err)
	}
	fileFD = int(file.Fd())
	fileReader()
	if _, err := file.Seek(0, 0); err != nil {
		panic(err)
	}
	ordinaryFile = file
	ordinaryFileReader()

	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		panic(err)
	}
	defer listener.Close()
	accepted := make(chan *net.TCPConn, 1)
	go func() {
		conn, err := listener.AcceptTCP()
		if err != nil {
			panic(err)
		}
		accepted <- conn
	}()
	client, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		panic(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()
	raw, err := server.SyscallConn()
	if err != nil {
		panic(err)
	}
	if err := raw.Control(func(fd uintptr) {
		socketFD, err = syscall.Dup(int(fd))
	}); err != nil || socketFD < 0 {
		panic("dup failed")
	}
	defer syscall.Close(socketFD)
	if err := syscall.SetNonblock(socketFD, true); err != nil {
		panic(err)
	}
	go func() {
		time.Sleep(5 * time.Millisecond)
		_, _ = client.Write([]byte("poll"))
	}()
	socketReader()
	ordinarySocket = server
	go func() {
		time.Sleep(5 * time.Millisecond)
		_, _ = client.Write([]byte("net!"))
	}()
	ordinarySocketReader()
	ordinarySocketReadOK = ordinarySocketN == 4 && ordinarySocketErr == nil &&
		string(ordinarySocketBuffer) == "net!"

	if err := server.SetReadDeadline(time.Now().Add(5 * time.Millisecond)); err != nil {
		panic(err)
	}
	ordinarySocketN = -1
	ordinarySocketErr = nil
	ordinarySocketReader()
	ordinarySocketDeadlineOK = ordinarySocketN == 0 && ordinarySocketErr != nil

	if err := server.SetReadDeadline(time.Time{}); err != nil {
		panic(err)
	}
	go func() {
		time.Sleep(5 * time.Millisecond)
		_ = server.Close()
	}()
	ordinarySocketN = -1
	ordinarySocketErr = nil
	ordinarySocketReader()
	ordinarySocketCloseOK = ordinarySocketN == 0 && ordinarySocketErr != nil

	<-gcDone
	if next != 2 || trace != [2]int{1, 2} || result != 42 ||
		childStage != 2 || parentSaw != 2 ||
		spawnStage != 2 || spawnSaw != 2 ||
		parameterResult != 42 ||
		returnedResult != 42 ||
		controlResult != 42 ||
		nestedResult != 7 ||
		nestedElseResult != -1 ||
		nestedReturnResult != 41 ||
		nestedEvaluationCount != 4 ||
		!slept || sleepElapsed < 5*time.Millisecond ||
		fileN != 4 || fileErrno != 0 || string(fileBuffer) != "file" ||
		ordinaryFileN != 4 || ordinaryFileErr != nil || string(ordinaryFileBuffer) != "file" ||
		socketN != 4 || socketErrno != 0 || string(socketBuffer) != "poll" ||
		!ordinarySocketReadOK ||
		!ordinarySocketDeadlineOK ||
		!ordinarySocketCloseOK ||
		socketProgress != 3 {
		println("nested", nestedResult, nestedElseResult, nestedReturnResult, nestedEvaluationCount)
		println("basic", next, result, childStage, parentSaw, spawnStage, spawnSaw)
		panic("bad coroutine trace")
	}
	println("stackless-coro-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building lowered coroutine failed: %v\n%s", err, out)
	}
	if want := "coro: phase=lower lowered=18"; !strings.Contains(out, want) {
		t.Fatalf("output does not contain %q\n%s", want, out)
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lowered coroutine failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\..*\.coro\.func[0-9]+$`, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of resume functions failed: %v\n%s", err, data)
	}
	disassembly := string(data)
	if !strings.Contains(disassembly, ".coro.func") {
		t.Fatalf("objdump did not find generated resume functions\n%s", disassembly)
	}
	if strings.Contains(disassembly, "runtime.morestack") {
		t.Fatalf("generated resume function contains runtime.morestack\n%s", disassembly)
	}
	if !strings.Contains(disassembly, "runtime.coroTerminalAction") {
		t.Fatalf("generated resume function does not check for a recovered panic\n%s",
			disassembly)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.child\.coro\.func[0-9]+$`, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of yield-only resume failed: %v\n%s", err, data)
	}
	if strings.Contains(string(data), "runtime.coroTerminalAction") {
		t.Fatalf("yield-only resume contains a terminal query\n%s", data)
	}
}

func TestOperationProgressLowering(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import (
	"net"
	"os"
	"runtime"
	"sync/atomic"
	"time"
)

var timerProgress int32
var timerObserved int32
var timerOK bool

var fileProgress int32
var fileRescued int32
var fileReader *os.File
var fileBuffer = make([]byte, 4)
var fileN int
var fileErr error

var netProgress int32
var netRescued int32
var netReader *net.TCPConn
var netBuffer = make([]byte, 4)
var netN int
var netErr error

//go:noinline
func timerProgressor() {
	atomic.StoreInt32(&timerProgress, 1)
	runtime.Gosched()
}

//go:noinline
func timerWaiter() {
	go timerProgressor()
	time.Sleep(time.Second)
	timerOK = atomic.LoadInt32(&timerObserved) != 0
}

//go:noinline
func fileProgressor() {
	atomic.StoreInt32(&fileProgress, 1)
	runtime.Gosched()
}

//go:noinline
func fileWaiter() {
	go fileProgressor()
	fileN, fileErr = fileReader.Read(fileBuffer)
}

//go:noinline
func netProgressor() {
	atomic.StoreInt32(&netProgress, 1)
	runtime.Gosched()
}

//go:noinline
func netWaiter() {
	go netProgressor()
	netN, netErr = netReader.Read(netBuffer)
}

func main() {
	go func() {
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if atomic.LoadInt32(&timerProgress) != 0 {
				atomic.StoreInt32(&timerObserved, 1)
				return
			}
			runtime.Gosched()
		}
	}()
	timerWaiter()

	readFile, writeFile, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	defer readFile.Close()
	defer writeFile.Close()
	fileReader = readFile
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for atomic.LoadInt32(&fileProgress) == 0 && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		if atomic.LoadInt32(&fileProgress) == 0 {
			atomic.StoreInt32(&fileRescued, 1)
		}
		if _, err := writeFile.Write([]byte("file")); err != nil {
			panic(err)
		}
	}()
	fileWaiter()

	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		panic(err)
	}
	defer listener.Close()
	accepted := make(chan *net.TCPConn, 1)
	go func() {
		conn, err := listener.AcceptTCP()
		if err != nil {
			panic(err)
		}
		accepted <- conn
	}()
	client, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		panic(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()
	netReader = server
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for atomic.LoadInt32(&netProgress) == 0 && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		if atomic.LoadInt32(&netProgress) == 0 {
			atomic.StoreInt32(&netRescued, 1)
		}
		if _, err := client.Write([]byte("net!")); err != nil {
			panic(err)
		}
	}()
	netWaiter()

	if !timerOK ||
		atomic.LoadInt32(&fileRescued) != 0 ||
		fileN != 4 || fileErr != nil || string(fileBuffer) != "file" ||
		atomic.LoadInt32(&netRescued) != 0 ||
		netN != 4 || netErr != nil || string(netBuffer) != "net!" {
		panic("operation blocked stackless scheduling")
	}
	println("stackless-coro-operation-progress-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-progress")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building operation progress test failed: %v\n%s", err, out)
	}
	if want := "coro: phase=lower lowered="; !strings.Contains(out, want) {
		t.Fatalf("output does not contain %q\n%s", want, out)
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("operation progress test failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-operation-progress-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("listing operation progress symbols failed: %v\n%s", err, data)
	}
	symbols := string(data)
	for _, want := range []string{
		"main.timerWaiter.coro.func",
		"main.fileWaiter.coro.func",
		"main.netWaiter.coro.func",
	} {
		if !strings.Contains(symbols, want) {
			t.Errorf("symbols do not contain %q", want)
		}
	}
}

func TestTimerAPILowering(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import (
	"runtime"
	"sync/atomic"
	"time"
)

var (
	progress        int32
	afterFuncDone   chan struct{}
	timerPanic      any
	timerPanicCalls int
)

//go:noinline
func markProgress() {
	atomic.StoreInt32(&progress, 1)
	runtime.Gosched()
}

//go:noinline
func sleepProgress() bool {
	go markProgress()
	time.Sleep(20 * time.Millisecond)
	return atomic.LoadInt32(&progress) != 0
}

//go:noinline
func afterWait() bool {
	select {
	case value, ok := <-time.After(time.Millisecond):
		return ok && !value.IsZero()
	}
}

//go:noinline
func finishAfterFunc() {
	close(afterFuncDone)
}

// Keep the callback top-level so this test isolates timer scheduling from
// captured-local closure lowering.
//go:noinline
func afterFuncWait() bool {
	afterFuncDone = make(chan struct{})
	timer := time.AfterFunc(time.Hour, finishAfterFunc)
	if !timer.Stop() {
		return false
	}
	if timer.Reset(time.Millisecond) {
		return false
	}
	select {
	case <-afterFuncDone:
		return !timer.Stop()
	}
}

//go:noinline
func resetWait() bool {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		return false
	}
	if timer.Reset(time.Millisecond) {
		return false
	}
	select {
	case value, ok := <-timer.C:
		return ok && !value.IsZero()
	}
}

//go:noinline
func stoppedTimer() bool {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		return false
	}
	select {
	case <-timer.C:
		return false
	default:
		return true
	}
}

//go:noinline
func tickerWait() int {
	ticker := time.NewTicker(time.Millisecond)
	count := 0
	for count < 3 {
		select {
		case <-ticker.C:
			count++
			if count == 1 {
				ticker.Reset(2 * time.Millisecond)
			}
		}
	}
	ticker.Stop()
	return count
}

//go:noinline
func tickWait() bool {
	select {
	case value, ok := <-time.Tick(time.Millisecond):
		return ok && !value.IsZero()
	}
}

//go:noinline
func captureTimerPanic() {
	timerPanic = recover()
}

//go:noinline
func invalidTicker() {
	defer captureTimerPanic()
	runtime.Gosched()
	timerPanicCalls++
	time.NewTicker(0)
	timerPanicCalls += 100
}

func main() {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	invalidTicker()
	if !sleepProgress() || !afterWait() || !afterFuncWait() ||
		!resetWait() || !stoppedTimer() || tickerWait() != 3 ||
		!tickWait() ||
		timerPanic == nil ||
		timerPanicCalls != 1 {
		panic("bad stackless coroutine timer API result")
	}
	println("stackless-coro-timer-api-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-timer-api")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building timer API test failed: %v\n%s", err, out)
	}
	if want := "coro: phase=lower lowered="; !strings.Contains(out, want) {
		t.Fatalf("output does not contain %q\n%s", want, out)
	}
	for _, name := range []string{
		"markProgress", "sleepProgress", "afterWait", "afterFuncWait",
		"resetWait", "stoppedTimer", "tickerWait", "tickWait",
		"invalidTicker",
	} {
		if want := "coro: phase=pre-lower-ssa func=" + name + ".coro,"; !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("timer API test failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-timer-api-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.(sleepProgress|afterWait|afterFuncWait|resetWait|stoppedTimer|tickerWait|tickWait|invalidTicker)\.coro\.func[0-9]+$`,
		exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of timer API resumes failed: %v\n%s", err, data)
	}
	disassembly := string(data)
	for _, want := range []string{
		"runtime.coroSleep", "runtime.coroSelect",
		"runtime.coroTerminalAction", "time.AfterFunc",
		"time.NewTicker", "time.Tick",
	} {
		if !strings.Contains(disassembly, want) {
			t.Errorf("timer API resumes do not call %s\n%s", want, disassembly)
		}
	}
	for _, unwanted := range []string{"runtime.selectgo", "runtime.chanrecv"} {
		if strings.Contains(disassembly, unwanted) {
			t.Errorf("timer API resumes unexpectedly call %s\n%s",
				unwanted, disassembly)
		}
	}
}

func TestFixedDeferLowering(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import "runtime"

var trace [6]int
var traceIndex int
var evaluated int

//go:noinline
func argument(value int) int {
	evaluated = evaluated*10 + value
	return value
}

//go:noinline
func record(value int) {
	trace[traceIndex] = value
	traceIndex++
}

//go:noinline
func add(target *int, value int) {
	*target += value
}

//go:noinline
func fixed(cond bool) (result int) {
	defer record(argument(1))
	if cond {
		defer record(argument(2))
	}
	runtime.Gosched()
	defer record(argument(3))
	runtime.Gosched()
	defer add(&result, 2)
	result = 40
	return
}

//go:noinline
func early() int {
	defer record(argument(4))
	runtime.Gosched()
	return 7
}

func main() {
	first := fixed(true)
	second := fixed(false)
	earlyResult := early()
	if first != 42 || second != 42 || earlyResult != 7 ||
		evaluated != 123134 || traceIndex != len(trace) ||
		trace != [6]int{3, 2, 1, 3, 1, 4} {
		panic("bad fixed defer result")
	}
	println("stackless-coro-defer-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-defer")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building fixed defer coroutine failed: %v\n%s", err, out)
	}
	if want := "coro: phase=lower lowered=3 skipped=0"; !strings.Contains(out, want) {
		t.Fatalf("output does not contain %q\n%s", want, out)
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixed defer coroutine failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-defer-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.(fixed|early)\.coro\.func[0-9]+$`, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of fixed defer resume functions failed: %v\n%s",
			err, data)
	}
	disassembly := string(data)
	if !strings.Contains(disassembly, ".coro.func") {
		t.Fatalf("objdump did not find fixed defer resume functions\n%s",
			disassembly)
	}
	for _, forbidden := range []string{"runtime.deferproc", "runtime.deferreturn"} {
		if strings.Contains(disassembly, forbidden) {
			t.Errorf("fixed defer resume contains %q\n%s",
				forbidden, disassembly)
		}
	}
}

func TestDynamicDeferLowering(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import "runtime"

type entry struct {
	value int
}

type largeValue [32]int

var evaluated [9]int
var evaluatedIndex int
var trace [9]int
var traceIndex int
var retained int
var literalRetained int
var literalMismatch bool
var largeTrace [4]int
var largeTraceIndex int

//go:noinline
func argument(value int) *entry {
	evaluated[evaluatedIndex] = value
	evaluatedIndex++
	return &entry{value: value}
}

//go:noinline
func record(value *entry) {
	if value.value >= 1000 {
		retained += value.value - 1000
		return
	}
	trace[traceIndex] = value.value
	traceIndex++
}

//go:noinline
func recordLarge(value largeValue) {
	largeTrace[largeTraceIndex] = value[0]
	largeTraceIndex++
}

//go:noinline
func dynamic(count int, fail bool) {
	defer record(argument(10))
	for i := 0; i < count; i++ {
		defer record(argument(20 + i))
		runtime.Gosched()
	}
	defer record(argument(30))
	if fail {
		panic("dynamic defer panic")
	}
}

//go:noinline
func retain(count int) {
	for i := 0; i < count; i++ {
		value := &entry{value: 1000 + i}
		defer record(value)
		runtime.Gosched()
	}
}

//go:noinline
func retainLiteral(count int) {
	for i := 0; i < count; i++ {
		index := i
		value := &entry{value: 1000 + i}
		defer func() {
			literalRetained += value.value - 1000
			if value.value != 1000+index {
				literalMismatch = true
			}
		}()
		runtime.Gosched()
	}
}

//go:noinline
func retainLarge() {
	for i := 0; i < len(largeTrace); i++ {
		var value largeValue
		value[0] = i + 1
		defer recordLarge(value)
		value[0] = 100
		runtime.Gosched()
	}
}

//go:noinline
func collect(ready, begin, done chan struct{}) {
	close(ready)
	<-begin
	for i := 0; i < 50; i++ {
		runtime.GC()
	}
	close(done)
}

//go:noinline
func invoke() (recovered any) {
	defer func() {
		recovered = recover()
	}()
	dynamic(2, true)
	return "unreachable"
}

func main() {
	oldProcs := runtime.GOMAXPROCS(2)

	ready := make(chan struct{})
	begin := make(chan struct{})
	done := make(chan struct{})
	go collect(ready, begin, done)
	<-ready
	close(begin)
	const retainedCount = 4096
	retain(retainedCount)
	retainLiteral(retainedCount)
	retainLarge()
	<-done

	dynamic(3, false)
	recovered := invoke()
	runtime.GOMAXPROCS(oldProcs)
	if recovered != "dynamic defer panic" ||
		retained != retainedCount*(retainedCount-1)/2 ||
		literalRetained != retainedCount*(retainedCount-1)/2 ||
		literalMismatch ||
		largeTraceIndex != len(largeTrace) ||
		largeTrace != [4]int{4, 3, 2, 1} ||
		evaluatedIndex != len(evaluated) ||
		evaluated != [9]int{10, 20, 21, 22, 30, 10, 20, 21, 30} ||
		traceIndex != len(trace) ||
		trace != [9]int{30, 22, 21, 20, 10, 30, 21, 20, 10} {
		panic("bad dynamic defer result")
	}
	println("stackless-coro-dynamic-defer-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-dynamic-defer")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building dynamic defer coroutine failed: %v\n%s", err, out)
	}
	if want := "coro: phase=lower lowered=5 skipped=3"; !strings.Contains(out, want) {
		t.Fatalf("output does not contain %q\n%s", want, out)
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dynamic defer coroutine failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-dynamic-defer-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.(dynamic\.coro|invoke)\.func[0-9]+$`, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of dynamic defer resume function failed: %v\n%s",
			err, data)
	}
	disassembly := string(data)
	if !strings.Contains(disassembly, ".coro.func") {
		t.Fatalf("objdump did not find dynamic defer resume function\n%s",
			disassembly)
	}
	for _, forbidden := range []string{
		"runtime.deferproc", "runtime.deferreturn", "runtime.gopanic",
		"runtime.gorecover",
	} {
		if strings.Contains(disassembly, forbidden) {
			t.Errorf("dynamic defer resume contains %q\n%s",
				forbidden, disassembly)
		}
	}
}

func TestRepeatedLiteralCaptureLowering(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import "runtime"

type entry struct {
	value int
}

var seenValue [3]int
var seenBare [3]int
var seenBoundary [3]int
var seenPair [3][2]int
var seenObject [3]int
var seenShared [3]int
var seenResult [3]int
var seenAddress [3]bool

//go:noinline
func boundaryValue(value int) int {
	runtime.Gosched()
	return 40 + value
}

//go:noinline
func boundaryPair(value int) (int, int) {
	runtime.Gosched()
	return 50 + value, 60 + value
}

//go:noinline
func capture(shared int) (result int) {
	result = 1
	for i := 0; i < len(seenValue); i++ {
		iteration := i
		value := i
		var bare int
		boundary := boundaryValue(i)
		left, right := boundaryPair(i)
		address := &value
		object := &entry{value: i}
		defer func() {
			seenValue[iteration] = value
			seenBare[iteration] = bare
			seenBoundary[iteration] = boundary
			seenPair[iteration] = [2]int{left, right}
			seenObject[iteration] = object.value
			seenShared[iteration] = shared
			seenResult[iteration] = result
			seenAddress[iteration] = &value == address
			shared++
			result += value
		}()
		defer func() {
			value += 10
			object.value += 20
		}()
		value++
		bare = 30 + i
		object.value++
		result++
		shared++
		runtime.Gosched()
	}
	shared += 100
	return
}

func main() {
	result := capture(2)
	if result != 40 ||
		seenValue != [3]int{11, 12, 13} ||
		seenBare != [3]int{30, 31, 32} ||
		seenBoundary != [3]int{40, 41, 42} ||
		seenPair != [3][2]int{{50, 60}, {51, 61}, {52, 62}} ||
		seenObject != [3]int{21, 22, 23} ||
		seenShared != [3]int{107, 106, 105} ||
		seenResult != [3]int{29, 17, 4} ||
		seenAddress != [3]bool{true, true, true} {
		panic("bad repeated literal capture")
	}
	println("stackless-coro-repeated-literal-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-repeated-literal")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building repeated literal coroutine failed: %v\n%s",
			err, out)
	}
	if want := "coro: phase=lower lowered=4 skipped=0"; !strings.Contains(out, want) {
		t.Fatalf("output does not contain %q\n%s", want, out)
	}
	for _, diagnostic := range []string{
		"skip main.capture:", "skip main.main:",
	} {
		if strings.Contains(out, diagnostic) {
			t.Errorf("output contains unexpected %q\n%s", diagnostic, out)
		}
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("repeated literal coroutine failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-repeated-literal-ok"; !strings.Contains(
		string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.capture\.coro\.func[0-9]+$`, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of repeated literal resume function failed: %v\n%s",
			err, data)
	}
	disassembly := string(data)
	if !strings.Contains(disassembly, "capture.coro.func") {
		t.Fatalf("objdump did not find repeated literal resume function\n%s",
			disassembly)
	}
	for _, forbidden := range []string{
		"runtime.deferproc", "runtime.deferreturn", "runtime.gopanic",
		"runtime.gorecover",
	} {
		if strings.Contains(disassembly, forbidden) {
			t.Errorf("repeated literal resume contains %q\n%s",
				forbidden, disassembly)
		}
	}
}

func TestRepeatedLiteralReadResultLowering(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import (
	"os"
	"runtime"
)

var seenN [2]int
var seenErr [2]bool
var seenText [2]string

//go:noinline
func readTwice(file *os.File) {
	for i := 0; i < len(seenN); i++ {
		index := i
		buffer := make([]byte, 4)
		var n int
		var err error
		n, err = file.Read(buffer)
		defer func() {
			seenN[index] = n
			seenErr[index] = err == nil
			seenText[index] = string(buffer)
		}()
		runtime.Gosched()
	}
}

func main() {
	file, err := os.CreateTemp("", "coro-repeated-read")
	if err != nil {
		panic(err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.WriteString("abcdefgh"); err != nil {
		panic(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		panic(err)
	}
	readTwice(file)
	if seenN != [2]int{4, 4} ||
		seenErr != [2]bool{true, true} ||
		seenText != [2]string{"abcd", "efgh"} {
		panic("bad repeated read capture")
	}
	println("stackless-coro-repeated-read-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-repeated-read")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building repeated read coroutine failed: %v\n%s",
			err, out)
	}
	if want := "coro: phase=lower lowered="; !strings.Contains(out, want) {
		t.Fatalf("output does not contain %q\n%s", want, out)
	}
	if strings.Contains(out, "skip main.readTwice:") {
		t.Fatalf("readTwice was not lowered\n%s", out)
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("repeated read coroutine failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-repeated-read-ok"; !strings.Contains(
		string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.readTwice\.coro\.func[0-9]+$`, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of repeated read resume function failed: %v\n%s",
			err, data)
	}
	disassembly := string(data)
	if !strings.Contains(disassembly, "readTwice.coro.func") {
		t.Fatalf("objdump did not find repeated read resume function\n%s",
			disassembly)
	}
	for _, forbidden := range []string{
		"runtime.deferproc", "runtime.deferreturn",
	} {
		if strings.Contains(disassembly, forbidden) {
			t.Errorf("repeated read resume contains %q\n%s",
				forbidden, disassembly)
		}
	}
}

func TestPanicOutcomeLowering(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import "runtime"

var trace [3]int
var traceIndex int

//go:noinline
func record(value int) {
	trace[traceIndex] = value
	traceIndex++
}

//go:noinline
func leaf(value any) {
	defer record(1)
	runtime.Gosched()
	panic(value)
}

//go:noinline
func middle(value any) {
	defer record(2)
	leaf(value)
	panic("unreachable middle")
}

//go:noinline
func root(value any) {
	defer record(3)
	middle(value)
	panic("unreachable root")
}

//go:noinline
func immediate(value any) {
	panic(value)
}

//go:noinline
func invoke(value any) (recovered any) {
	defer func() {
		recovered = recover()
	}()
	root(value)
	return "unreachable invoke"
}

//go:noinline
func invokeImmediate(value any) (recovered any) {
	defer func() {
		recovered = recover()
	}()
	immediate(value)
	return "unreachable invokeImmediate"
}

func reset() {
	trace = [3]int{}
	traceIndex = 0
}

func main() {
	if recovered := invokeImmediate(17); recovered != 17 {
		panic("bad immediate panic outcome")
	}
	if recovered := invoke("panic-value"); recovered != "panic-value" ||
		trace != [3]int{1, 2, 3} || traceIndex != len(trace) {
		panic("bad panic outcome")
	}
	reset()
	if recovered := invoke(nil); recovered == nil ||
		trace != [3]int{1, 2, 3} || traceIndex != len(trace) {
		panic("bad nil panic outcome")
	}
	println("stackless-coro-panic-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-panic")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building panic coroutine failed: %v\n%s", err, out)
	}
	if want := "coro: phase=lower lowered=7 skipped=0"; !strings.Contains(out, want) {
		t.Fatalf("output does not contain %q\n%s", want, out)
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("panic coroutine failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-panic-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.(leaf|middle|root|immediate)\.coro\.func[0-9]+$|main\.(invoke|invokeImmediate)\.func[0-9]+$`, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of panic resume functions failed: %v\n%s", err, data)
	}
	disassembly := string(data)
	for _, want := range []string{
		"runtime.coroPanic", "runtime.coroTerminalAction",
		"runtime.coroDeferRecover",
	} {
		if !strings.Contains(disassembly, want) {
			t.Errorf("panic resume does not contain %q\n%s", want, disassembly)
		}
	}
	for _, forbidden := range []string{
		"runtime.gopanic", "runtime.gorecover",
		"runtime.deferproc", "runtime.deferreturn",
	} {
		if strings.Contains(disassembly, forbidden) {
			t.Errorf("panic resume contains %q\n%s", forbidden, disassembly)
		}
	}
}

func TestImplicitPanicOutcomeLowering(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import "runtime"

var trace [3]int
var traceIndex int

//go:noinline
func record(value int) {
	trace[traceIndex] = value
	traceIndex++
}

//go:noinline
func helperNil() int {
	var pointer *int
	return *pointer
}

//go:noinline
func replaceWithDivide() {
	zero := 0
	traceIndex += 1 / zero
}

//go:noinline
func fault(kind int) int {
	defer record(1)
	runtime.Gosched()
	result := 0
	if kind == 0 {
		var pointer *int
		result = *pointer
	}
	if kind == 1 {
		zero := kind - kind
		result = 1 / zero
	}
	if kind == 2 {
		values := []int{1}
		result = values[kind]
	}
	if kind == 3 {
		result = helperNil()
	}
	if kind == 4 {
		defer replaceWithDivide()
		var pointer *int
		result = *pointer
	}
	return result
}

//go:noinline
func middle(kind int) int {
	defer record(2)
	return fault(kind)
}

//go:noinline
func root(kind int) int {
	defer record(3)
	return middle(kind)
}

//go:noinline
func invoke(kind int) (recovered any) {
	defer func() {
		recovered = recover()
	}()
	_ = root(kind)
	return "missing panic"
}

//go:noinline
func check(kind int) {
	trace = [3]int{}
	traceIndex = 0
	if recovered := invoke(kind); recovered == nil ||
		trace != [3]int{1, 2, 3} || traceIndex != len(trace) {
		panic("bad implicit panic outcome")
	}
}

func main() {
	check(0)
	check(1)
	check(2)
	check(3)
	check(4)
	println("stackless-coro-implicit-panic-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-implicit-panic")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building implicit panic coroutine failed: %v\n%s", err, out)
	}
	for _, name := range []string{"fault", "middle", "root", "invoke", "check"} {
		want := "coro: func=main." + name + " "
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("implicit panic coroutine failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-implicit-panic-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.(fault|middle|root|invoke)\.coro\.func[0-9]+$`, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of implicit panic resume functions failed: %v\n%s",
			err, data)
	}
	if want := "runtime.coroTerminalAction"; !strings.Contains(string(data), want) {
		t.Errorf("implicit panic resume does not contain %q\n%s", want, data)
	}
}

func TestImplicitPanicInSpawnedCoroutine(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import "runtime"

var sink int

//go:noinline
func child() {
	defer println("stackless-coro-spawn-cleanup")
	runtime.Gosched()
	var pointer *int
	sink = *pointer
}

//go:noinline
func root() {
	go child()
	for {
		runtime.Gosched()
	}
}

func main() {
	root()
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-implicit-spawn-panic")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building spawned implicit panic failed: %v\n%s", err, out)
	}
	for _, name := range []string{"child", "root"} {
		want := "coro: func=main." + name + " "
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}

	// The child competes with many toolchain subprocesses during all.bash.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd = testenv.CommandContext(t, ctx, exe)
	data, err = cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("spawned implicit panic timed out: %v\n%s", ctx.Err(), data)
	}
	if err == nil {
		t.Fatalf("spawned implicit panic unexpectedly succeeded\n%s", data)
	}
	output := string(data)
	for _, want := range []string{
		"stackless-coro-spawn-cleanup",
		"panic: runtime error: invalid memory address",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output does not contain %q\n%s", want, output)
		}
	}
}

func TestRecoverAndReplacementPanicLowering(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import (
	"os"
	"runtime"
)

var trace [7]int
var traceIndex int
var namedValue any
var namedArgValue any
var namedArgTag int
var namedDynamicValue [3]any
var namedOriginal any
var namedMethodValue any
var namedMethodTag int

//go:noinline
func record(value int) {
	trace[traceIndex] = value
	traceIndex++
}

//go:noinline
func replacement() (original, replacement any) {
	defer func() {
		replacement = recover()
		record(1)
	}()
	defer func() {
		original = recover()
		record(2)
		panic("replacement")
	}()
	runtime.Gosched()
	panic("original")
}

//go:noinline
func normalCleanup() (recovered any) {
	defer func() {
		recovered = recover()
		record(3)
	}()
	defer func() {
		record(4)
		panic("cleanup")
	}()
	runtime.Gosched()
	return
}

//go:noinline
func dynamicRecover() (recovered [3]any) {
	for i := 0; i < len(recovered); i++ {
		index := i
		defer func() {
			recovered[index] = recover()
			record(10 + index)
		}()
		runtime.Gosched()
	}
	panic("dynamic")
}

//go:noinline
func dynamicReplacement() (replacement any) {
	defer func() {
		replacement = recover()
	}()
	for i := 0; i < 3; i++ {
		value := i
		defer func() {
			if value == 12 {
				panic("dynamic replacement")
			}
		}()
		value += 10
		runtime.Gosched()
	}
	panic("dynamic original")
}

//go:noinline
func noPanic() (recovered any) {
	defer func() {
		recovered = recover()
	}()
	runtime.Gosched()
	return
}

//go:noinline
func nilPanic() (recovered any) {
	defer func() {
		recovered = recover()
	}()
	runtime.Gosched()
	panic(nil)
}

//go:noinline
func namedRecover() {
	namedValue = recover()
}

//go:noinline
func namedFallback() {
	defer namedRecover()
	runtime.Gosched()
	panic("named")
}

//go:noinline
func namedRecoverArg(value *any, tag int) {
	*value = recover()
	namedArgTag = tag
}

//go:noinline
func namedArg() {
	defer namedRecoverArg(&namedArgValue, 23)
	runtime.Gosched()
	panic("named-arg")
}

//go:noinline
func namedRecoverIndex(index int) {
	namedDynamicValue[index] = recover()
}

//go:noinline
func namedDynamicRecover() {
	for i := 0; i < len(namedDynamicValue); i++ {
		defer namedRecoverIndex(i)
		runtime.Gosched()
	}
	panic("named-dynamic")
}

//go:noinline
func namedRecoverAndReplace() {
	namedOriginal = recover()
	panic("named-replacement")
}

//go:noinline
func namedReplacement() (replacement any) {
	defer func() {
		replacement = recover()
	}()
	defer namedRecoverAndReplace()
	runtime.Gosched()
	panic("named-original")
}

type namedReceiver struct{}

//go:noinline
func (*namedReceiver) recover(value *any, tag int) {
	*value = recover()
	namedMethodTag = tag
}

//go:noinline
func namedMethod() {
	var receiver namedReceiver
	defer receiver.recover(&namedMethodValue, 29)
	runtime.Gosched()
	panic("named-method")
}

func main() {
	original, replacement := replacement()
	cleanup := normalCleanup()
	dynamic := dynamicRecover()
	dynamicReplacementValue := dynamicReplacement()
	none := noPanic()
	nilValue := nilPanic()
	namedFallback()
	namedArg()
	namedDynamicRecover()
	namedReplacementValue := namedReplacement()
	namedMethod()
	expectNil := os.Getenv("EXPECT_PANICNIL") == "1"
	if original != "original" ||
		replacement != "replacement" ||
		cleanup != "cleanup" ||
		dynamic[2] != "dynamic" ||
		dynamic[1] != nil || dynamic[0] != nil ||
		dynamicReplacementValue != "dynamic replacement" ||
		none != nil || (nilValue == nil) != expectNil ||
		namedValue != "named" ||
		namedArgValue != "named-arg" || namedArgTag != 23 ||
		namedDynamicValue != [3]any{nil, nil, "named-dynamic"} ||
		namedOriginal != "named-original" ||
		namedReplacementValue != "named-replacement" ||
		namedMethodValue != "named-method" || namedMethodTag != 29 ||
		traceIndex != len(trace) ||
		trace != [7]int{2, 1, 4, 3, 12, 11, 10} {
		panic("bad recover or replacement panic result")
	}
	println("stackless-coro-recover-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-recover")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building recover coroutine failed: %v\n%s", err, out)
	}
	if want := "coro: phase=lower lowered=12 skipped=4"; !strings.Contains(out, want) {
		t.Fatalf("output does not contain %q\n%s", want, out)
	}
	for _, name := range []string{
		"replacement", "normalCleanup", "dynamicRecover", "dynamicReplacement",
		"noPanic", "nilPanic",
		"namedFallback", "namedArg", "namedDynamicRecover",
		"namedReplacement", "namedMethod",
	} {
		if diagnostic := "skip main." + name + ":"; strings.Contains(out, diagnostic) {
			t.Errorf("output contains unexpected %q\n%s", diagnostic, out)
		}
	}
	if want := "func=namedRecoverAndReplace.coro,"; !strings.Contains(out, want) {
		t.Errorf("output does not contain nested terminal factory %q\n%s",
			want, out)
	}

	cmd = testenv.Command(t, exe)
	cmd.Env = append(cmd.Environ(), "GODEBUG=panicnil=0")
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("recover coroutine failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-recover-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, exe)
	cmd.Env = append(cmd.Environ(),
		"GODEBUG=panicnil=1",
		"EXPECT_PANICNIL=1",
	)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("legacy panicnil recover coroutine failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-recover-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("legacy panicnil output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.(replacement|normalCleanup|noPanic|nilPanic)(\.coro)?\.func[0-9]+$`,
		exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of recover functions failed: %v\n%s", err, data)
	}
	disassembly := string(data)
	for _, want := range []string{
		"runtime.coroDeferPanic", "runtime.coroDeferRecover",
	} {
		if !strings.Contains(disassembly, want) {
			t.Errorf("recover functions do not contain %q\n%s", want, disassembly)
		}
	}
	for _, forbidden := range []string{
		"runtime.gopanic", "runtime.gorecover",
		"runtime.deferproc", "runtime.deferreturn",
	} {
		if strings.Contains(disassembly, forbidden) {
			t.Errorf("recover functions contain %q\n%s", forbidden, disassembly)
		}
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.named.*`,
		exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of named recover functions failed: %v\n%s", err, data)
	}
	disassembly = string(data)
	for _, want := range []string{
		"runtime.coroDeferCall", "runtime.gorecover",
	} {
		if !strings.Contains(disassembly, want) {
			t.Errorf("named recover functions do not contain %q\n%s",
				want, disassembly)
		}
	}
}

func TestSpawnPanicFallback(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import "runtime"

//go:noinline
func panics() {
	runtime.Gosched()
	panic("spawn panic")
}

//go:noinline
func spawn() {
	runtime.Gosched()
	go panics()
}

//go:noinline
func recoverer() (recovered any) {
	defer func() {
		recovered = recover()
	}()
	runtime.Gosched()
	panic("recovered")
}

func main() {
	if recovered := recoverer(); recovered != "recovered" {
		panic("bad recover fallback")
	}
	println("stackless-coro-terminal-fallback-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-terminal-fallback")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building terminal fallback program failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"coro: phase=lower lowered=3 skipped=1",
		"main.spawn: spawn target may panic",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("terminal fallback program failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-terminal-fallback-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}
}

func TestGoexitOutcomeLowering(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import "runtime"

var trace string
var overlayRecovered any
var detachedExited bool
var detachedParentObserved bool
var detachedReturned bool
var immediateReturned bool
var immediateRecovered any

//go:noinline
func leaf() {
	defer func() {
		trace += "leaf;"
	}()
	runtime.Gosched()
	runtime.Goexit()
	panic("leaf returned after Goexit")
}

//go:noinline
func middle() {
	defer func() {
		trace += "middle;"
	}()
	leaf()
	panic("middle returned after Goexit")
}

//go:noinline
func structured(done chan struct{}) {
	defer func() {
		trace += "root;"
		close(done)
	}()
	middle()
	panic("root returned after Goexit")
}

//go:noinline
func overlay(done chan struct{}) {
	defer func() {
		close(done)
	}()
	defer func() {
		overlayRecovered = recover()
		trace += "recover;"
	}()
	defer func() {
		panic("panic during Goexit")
	}()
	runtime.Gosched()
	runtime.Goexit()
	panic("overlay returned after Goexit")
}

//go:noinline
func detached() {
	defer func() {
		detachedExited = true
	}()
	runtime.Gosched()
	runtime.Goexit()
	detachedReturned = true
}

//go:noinline
func spawnRoot(done chan struct{}) {
	defer func() {
		close(done)
	}()
	go detached()
	runtime.Gosched()
	runtime.Gosched()
	detachedParentObserved = detachedExited
}

//go:noinline
func immediate(done chan struct{}) {
	defer func() {
		immediateRecovered = recover()
		close(done)
	}()
	runtime.Goexit()
	immediateReturned = true
}

func main() {
	structuredDone := make(chan struct{})
	go structured(structuredDone)
	<-structuredDone
	if trace != "leaf;middle;root;" {
		panic("bad structured Goexit cleanup")
	}

	overlayDone := make(chan struct{})
	go overlay(overlayDone)
	<-overlayDone
	if overlayRecovered != "panic during Goexit" ||
		trace != "leaf;middle;root;recover;" {
		panic("bad panic and recover during Goexit")
	}

	spawnDone := make(chan struct{})
	go spawnRoot(spawnDone)
	<-spawnDone
	if !detachedExited || !detachedParentObserved || detachedReturned {
		panic("detached Goexit terminated its parent")
	}

	immediateDone := make(chan struct{})
	go immediate(immediateDone)
	<-immediateDone
	if immediateReturned || immediateRecovered != nil {
		panic("bad immediate Goexit")
	}
	println("stackless-coro-goexit-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-goexit")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building Goexit coroutine failed: %v\n%s", err, out)
	}
	if want := "coro: phase=lower lowered=7"; !strings.Contains(out, want) {
		t.Fatalf("output does not contain %q\n%s", want, out)
	}
	for _, name := range []string{
		"main.leaf", "main.middle", "main.structured",
		"main.overlay", "main.detached", "main.spawnRoot", "main.immediate",
	} {
		if strings.Contains(out, "coro: skip "+name+":") {
			t.Errorf("Goexit function %s was not lowered\n%s", name, out)
		}
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Goexit coroutine failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-goexit-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.(leaf|middle|structured|overlay|detached|spawnRoot|immediate)\.coro\.func[0-9]+$`,
		exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of Goexit resume functions failed: %v\n%s", err, data)
	}
	disassembly := string(data)
	for _, want := range []string{
		"runtime.coroGoexit", "runtime.coroTerminalAction",
	} {
		if !strings.Contains(disassembly, want) {
			t.Errorf("Goexit resume functions do not contain %q\n%s",
				want, disassembly)
		}
	}
	if strings.Contains(disassembly, "runtime.Goexit") {
		t.Errorf("Goexit resume functions retain runtime.Goexit\n%s",
			disassembly)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.overlay\.func[0-9]+$`, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of Goexit defer functions failed: %v\n%s", err, data)
	}
	disassembly = string(data)
	for _, want := range []string{
		"runtime.coroDeferPanic", "runtime.coroDeferRecover",
	} {
		if !strings.Contains(disassembly, want) {
			t.Errorf("Goexit defer functions do not contain %q\n%s",
				want, disassembly)
		}
	}
}

func TestDeferGoexitLowering(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import "runtime"

var trace string
var returned [10]bool
var literalRecover any
var namedRecover any
var argumentValue int
var methodValue int
var recoveredParent any
var recoveredAfterExit any
var recoveredOverlay any
var recoveredFault any
var faultValue int
var normalValue int

//go:noinline
func literal() {
	defer func() {
		literalRecover = recover()
		trace += "literal-outer;"
	}()
	defer func() {
		defer func() {
			trace += "literal-inner;"
		}()
		runtime.Goexit()
		trace += "literal-returned;"
	}()
	runtime.Gosched()
	panic("literal panic")
}

//go:noinline
func namedExit() {
	defer func() {
		namedRecover = recover()
		trace += "named-target;"
	}()
	runtime.Goexit()
	trace += "named-returned;"
}

//go:noinline
func namedParent() {
	defer func() {
		trace += "named-outer;"
	}()
	defer namedExit()
	runtime.Gosched()
}

//go:noinline
func exitArgument(value *int, tag int) int {
	defer func() {
		*value += tag
		trace += "argument-target;"
	}()
	runtime.Goexit()
	return 99
}

//go:noinline
func argumentParent() {
	defer func() {
		trace += "argument-outer;"
	}()
	defer exitArgument(&argumentValue, 7)
	runtime.Gosched()
}

type receiver struct {
	value *int
}

//go:noinline
func (r *receiver) exit(tag int) {
	defer func() {
		*r.value += tag
		trace += "method-target;"
	}()
	runtime.Goexit()
}

//go:noinline
func methodParent() {
	defer func() {
		trace += "method-outer;"
	}()
	r := receiver{value: &methodValue}
	defer r.exit(11)
	runtime.Gosched()
}

//go:noinline
func exitIndex(index int) {
	switch index {
	case 0:
		trace += "dynamic-0;"
	case 1:
		trace += "dynamic-1;"
	case 2:
		trace += "dynamic-2;"
	}
	runtime.Goexit()
}

//go:noinline
func dynamicOuter() {
	trace += "dynamic-outer;"
}

//go:noinline
func dynamicParent() {
	defer dynamicOuter()
	for i := 0; i < 3; i++ {
		defer exitIndex(i)
	}
	runtime.Gosched()
}

//go:noinline
func dynamicLiteralParent() {
	defer func() {
		trace += "dynamic-literal-outer;"
	}()
	for i := 0; i < 3; i++ {
		value := i
		defer func() {
			switch value {
			case 10:
				trace += "dynamic-literal-10;"
			case 11:
				trace += "dynamic-literal-11;"
			case 12:
				trace += "dynamic-literal-12;"
				runtime.Goexit()
			}
		}()
		value += 10
	}
	runtime.Gosched()
}

//go:noinline
func recoverThenExit() {
	recoveredParent = recover()
	runtime.Goexit()
}

//go:noinline
func recoverParent() {
	defer func() {
		recoveredAfterExit = recover()
		trace += "recover-outer;"
	}()
	defer recoverThenExit()
	runtime.Gosched()
	panic("parent panic")
}

//go:noinline
func panicDuringExit() {
	defer func() {
		panic("panic during exit")
	}()
	runtime.Goexit()
}

//go:noinline
func overlayParent() {
	defer func() {
		recoveredOverlay = recover()
		trace += "overlay-outer;"
	}()
	defer panicDuringExit()
	runtime.Gosched()
}

//go:noinline
func faultDuringExit() {
	defer func() {
		var pointer *int
		faultValue = *pointer
	}()
	runtime.Goexit()
}

//go:noinline
func faultParent() {
	defer func() {
		recoveredFault = recover()
		trace += "fault-outer;"
	}()
	defer faultDuringExit()
	runtime.Gosched()
}

//go:noinline
func maybeExit(exit bool, value *int) int {
	defer func() {
		trace += "normal-target;"
	}()
	if exit {
		runtime.Goexit()
	}
	*value = 42
	return 7
}

//go:noinline
func normalParent() {
	defer maybeExit(false, &normalValue)
	runtime.Gosched()
}

func run(index int, f func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
		returned[index] = true
	}()
	<-done
}

func main() {
	run(0, literal)
	run(1, namedParent)
	run(2, argumentParent)
	run(3, methodParent)
	run(4, dynamicParent)
	run(5, recoverParent)
	run(6, overlayParent)
	run(7, faultParent)
	run(8, normalParent)
	run(9, dynamicLiteralParent)

	const wantTrace = "literal-inner;literal-outer;" +
		"named-target;named-outer;" +
		"argument-target;argument-outer;" +
		"method-target;method-outer;" +
		"dynamic-2;dynamic-1;dynamic-0;dynamic-outer;" +
		"recover-outer;overlay-outer;fault-outer;normal-target;" +
		"dynamic-literal-12;dynamic-literal-11;dynamic-literal-10;" +
		"dynamic-literal-outer;"
	if trace != wantTrace ||
		returned != [10]bool{false, false, false, false, false, false, false, false, true, false} ||
		literalRecover != nil || namedRecover != nil ||
		argumentValue != 7 || methodValue != 11 ||
		recoveredParent != "parent panic" || recoveredAfterExit != nil ||
		recoveredOverlay != "panic during exit" || recoveredFault == nil ||
		faultValue != 0 || normalValue != 42 {
		panic("bad defer Goexit result")
	}
	println("stackless-coro-defer-goexit-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-defer-goexit")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building defer Goexit coroutine failed: %v\n%s", err, out)
	}
	for _, name := range []string{
		"literal", "namedExit", "namedParent", "exitArgument",
		"argumentParent", "(*receiver).exit", "methodParent", "exitIndex",
		"dynamicParent", "dynamicLiteralParent", "recoverThenExit", "recoverParent",
		"panicDuringExit", "overlayParent", "faultDuringExit",
		"faultParent", "maybeExit", "normalParent",
	} {
		if diagnostic := "coro: skip main." + name + ":"; strings.Contains(out,
			diagnostic) {
			t.Errorf("output contains unexpected %q\n%s", diagnostic, out)
		}
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("defer Goexit coroutine failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-defer-goexit-ok"; !strings.Contains(
		string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\..*(deferwrap|func)[0-9]+$`, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of defer Goexit wrappers failed: %v\n%s", err, data)
	}
	disassembly := string(data)
	for _, want := range []string{
		"runtime.coroDeferGoexit", "runtime.coroDeferRun",
	} {
		if !strings.Contains(disassembly, want) {
			t.Errorf("defer Goexit wrappers do not contain %q\n%s",
				want, disassembly)
		}
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.(namedExit|exitArgument|exitIndex|recoverThenExit|panicDuringExit|faultDuringExit|maybeExit)\.coro\.func[0-9]+$`,
		exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of named defer targets failed: %v\n%s", err, data)
	}
	disassembly = string(data)
	if !strings.Contains(disassembly, "runtime.coroGoexit") {
		t.Errorf("named defer targets do not contain coroGoexit\n%s",
			disassembly)
	}
	if strings.Contains(disassembly, "runtime.Goexit") {
		t.Errorf("named defer targets retain runtime.Goexit\n%s", disassembly)
	}
}

func TestDirectSystemABIStackAlignment(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	testenv.MustHaveCGO(t)
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("System V AMD64 stack alignment is only used on linux/amd64")
	}

	tmp := t.TempDir()
	writeFile := func(name, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(contents), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("go.mod", "module example.com/coroalignment\n\ngo 1.28\n")
	writeFile("main.go", `package main

import (
	"runtime"
	corort "runtime/coro"
)

//go:noinline
func probe0() uint64 {
	return corort.DirectAdd(0, 0)
}

//go:noinline
func probe8() uint64 {
	var scratch [8]byte
	scratch[0] = 1
	value := corort.DirectAdd(0, 0)
	runtime.KeepAlive(&scratch)
	return value
}

//go:noinline
func probe16() uint64 {
	var scratch [16]byte
	scratch[0] = 1
	value := corort.DirectAdd(0, 0)
	runtime.KeepAlive(&scratch)
	return value
}

//go:noinline
func probe24() uint64 {
	var scratch [24]byte
	scratch[0] = 1
	value := corort.DirectAdd(0, 0)
	runtime.KeepAlive(&scratch)
	return value
}

func main() {
	values := [...]uint64{
		corort.DirectAdd(0, 0),
		probe0(),
		probe8(),
		probe16(),
		probe24(),
	}
	for _, value := range values {
		if value != 8 {
			panic("misaligned System V AMD64 call")
		}
	}
	println("direct-system-abi-alignment-ok")
}
`)
	asm := filepath.Join(tmp, "fixture.s")
	writeFile("fixture.s", `.text
.globl coro_add_u64
.type coro_add_u64,@function
coro_add_u64:
	movq %rsp, %rax
	andl $15, %eax
	ret
.size coro_add_u64, .-coro_add_u64

.section .note.GNU-stack,"",@progbits
`)

	cmd := testenv.Command(t, testenv.GoToolPath(t), "env", "CC")
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go env CC failed: %v\n%s", err, data)
	}
	cc := strings.TrimSpace(string(data))
	obj := filepath.Join(tmp, "fixture.syso")
	cmd = testenv.Command(t, cc, "-c", asm, "-o", obj)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compiling alignment fixture failed: %v\n%s", err, data)
	}
	if err := os.Remove(asm); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "alignment")
	cmd = testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-ldflags=-linkmode=external",
		".")
	cmd.Dir = tmp
	cmd.Env = append(cmd.Environ(),
		"CGO_ENABLED=1",
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building alignment fixture failed: %v\n%s", err, data)
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("alignment fixture failed: %v\n%s", err, data)
	}
	if want := "direct-system-abi-alignment-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `runtime/coro\.DirectAdd`, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump failed: %v\n%s", err, data)
	}
	if disassembly := string(data); !strings.Contains(disassembly, "ANDQ $-0x10, SP") {
		t.Errorf("direct call does not align SP for the System V ABI\n%s", disassembly)
	}
}

func TestDirectSystemABI(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	testenv.MustHaveCGO(t)
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("direct System ABI fixture is only implemented on Darwin and Linux")
	}
	if runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64" {
		t.Skip("direct System ABI fixture is only implemented on arm64 and amd64")
	}

	tmp := t.TempDir()
	writeFile := func(name, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(contents), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("go.mod", "module example.com/corodirect\n\ngo 1.28\n")
	writeFile("main.go", `package main

import (
	"runtime"
	corort "runtime/coro"
	"sync/atomic"
	"syscall"
	"time"
)

var sum uint64
var gate uint32
var entered uint32
var asyncReadFD int
var asyncWriteFD int
var asyncResult uint64
var asyncErrno uintptr
var logicalProgress uint32
var watchdogUsed uint32
var directFaultRecovered bool

//go:noinline
func directFault() {
	_ = corort.DirectAdd(1, 2)
	var pointer *int
	sum += uint64(*pointer)
}

//go:noinline
func checkDirectFault() {
	defer func() {
		directFaultRecovered = recover() != nil
	}()
	directFault()
}

//go:noinline
func foreign() {
	sum = corort.DirectAdd(19, 23)
	runtime.Gosched()
	atomic.StoreUint32(&entered, 1)
	corort.DirectBlock(&gate)
	corort.AsyncDouble(asyncReadFD, asyncWriteFD, 21, &asyncResult, &asyncErrno)
}

//go:noinline
func replacement() {
	for atomic.LoadUint32(&entered) == 0 {
		runtime.Gosched()
	}
	atomic.StoreUint32(&logicalProgress, 1)
	atomic.CompareAndSwapUint32(&gate, 0, 1)
}

func main() {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	var pipe [2]int
	if err := syscall.Pipe(pipe[:]); err != nil {
		panic(err)
	}
	asyncReadFD, asyncWriteFD = pipe[0], pipe[1]
	defer syscall.Close(asyncReadFD)
	defer syscall.Close(asyncWriteFD)
	if err := syscall.SetNonblock(asyncReadFD, true); err != nil {
		panic(err)
	}
	checkDirectFault()
	go replacement()
	go func() {
		time.Sleep(2 * time.Second)
		if atomic.CompareAndSwapUint32(&gate, 0, 1) {
			atomic.StoreUint32(&watchdogUsed, 1)
		}
	}()
	foreign()
	if sum != 42 || atomic.LoadUint32(&gate) != 1 ||
		atomic.LoadUint32(&entered) != 1 ||
		atomic.LoadUint32(&logicalProgress) != 1 ||
		atomic.LoadUint32(&watchdogUsed) != 0 ||
		asyncResult != 42 || asyncErrno != 0 || !directFaultRecovered {
		panic("bad direct System ABI result")
	}
	println("direct-system-abi-ok")
}
`)
	csrc := filepath.Join(tmp, "fixture.c")
	writeFile("fixture.c", `#define _GNU_SOURCE
#include <errno.h>
#include <pthread.h>
#include <stdint.h>
#include <stdlib.h>
#include <unistd.h>

struct coro_request {
	uint64_t id;
	uint64_t value;
	int fd;
};

#if defined(__x86_64__)
#define CORO_SYSTEM_ABI_ALIGNED() \
	((((uintptr_t)__builtin_frame_address(0) + sizeof(void *)) & 15) == 8)
#else
#define CORO_SYSTEM_ABI_ALIGNED() 1
#endif

static int coro_on_native_thread_stack(void) {
	char local;
	uintptr_t sp = (uintptr_t)&local;
#if defined(__APPLE__)
	pthread_t self = pthread_self();
	uintptr_t hi = (uintptr_t)pthread_get_stackaddr_np(self);
	size_t size = pthread_get_stacksize_np(self);
	return sp >= hi - size && sp < hi;
#elif defined(__linux__)
	pthread_attr_t attr;
	void *base;
	size_t size;
	if (pthread_getattr_np(pthread_self(), &attr) != 0) {
		return 0;
	}
	if (pthread_attr_getstack(&attr, &base, &size) != 0) {
		pthread_attr_destroy(&attr);
		return 0;
	}
	pthread_attr_destroy(&attr);
	return sp >= (uintptr_t)base && sp < (uintptr_t)base + size;
#else
	return 0;
#endif
}

uint64_t coro_add_u64(uint64_t a, uint64_t b) {
	if (!CORO_SYSTEM_ABI_ALIGNED()) {
		return UINT64_MAX;
	}
	if (!coro_on_native_thread_stack()) {
		return UINT64_MAX;
	}
	return a + b;
}

void coro_block_until(uint32_t *gate) {
	if (!CORO_SYSTEM_ABI_ALIGNED()) {
		__atomic_store_n(gate, UINT32_MAX, __ATOMIC_RELEASE);
		return;
	}
	while (__atomic_load_n(gate, __ATOMIC_ACQUIRE) == 0) {
	}
}

static void *coro_double_worker(void *arg) {
	struct coro_request *request = arg;
	uint64_t packet[2] = {request->id, request->value * 2};
	int fd = request->fd;
	free(request);
	while (write(fd, packet, sizeof(packet)) < 0 && errno == EINTR) {
	}
	return NULL;
}

int32_t coro_submit_u64(uint64_t id, uint64_t value, int fd) {
	if (!CORO_SYSTEM_ABI_ALIGNED()) {
		return EINVAL;
	}
	struct coro_request *request = malloc(sizeof(*request));
	if (request == NULL) {
		return ENOMEM;
	}
	request->id = id;
	request->value = value;
	request->fd = fd;

	pthread_t thread;
	int error = pthread_create(&thread, NULL, coro_double_worker, request);
	if (error != 0) {
		free(request);
		return error;
	}
	error = pthread_detach(thread);
	if (error != 0) {
		return error;
	}
	return 0;
}
`)

	cmd := testenv.Command(t, testenv.GoToolPath(t), "env", "CC")
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go env CC failed: %v\n%s", err, data)
	}
	cc := strings.TrimSpace(string(data))
	obj := filepath.Join(tmp, "fixture.syso")
	cmd = testenv.Command(t, cc, "-pthread", "-c", csrc, "-o", obj)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compiling C fixture failed: %v\n%s", err, data)
	}
	if err := os.Remove(csrc); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "direct")
	cmd = testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=example.com/corodirect=-l -d=coro=4",
		"-ldflags=-linkmode=external -extldflags=-pthread",
		".")
	cmd.Dir = tmp
	cmd.Env = append(cmd.Environ(),
		"CGO_ENABLED=1",
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err = cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building direct fixture failed: %v\n%s", err, out)
	}
	if want := "coro: phase=lower lowered=4"; !strings.Contains(out, want) {
		t.Fatalf("output does not contain %q\n%s", want, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd = testenv.CommandContext(t, ctx, exe)
	data, err = cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("direct fixture timed out: %v\n%s", ctx.Err(), data)
	}
	if err != nil {
		t.Fatalf("direct fixture failed: %v\n%s", err, data)
	}
	if want := "direct-system-abi-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `runtime/coro\.Direct(Add|Block)`, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump failed: %v\n%s", err, data)
	}
	disassembly := string(data)
	for _, forbidden := range []string{
		"runtime.cgocall",
		"runtime.asmcgocall",
		"runtime.morestack",
		"_cgo_Cfunc_",
	} {
		if strings.Contains(disassembly, forbidden) {
			t.Errorf("direct call disassembly contains %q\n%s", forbidden, disassembly)
		}
	}
	for _, want := range []string{
		"DirectAdd",
		"DirectBlock",
		"coro_add_u64",
		"coro_block_until",
	} {
		if !strings.Contains(disassembly, want) {
			t.Errorf("direct call disassembly does not contain %q\n%s", want, disassembly)
		}
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `runtime\.coroSubmit`, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of async submit failed: %v\n%s", err, data)
	}
	disassembly = string(data)
	if !strings.Contains(disassembly, "coro_submit_u64") {
		t.Errorf("async submit disassembly does not call coro_submit_u64\n%s", disassembly)
	}
	for _, forbidden := range []string{"runtime.cgocall", "runtime.asmcgocall", "_cgo_Cfunc_"} {
		if strings.Contains(disassembly, forbidden) {
			t.Errorf("async submit disassembly contains %q\n%s", forbidden, disassembly)
		}
	}
}

func TestChannelOperationLowering(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import (
	"runtime"
	"sync/atomic"
	"time"
)

type namedBool bool

var evaluated int
var sendChannel chan int
var manyChannel chan int
var pointerChannel chan *int
var gcDone atomic.Uint32

//go:noinline
func nextValue() int {
	evaluated++
	return 7
}

//go:noinline
func send() {
	sendChannel <- nextValue()
}

//go:noinline
func sendOne() {
	manyChannel <- 1
}

//go:noinline
func newPointer() *int {
	value := new(int)
	*value = 29
	return value
}

//go:noinline
func sendPointer() {
	pointerChannel <- newPointer()
}

//go:noinline
func directionalSend(ch chan<- int) {
	ch <- 23
}

//go:noinline
func directionalRecv(ch <-chan int) int {
	value := <-ch
	return value
}

//go:noinline
func collect() {
	runtime.GC()
	gcDone.Store(1)
}

//go:noinline
func launch(f func()) {
	go f()
}

//go:noinline
func receive(ch chan int) (int, bool) {
	value, ok := <-ch
	return value, ok
}

//go:noinline
func discard(ch chan int) {
	<-ch
}

//go:noinline
func timerReceive(ch <-chan time.Time) time.Time {
	value := <-ch
	return value
}

//go:noinline
func closedSend(ch chan int) (recovered any) {
	defer func() {
		recovered = recover()
	}()
	ch <- 1
	return
}

//go:noinline
func many() int {
	ch := make(chan int)
	manyChannel = ch
	go sendOne()
	go sendOne()
	go sendOne()
	go sendOne()
	go sendOne()
	go sendOne()
	go sendOne()
	go sendOne()
	sum := 0
	for i := 0; i < 8; i++ {
		value, ok := <-ch
		if !ok {
			panic("unexpected closed channel")
		}
		sum += value
	}
	return sum
}

func main() {
	ch := make(chan int)
	sendChannel = ch
	go send()
	runtime.Gosched()
	if evaluated != 1 {
		panic("send value was not evaluated before blocking")
	}
	value, ok := receive(ch)
	if value != 7 || !ok || evaluated != 1 {
		panic("bad unbuffered channel result")
	}

	buffered := make(chan int, 1)
	buffered <- 11
	if got := <-buffered; got != 11 {
		panic("bad buffered channel result")
	}
	close(buffered)
	zero, open := <-buffered
	if zero != 0 || open {
		panic("bad closed channel receive")
	}

	firstBlank := make(chan int, 1)
	firstBlank <- 13
	_, onlyOK := <-firstBlank
	if !onlyOK {
		panic("bad blank channel value")
	}

	secondBlank := make(chan int, 1)
	secondBlank <- 17
	onlyValue, _ := <-secondBlank
	if onlyValue != 17 {
		panic("bad blank channel status")
	}

	namedChannel := make(chan int, 1)
	namedChannel <- 19
	var namedValue int
	var namedOK namedBool
	namedValue, namedOK = <-namedChannel
	if namedValue != 19 || !namedOK {
		panic("bad named channel status")
	}

	directional := make(chan int, 1)
	directionalSend(directional)
	if directionalRecv(directional) != 23 {
		panic("bad directional channel result")
	}

	pointers := make(chan *int)
	pointerChannel = pointers
	go sendPointer()
	runtime.Gosched()
	launch(collect)
	for gcDone.Load() == 0 {
		runtime.Gosched()
	}
	pointer := <-pointers
	if pointer == nil || *pointer != 29 {
		panic("channel send value was not kept alive")
	}

	discarded := make(chan int, 1)
	discarded <- 1
	discard(discarded)

	timer := time.NewTimer(10 * time.Millisecond)
	if value := timerReceive(timer.C); value.IsZero() {
		panic("bad timer channel result")
	}

	closed := make(chan int)
	close(closed)
	if closedSend(closed) == nil {
		panic("send on closed channel did not panic")
	}
	if sum := many(); sum != 8 {
		panic("blocked channel operations stalled the scheduler")
	}
	println("stackless-coro-channel-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-channel")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building channel coroutine failed: %v\n%s", err, out)
	}
	for _, name := range []string{
		"send", "sendOne", "sendPointer", "directionalSend",
		"directionalRecv", "receive", "discard", "timerReceive",
		"closedSend", "many", "main",
	} {
		if diagnostic := "skip main." + name + ":"; strings.Contains(
			out, diagnostic) {
			t.Errorf("output contains unexpected %q\n%s", diagnostic, out)
		}
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("channel coroutine failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-channel-ok"; !strings.Contains(
		string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.(send|receive)\.coro\.func[0-9]+$`, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of channel resume functions failed: %v\n%s",
			err, data)
	}
	disassembly := string(data)
	for _, want := range []string{
		"runtime.coroChanSend", "runtime.coroChanRecv",
	} {
		if !strings.Contains(disassembly, want) {
			t.Errorf("channel resume functions do not contain %q\n%s",
				want, disassembly)
		}
	}
	for _, forbidden := range []string{
		"runtime.chansend1", "runtime.chanrecv1", "runtime.chanrecv2",
	} {
		if strings.Contains(disassembly, forbidden) {
			t.Errorf("channel resume functions contain %q\n%s",
				forbidden, disassembly)
		}
	}
}

func TestChannelRangeLowering(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import "runtime"

var evaluated int
var produced chan int

//go:noinline
func rangeSource(ch <-chan int) <-chan int {
	evaluated++
	return ch
}

//go:noinline
func values3(a, b, c int) <-chan int {
	ch := make(chan int, 3)
	ch <- a
	ch <- b
	ch <- c
	close(ch)
	return ch
}

//go:noinline
func values2(a, b int) <-chan int {
	ch := make(chan int, 2)
	ch <- a
	ch <- b
	close(ch)
	return ch
}

//go:noinline
func channels2(a, b <-chan int) <-chan (<-chan int) {
	ch := make(chan (<-chan int), 2)
	ch <- a
	ch <- b
	close(ch)
	return ch
}

//go:noinline
func empty() <-chan int {
	ch := make(chan int)
	close(ch)
	return ch
}

//go:noinline
func rangeSum(ch <-chan int) (int, int) {
	sum := 0
	last := 37
	for last = range rangeSource(ch) {
		runtime.Gosched()
		sum += last
	}
	return sum, last
}

//go:noinline
func rangeBlank(ch <-chan int) int {
	count := 0
	for range ch {
		count++
	}
	return count
}

//go:noinline
func rangeInterface(ch <-chan int) int {
	var value any
	sum := 0
	for value = range ch {
		sum += value.(int)
	}
	return sum
}

//go:noinline
func rangeDistinct(ch <-chan int) bool {
	var values []*int
	for value := range ch {
		values = append(values, &value)
		runtime.Gosched()
	}
	return len(values) == 2 && values[0] != values[1] &&
		*values[0] == 41 && *values[1] == 43
}

//go:noinline
func rangeNested(ch <-chan (<-chan int)) int {
	sum := 0
	for inner := range ch {
		for value := range inner {
			sum += value
		}
	}
	return sum
}

//go:noinline
func produce() {
	produced <- 19
	produced <- 23
	close(produced)
}

//go:noinline
func rangeProduced() int {
	ch := make(chan int)
	produced = ch
	go produce()
	return rangeBlank(ch)
}

func main() {
	sumValues := values3(2, 3, 5)
	total, last := rangeSum(sumValues)
	if total != 10 || last != 5 || evaluated != 1 {
		panic("bad channel range result")
	}

	emptyValues := empty()
	emptyTotal, emptyLast := rangeSum(emptyValues)
	if emptyTotal != 0 || emptyLast != 37 || evaluated != 2 {
		panic("closed channel range changed the iteration variable")
	}
	blankValues := values2(7, 11)
	blankCount := rangeBlank(blankValues)
	if blankCount != 2 {
		panic("bad blank channel range")
	}
	interfaceValues := values2(13, 17)
	interfaceSum := rangeInterface(interfaceValues)
	if interfaceSum != 30 {
		panic("bad converted channel range")
	}
	distinctValues := values2(41, 43)
	distinct := rangeDistinct(distinctValues)
	if !distinct {
		panic("bad channel range iteration variables")
	}
	firstNested := values2(59, 61)
	secondNested := values2(67, 71)
	nestedValues := channels2(firstNested, secondNested)
	nestedSum := rangeNested(nestedValues)
	if nestedSum != 258 {
		panic("bad nested channel range")
	}
	producedCount := rangeProduced()
	if producedCount != 2 {
		panic("blocked channel range stalled the scheduler")
	}
	println("stackless-coro-channel-range-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-channel-range")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building channel range coroutine failed: %v\n%s", err, out)
	}
	for _, name := range []string{
		"values3", "values2", "channels2", "rangeSum", "rangeBlank",
		"rangeInterface", "rangeDistinct", "rangeNested", "produce",
		"rangeProduced", "main",
	} {
		if diagnostic := "skip main." + name + ":"; strings.Contains(
			out, diagnostic) {
			t.Errorf("output contains unexpected %q\n%s", diagnostic, out)
		}
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("channel range coroutine failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-channel-range-ok"; !strings.Contains(
		string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.range(Sum|Blank|Interface|Distinct|Nested)\.coro\.func[0-9]+$`,
		exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of channel range resume functions failed: %v\n%s",
			err, data)
	}
	disassembly := string(data)
	if !strings.Contains(disassembly, "runtime.coroChanRecv") {
		t.Errorf("channel range resume functions do not contain coroChanRecv\n%s",
			disassembly)
	}
	for _, forbidden := range []string{
		"runtime.chanrecv1", "runtime.chanrecv2",
	} {
		if strings.Contains(disassembly, forbidden) {
			t.Errorf("channel range resume functions contain %q\n%s",
				forbidden, disassembly)
		}
	}
}

func TestChannelRangeClosureFallback(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import "runtime"

//go:noinline
func values() <-chan int {
	ch := make(chan int, 2)
	ch <- 47
	ch <- 53
	close(ch)
	return ch
}

//go:noinline
func collect(ch <-chan int) []func() int {
	var result []func() int
	for value := range ch {
		result = append(result, func() int {
			return value
		})
		runtime.Gosched()
	}
	return result
}

func main() {
	ch := values()
	result := collect(ch)
	first := result[0]()
	second := result[1]()
	if len(result) != 2 || first != 47 || second != 53 {
		println("channel range closure result", len(result), first, second)
		panic("bad channel range closure variables")
	}
	println("stackless-coro-channel-range-closure-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-channel-range-closure")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building channel range closure failed: %v\n%s", err, out)
	}
	if diagnostic := "skip main.values:"; strings.Contains(out, diagnostic) {
		t.Errorf("output contains unexpected %q\n%s", diagnostic, out)
	}
	if diagnostic := "coro: skip main.collect: range variable captured by closure"; !strings.Contains(
		out, diagnostic) {
		t.Errorf("output does not contain %q\n%s", diagnostic, out)
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("channel range closure failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-channel-range-closure-ok"; !strings.Contains(
		string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}
}

func TestChannelSelectLowering(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	program := `package main

import (
	"runtime"
	"time"
)

var evaluation int
var produced chan int
var consumed chan int
var duplicateChannel chan int
var emptyStarted chan struct{}

//go:noinline
func channel(id int, ch chan int) chan int {
	evaluation = evaluation*10 + id
	return ch
}

//go:noinline
func value(id, result int) int {
	evaluation = evaluation*10 + id
	return result
}

//go:noinline
func buffered(ch chan int, disabled chan int) (int, bool) {
	select {
	case result, ok := <-channel(1, ch):
		runtime.Gosched()
		return result, ok
	case channel(2, disabled) <- value(3, 17):
		return -1, false
	}
}

//go:noinline
func withDefault(recv, send chan int) (int, bool) {
	result := 29
	ok := true
	select {
	case result, ok = <-channel(4, recv):
	case channel(5, send) <- value(6, 31):
	default:
		runtime.Gosched()
	}
	return result, ok
}

//go:noinline
func produce() {
	produced <- 37
}

//go:noinline
func blocked(first, second chan int) int {
	produced = second
	go produce()
	select {
	case result := <-first:
		return result + 1000
	case result := <-second:
		runtime.Gosched()
		return result
	}
}

//go:noinline
func consume() {
	result := <-produced
	consumed <- result
}

//go:noinline
func selectedSend(ch chan int) int {
	produced = ch
	consumed = make(chan int, 1)
	go consume()
	select {
	case ch <- 41:
		result := <-consumed
		return result
	}
}

//go:noinline
func bufferedSend(ch chan int) int {
	select {
	case ch <- 42:
	}
	result := <-ch
	return result
}

//go:noinline
func closedReceive(ch chan int) (int, bool) {
	result := 43
	ok := true
	select {
	case result, ok = <-ch:
	}
	return result, ok
}

//go:noinline
func closedSend(ch chan int) (recovered any) {
	defer func() {
		recovered = recover()
	}()
	select {
	case ch <- 47:
	}
	return nil
}

//go:noinline
func heterogeneous(ints chan int, strings chan string) string {
	select {
	case intResult := <-ints:
		if intResult == 0 {
			return "zero"
		}
		return "int"
	case stringResult := <-strings:
		runtime.Gosched()
		return stringResult
	}
}

//go:noinline
func onlyDefault() int {
	select {
	default:
		runtime.Gosched()
		return 71
	}
}

//go:noinline
func nested(outer, inner chan int) int {
	select {
	case first := <-outer:
		select {
		case second := <-inner:
			runtime.Gosched()
			return first + second
		}
	}
}

//go:noinline
func produceDuplicate() {
	duplicateChannel <- 79
}

//go:noinline
func duplicate(ch chan int) int {
	duplicateChannel = ch
	go produceDuplicate()
	select {
	case result := <-ch:
		return result
	case result := <-ch:
		return result + 100
	}
}

//go:noinline
func selectedTimer(ch <-chan time.Time) time.Time {
	select {
	case result := <-ch:
		return result
	}
}

//go:noinline
func emptySelect() {
	emptyStarted <- struct{}{}
	select {}
}

func main() {
	ready := make(chan int, 1)
	ready <- 11
	result, ok := buffered(ready, nil)
	if result != 11 || !ok || evaluation != 123 {
		panic("bad buffered select")
	}

	result, ok = withDefault(nil, nil)
	if result != 29 || !ok || evaluation != 123456 {
		panic("bad default select")
	}

	if result := blocked(nil, make(chan int)); result != 37 {
		panic("blocked select stalled the scheduler")
	}
	if result := selectedSend(make(chan int)); result != 41 {
		panic("bad selected send")
	}
	if result := bufferedSend(make(chan int, 1)); result != 42 {
		panic("bad buffered send")
	}

	closed := make(chan int)
	close(closed)
	result, ok = closedReceive(closed)
	if result != 0 || ok {
		panic("bad closed receive")
	}
	if closedSend(closed) == nil {
		panic("send on closed channel did not panic")
	}

	strings := make(chan string, 1)
	strings <- "selected"
	if heterogeneous(nil, strings) != "selected" {
		panic("bad heterogeneous select")
	}
	if onlyDefault() != 71 {
		panic("bad default-only select")
	}

	outer := make(chan int, 1)
	inner := make(chan int, 1)
	outer <- 73
	inner <- 5
	if nested(outer, inner) != 78 {
		panic("bad nested select")
	}
	duplicateResult := duplicate(make(chan int))
	if duplicateResult != 79 && duplicateResult != 179 {
		panic("bad duplicate-channel select")
	}
	timer := time.NewTimer(5 * time.Millisecond)
	timerResult := selectedTimer(timer.C)
	if timerResult.IsZero() {
		panic("bad timer select")
	}
	started := make(chan struct{})
	emptyStarted = started
	go emptySelect()
	<-started
	println("stackless-coro-select-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "coro-select")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=command-line-arguments=-l -d=coro=4",
		src)
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("building channel select coroutine failed: %v\n%s", err, out)
	}
	for _, name := range []string{
		"buffered", "withDefault", "produce", "blocked", "consume",
		"selectedSend", "bufferedSend", "closedReceive", "closedSend",
		"heterogeneous",
		"onlyDefault", "nested", "produceDuplicate", "duplicate", "main",
		"selectedTimer", "emptySelect",
	} {
		if diagnostic := "skip main." + name + ":"; strings.Contains(
			out, diagnostic) {
			t.Errorf("output contains unexpected %q\n%s", diagnostic, out)
		}
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("channel select coroutine failed: %v\n%s", err, data)
	}
	if want := "stackless-coro-select-ok"; !strings.Contains(
		string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.(buffered|withDefault|blocked|selectedSend|bufferedSend|closedReceive|closedSend|heterogeneous|nested|duplicate|selectedTimer|emptySelect)\.coro\.func[0-9]+$`,
		exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of channel select resume functions failed: %v\n%s",
			err, data)
	}
	disassembly := string(data)
	if !strings.Contains(disassembly, "runtime.coroSelect") {
		t.Errorf("channel select resume functions do not contain coroSelect\n%s",
			disassembly)
	}
	for _, forbidden := range []string{
		"runtime.selectgo", "runtime.selectnbsend", "runtime.selectnbrecv",
	} {
		if strings.Contains(disassembly, forbidden) {
			t.Errorf("channel select resume functions contain %q\n%s",
				forbidden, disassembly)
		}
	}
}

func TestInliningCompatibility(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "p.go")
	program := `package p

func leaf(ch chan int) {
	ch <- 1
}

func caller(ch chan int) {
	leaf(ch)
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}

	cmd := testenv.Command(t, testenv.GoToolPath(t),
		"tool", "compile", "-m=2", "-d=coro=2",
		"-p=p", "-o", filepath.Join(tmp, "p.o"), src)
	cmd.Env = append(cmd.Environ(), "GOEXPERIMENT=coro")
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("compile failed: %v\n%s", err, out)
	}

	for _, want := range []string{
		"can inline leaf",
		"inlining call to leaf",
		"coro: phase=provisional",
		"coro: func=p.caller effect=may-suspend local=nosuspend",
		"coro: phase=final",
		"coro: func=p.caller effect=may-suspend local=may-suspend recursive=false seeds=channel-send",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}
}

func TestCrossPackageSummary(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	writeFile := func(name, contents string) {
		t.Helper()
		name = filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Dir(name), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(contents), 0o666); err != nil {
			t.Fatal(err)
		}
	}

	writeFile("go.mod", "module example.com/coropoc\n\ngo 1.28\n")
	writeFile("leaf/leaf.go", `package leaf

var Ch chan int

func Suspend() {
	Ch <- 1
}

func Plain() int {
	return 1
}
`)
	writeFile("mid/mid.go", `package mid

import "example.com/coropoc/leaf"

func Suspend() {
	leaf.Suspend()
}

func Plain() int {
	return leaf.Plain()
}
`)
	writeFile("root/main.go", `package main

import "example.com/coropoc/mid"

func rootSuspend() {
	mid.Suspend()
}

func main() {
	ch := make(chan int, 1)
	ch <- 42
	if <-ch != 42 {
		panic("bad channel result")
	}
	println("coro-poc-ok")
}
`)

	exe := filepath.Join(tmp, "coropoc")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=example.com/coropoc/...=-l -d=coro=2", "./root")
	cmd.Dir = tmp
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("cross-package build failed: %v\n%s", err, out)
	}

	for _, want := range []string{
		"coro: edge=direct caller=example.com/coropoc/mid.Suspend callee=example.com/coropoc/leaf.Suspend unknown=false effect=may-suspend",
		"coro: edge=direct caller=example.com/coropoc/mid.Plain callee=example.com/coropoc/leaf.Plain unknown=false effect=nosuspend",
		"coro: func=example.com/coropoc/mid.Suspend effect=may-suspend",
		"coro: edge=direct caller=main.rootSuspend callee=example.com/coropoc/mid.Suspend unknown=false effect=may-suspend",
		"coro: func=main.rootSuspend effect=may-suspend",
		"coro: func=main.main effect=may-suspend",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}

	cmd = testenv.Command(t, exe)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("experiment executable failed: %v\n%s", err, data)
	} else if want := "coro-poc-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("executable output does not contain %q\n%s", want, data)
	}
}

func TestCrossPackagePanicOutcome(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	writeFile := func(name, contents string) {
		t.Helper()
		name = filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Dir(name), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(contents), 0o666); err != nil {
			t.Fatal(err)
		}
	}

	writeFile("go.mod", "module example.com/coropanic\n\ngo 1.28\n")
	writeFile("leaf/leaf.go", `package leaf

import "runtime"

//go:noinline
func Panic(value any) {
	runtime.Gosched()
	panic(value)
}
`)
	writeFile("mid/mid.go", `package mid

import "example.com/coropanic/leaf"

//go:noinline
func Panic(value any) {
	leaf.Panic(value)
}
`)
	writeFile("root/main.go", `package main

import "example.com/coropanic/mid"

//go:noinline
func invoke(value any) (recovered any) {
	defer func() {
		recovered = recover()
	}()
	mid.Panic(value)
	return "unreachable"
}

func main() {
	if recovered := invoke("cross-package panic"); recovered != "cross-package panic" {
		panic("bad cross-package panic")
	}
	println("stackless-coro-cross-panic-ok")
}
`)

	exe := filepath.Join(tmp, "coro-cross-panic")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=example.com/coropanic/...=-l -d=coro=4",
		"./root")
	cmd.Dir = tmp
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("cross-package panic build failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"coro: func=example.com/coropanic/leaf.Panic effect=may-suspend local=may-suspend",
		"terminal=panic local-terminal=panic",
		"coro: func=example.com/coropanic/mid.Panic effect=may-suspend local=nosuspend",
		"terminal=panic local-terminal=-",
		"coro: site=1 func=example.com/coropanic/mid.Panic kind=await",
		"coro: phase=lower lowered=2 skipped=0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}

	cmd = testenv.Command(t, exe)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-package panic executable failed: %v\n%s", err, data)
	} else if want := "stackless-coro-cross-panic-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("executable output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nm of cross-package panic executable failed: %v\n%s",
			err, data)
	}
	symbols := string(data)
	for _, want := range []string{
		"example.com/coropanic/leaf.Panic.coro",
		"example.com/coropanic/mid.Panic.coro",
		"main.invoke.coro",
	} {
		if !strings.Contains(symbols, want) {
			t.Errorf("symbols do not contain %q", want)
		}
	}
}

func TestCrossPackageGoexitOutcome(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	writeFile := func(name, contents string) {
		t.Helper()
		name = filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Dir(name), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(contents), 0o666); err != nil {
			t.Fatal(err)
		}
	}

	writeFile("go.mod", "module example.com/corogoexit\n\ngo 1.28\n")
	writeFile("leaf/leaf.go", `package leaf

import "runtime"

//go:noinline
func Exit(trace *string) {
	defer func() {
		*trace += "leaf;"
	}()
	runtime.Gosched()
	runtime.Goexit()
	panic("leaf returned after Goexit")
}
`)
	writeFile("mid/mid.go", `package mid

import "example.com/corogoexit/leaf"

//go:noinline
func Exit(trace *string) {
	defer func() {
		*trace += "mid;"
	}()
	leaf.Exit(trace)
	panic("mid returned after Goexit")
}
`)
	writeFile("root/main.go", `package main

import "example.com/corogoexit/mid"

var trace string

//go:noinline
func run(done chan struct{}) {
	defer func() {
		trace += "root;"
		close(done)
	}()
	mid.Exit(&trace)
	panic("root returned after Goexit")
}

func main() {
	done := make(chan struct{})
	go run(done)
	<-done
	if trace != "leaf;mid;root;" {
		panic("bad cross-package Goexit cleanup")
	}
	println("stackless-coro-cross-goexit-ok")
}
`)

	exe := filepath.Join(tmp, "coro-cross-goexit")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=example.com/corogoexit/...=-l -d=coro=4",
		"./root")
	cmd.Dir = tmp
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("cross-package Goexit build failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"coro: func=example.com/corogoexit/leaf.Exit",
		"terminal=panic,goexit local-terminal=panic,goexit",
		"kind=goexit",
		"coro: func=example.com/corogoexit/mid.Exit",
		"terminal=panic,goexit local-terminal=panic",
		"coro: site=2 func=example.com/corogoexit/mid.Exit kind=await",
		"coro: func=main.run",
		"terminal=panic,goexit local-terminal=panic",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q\n%s", want, out)
		}
	}
	for _, name := range []string{
		"example.com/corogoexit/leaf.Exit",
		"example.com/corogoexit/mid.Exit",
		"main.run",
	} {
		if strings.Contains(out, "coro: skip "+name+":") {
			t.Errorf("cross-package Goexit function %s was not lowered\n%s",
				name, out)
		}
	}

	cmd = testenv.Command(t, exe)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-package Goexit executable failed: %v\n%s", err, data)
	} else if want := "stackless-coro-cross-goexit-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("executable output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nm of cross-package Goexit executable failed: %v\n%s",
			err, data)
	}
	symbols := string(data)
	for _, want := range []string{
		"example.com/corogoexit/leaf.Exit.coro",
		"example.com/corogoexit/mid.Exit.coro",
		"main.run.coro",
	} {
		if !strings.Contains(symbols, want) {
			t.Errorf("symbols do not contain %q", want)
		}
	}
}

func TestCrossPackageTerminalDefer(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	writeFile := func(name, contents string) {
		t.Helper()
		name = filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Dir(name), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(contents), 0o666); err != nil {
			t.Fatal(err)
		}
	}

	writeFile("go.mod", "module example.com/corodefer\n\ngo 1.28\n")
	writeFile("leaf/leaf.go", `package leaf

import "runtime"

//go:noinline
func Recover(value *any) {
	*value = recover()
}

//go:noinline
func indirectPanic(trace *string) {
	*trace += "leaf-call;"
	panic("nested panic")
}

//go:noinline
func Panic(trace *string) {
	defer func() {
		*trace += "leaf-cleanup;"
	}()
	indirectPanic(trace)
}

//go:noinline
func indirectExit() {
	runtime.Goexit()
}

//go:noinline
func Exit(trace *string, tag int) int {
	defer func() {
		if tag == 7 {
			*trace += "leaf-cleanup;"
		}
	}()
	indirectExit()
	panic("returned after Goexit")
}
`)
	writeFile("mid/mid.go", `package mid

import "example.com/corodefer/leaf"

//go:noinline
func Panic(trace *string) {
	*trace += "mid-call;"
	leaf.Panic(trace)
}

//go:noinline
func Exit(trace *string) int {
	defer func() {
		*trace += "mid-cleanup;"
	}()
	leaf.Exit(trace, 7)
	panic("returned after Exit")
}
`)
	writeFile("root/main.go", `package main

import (
	"example.com/corodefer/leaf"
	"example.com/corodefer/mid"
	"runtime"
)

var exitTrace string
var directTrace string

//go:noinline
func panicCase() (trace string, recovered any) {
	defer leaf.Recover(&recovered)
	defer mid.Panic(&trace)
	runtime.Gosched()
	panic("parent panic")
}

//go:noinline
func exitCase(done chan struct{}) {
	trace := ""
	defer func() {
		exitTrace = trace + "root-cleanup;"
		close(done)
	}()
	defer mid.Exit(&trace)
	runtime.Gosched()
}

//go:noinline
func directExit(done chan struct{}) {
	trace := ""
	defer func() {
		directTrace = trace + "root-cleanup;"
		close(done)
	}()
	defer runtime.Goexit()
	runtime.Gosched()
}

func main() {
	trace, recovered := panicCase()
	if trace != "mid-call;leaf-call;leaf-cleanup;" ||
		recovered != "nested panic" {
		panic("bad cross-package panic defer")
	}

	done := make(chan struct{})
	go exitCase(done)
	<-done
	if exitTrace != "leaf-cleanup;mid-cleanup;root-cleanup;" {
		panic("bad cross-package Goexit defer")
	}

	done = make(chan struct{})
	go directExit(done)
	<-done
	if directTrace != "root-cleanup;" {
		panic("bad direct Goexit defer")
	}
	println("stackless-coro-cross-defer-ok")
}
`)

	exe := filepath.Join(tmp, "coro-cross-defer")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=example.com/corodefer/...=-l -d=coro=4",
		"./root")
	cmd.Dir = tmp
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	out := string(data)
	if err != nil {
		t.Fatalf("cross-package terminal defer build failed: %v\n%s",
			err, out)
	}
	for _, name := range []string{
		"example.com/corodefer/leaf.Panic",
		"example.com/corodefer/leaf.Exit",
		"example.com/corodefer/mid.Panic",
		"example.com/corodefer/mid.Exit",
		"main.panicCase",
		"main.exitCase",
		"main.directExit",
	} {
		if strings.Contains(out, "coro: skip "+name+":") {
			t.Errorf("cross-package terminal defer function %s was not lowered\n%s",
				name, out)
		}
	}

	cmd = testenv.Command(t, exe)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-package terminal defer executable failed: %v\n%s",
			err, data)
	} else if want := "stackless-coro-cross-defer-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("executable output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nm of cross-package terminal defer executable failed: %v\n%s",
			err, data)
	}
	symbols := string(data)
	for _, want := range []string{
		"example.com/corodefer/leaf.Panic.coro",
		"example.com/corodefer/leaf.Exit.coro",
		"example.com/corodefer/mid.Panic.coro",
		"example.com/corodefer/mid.Exit.coro",
		"main.panicCase.coro",
		"main.exitCase.coro",
		"main.directExit.coro",
	} {
		if !strings.Contains(symbols, want) {
			t.Errorf("symbols do not contain %q", want)
		}
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.directExit.*`, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of direct Goexit defer failed: %v\n%s", err, data)
	}
	disassembly := string(data)
	if !strings.Contains(disassembly, "runtime.coroDeferGoexit") {
		t.Errorf("direct Goexit defer does not call runtime.coroDeferGoexit\n%s",
			disassembly)
	}
	if strings.Contains(disassembly, "runtime.Goexit") {
		t.Errorf("direct Goexit defer retains runtime.Goexit\n%s", disassembly)
	}
}

func TestCrossPackageFactory(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	writeFile := func(name, contents string) {
		t.Helper()
		name = filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Dir(name), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(contents), 0o666); err != nil {
			t.Fatal(err)
		}
	}

	writeFile("go.mod", "module example.com/corofactory\n\ngo 1.28\n")
	writeFile("leaf/leaf.go", `package leaf

import "runtime"

type Counter int

type Base int

//go:noinline
func Add(value int) int {
	runtime.Gosched()
	return value + 1
}

//go:noinline
func Variadic(base int, values ...int) int {
	runtime.Gosched()
	return base + sumValues(values)
}

func sumValues(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}

//go:noinline
func (counter *Counter) Add(delta int) (int, int) {
	runtime.Gosched()
	*counter += Counter(delta)
	return int(*counter), delta
}

//go:noinline
func (base Base) Sum(delta int) int {
	runtime.Gosched()
	return int(base) + delta
}

//go:noinline
func Pair(value *int) (int, int) {
	runtime.Gosched()
	*value = 42
	return 1, 2
}

//go:noinline
func Yield() {
	runtime.Gosched()
}
`)
	writeFile("mid/mid.go", `package mid

import "example.com/corofactory/leaf"

type Runner struct {
	Value leaf.Counter
}

type Calculator int

//go:noinline
func Add(value int) int {
	next := leaf.Add(value)
	return next + 1
}

//go:noinline
func Variadic(values ...int) int {
	total := leaf.Variadic(10, values...)
	return total + 1
}

//go:noinline
func (runner *Runner) Run(delta int) int {
	counter := &runner.Value
	total, _ := counter.Add(delta)
	return total + 1
}

//go:noinline
func (calculator Calculator) Sum(delta int) int {
	total := leaf.Base(calculator).Sum(delta)
	return total + 1
}

//go:noinline
func AddAgain(value int) int {
	next := leaf.Add(value)
	return next + 2
}

//go:noinline
func Discard(value int) {
	leaf.Add(value)
}

//go:noinline
func NestedAddReturn(value int) int {
	return leaf.Add(value) + 1
}

var nestedAddCallResult int

//go:noinline
func recordAdd(value int) {
	nestedAddCallResult = value
}

//go:noinline
func NestedAddCall(value int) int {
	nestedAddCallResult = 0
	recordAdd(leaf.Add(value))
	return nestedAddCallResult
}

//go:noinline
func NestedAddAssign(value int) int {
	result := 0
	result = leaf.Add(value) + 1
	return result
}

//go:noinline
func NestedAddTwice(value int) int {
	return leaf.Add(value) + leaf.Add(value+1)
}

//go:noinline
func NestedAddChain(value int) int {
	return Add(leaf.Add(value))
}

var unsafeNestedAddOrder int

//go:noinline
func beforeNestedAdd() int {
	unsafeNestedAddOrder = unsafeNestedAddOrder*10 + 1
	return 1
}

//go:noinline
func UnsafeNestedAdd(value int) int {
	unsafeNestedAddOrder = 0
	return beforeNestedAdd() + leaf.Add(value)
}

func UnsafeNestedAddOrder() int {
	return unsafeNestedAddOrder
}

//go:noinline
func DiscardPair(value *int) {
	leaf.Pair(value)
}

//go:noinline
func Pair(value *int) (int, int) {
	left, right := leaf.Pair(value)
	return left + 10, right + 20
}

//go:noinline
func First(value *int) int {
	first, _ := leaf.Pair(value)
	return first
}

//go:noinline
func NestedPair(value *int) int {
	total := sum(leaf.Pair(value))
	return total
}

var nestedPairCallResult int

//go:noinline
func recordPair(left, right int) {
	nestedPairCallResult = left + right
}

//go:noinline
func NestedPairCall(value *int) int {
	nestedPairCallResult = 0
	recordPair(leaf.Pair(value))
	return nestedPairCallResult
}

//go:noinline
func NestedPairAssign(value *int) int {
	total := 0
	total = sum(leaf.Pair(value))
	return total
}

//go:noinline
func split(left, right int) (int, int) {
	return left + 10, right + 20
}

//go:noinline
func NestedPairList(value *int) (int, int) {
	var left, right int
	left, right = split(leaf.Pair(value))
	return left, right
}

var complexOrder int

//go:noinline
func complexTarget(results []int) []int {
	complexOrder = complexOrder*10 + 1
	return results
}

//go:noinline
func complexValue(value *int) *int {
	complexOrder = complexOrder*10 + 2
	return value
}

//go:noinline
func ComplexPair(value *int) int {
	results := []int{0}
	index := 0
	complexOrder = 0
	complexTarget(results)[index], index = leaf.Pair(complexValue(value))
	return results[0] + index
}

func ComplexOrder() int {
	return complexOrder
}

//go:noinline
func sum(left, right int) int {
	return left + right
}

//go:noinline
func Yield() {
	leaf.Yield()
}
`)
	writeFile("root/main.go", `package main

import "example.com/corofactory/mid"

func main() {
	got := mid.Add(40)
	if got != 42 {
		println("cross-package-factory-bad")
		return
	}
	if got := mid.AddAgain(40); got != 43 {
		println("cross-package-factory-reuse-bad")
		return
	}
	mid.Discard(40)
	if got := mid.NestedAddReturn(40); got != 42 {
		println("nested-single-return-bad")
		return
	}
	if got := mid.NestedAddCall(40); got != 41 {
		println("nested-single-call-bad")
		return
	}
	if got := mid.NestedAddAssign(40); got != 42 {
		println("nested-single-assign-bad")
		return
	}
	if got := mid.NestedAddTwice(40); got != 83 {
		println("nested-single-twice-bad")
		return
	}
	if got := mid.NestedAddChain(39); got != 42 {
		println("nested-single-chain-bad")
		return
	}
	mid.Yield()
	println("cross-package-factory-ok")
}
`)
	writeFile("rootpair/main.go", `package main

import "example.com/corofactory/mid"

func main() {
	var runner mid.Runner
	if got := runner.Run(41); got != 42 || runner.Value != 41 {
		println("method-factory-bad")
		return
	}
	if got := mid.Calculator(10).Sum(30); got != 41 {
		println("value-method-factory-bad")
		return
	}
	if got := mid.Variadic(1, 2, 3); got != 17 {
		println("variadic-factory-bad")
		return
	}
	values := []int{4, 5}
	if got := mid.Variadic(values...); got != 20 {
		println("variadic-slice-factory-bad")
		return
	}
	value := 0
	left, right := mid.Pair(&value)
	if value != 42 || left != 11 || right != 22 {
		println("multi-result-assignment-bad")
		return
	}
	value = 0
	if first := mid.First(&value); value != 42 || first != 1 {
		println("multi-result-blank-bad")
		return
	}
	value = 0
	mid.DiscardPair(&value)
	if value != 42 {
		println("multi-result-discard-bad")
		return
	}
	value = 0
	if nested := mid.NestedPair(&value); value != 42 || nested != 3 {
		println("multi-result-return-expression-bad")
		return
	}
	value = 0
	if nested := mid.NestedPairCall(&value); value != 42 || nested != 3 {
		println("multi-result-call-expression-bad")
		return
	}
	value = 0
	if nested := mid.NestedPairAssign(&value); value != 42 || nested != 3 {
		println("multi-result-assignment-expression-bad")
		return
	}
	value = 0
	left, right = mid.NestedPairList(&value)
	if value != 42 || left != 11 || right != 22 {
		println("multi-result-assignment-list-expression-bad")
		return
	}
	println("multi-result-factory-ok")
}
`)
	writeFile("rootfallback/main.go", `package main

import "example.com/corofactory/mid"

func main() {
	nestedValue := 0
	complexValue := 0
	nested := mid.NestedPair(&nestedValue)
	complex := mid.ComplexPair(&complexValue)
	unsafe := mid.UnsafeNestedAdd(40)
	if nestedValue != 42 || complexValue != 42 ||
		nested != 3 || complex != 3 || mid.ComplexOrder() != 12 ||
		unsafe != 42 || mid.UnsafeNestedAddOrder() != 1 {
		println("multi-result-fallback-bad")
		return
	}
	println("multi-result-fallback-ok")
}
`)
	writeFile("rootplain/main.go", `package main

import (
	"example.com/corofactory/leaf"
	"example.com/corofactory/mid"
)

func main() {
	got := leaf.Add(41)
	if got != 42 {
		println("ordinary-entry-bad")
		return
	}
	var runner mid.Runner
	if got := runner.Run(41); got != 42 || runner.Value != 41 {
		println("ordinary-method-entry-bad")
		return
	}
	if got := mid.Variadic(1, 2, 3); got != 17 {
		println("ordinary-variadic-entry-bad")
		return
	}
	println("ordinary-entry-ok")
}
`)

	env := []string{
		"GOEXPERIMENT=coro",
		"GOCACHE=" + filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	}
	exe := filepath.Join(tmp, "factory")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=example.com/corofactory/...=-l -d=coro=4",
		"./root")
	cmd.Dir = tmp
	cmd.Env = append(cmd.Environ(), env...)
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cross-package factory build failed: %v\n%s", err, data)
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cross-package factory executable failed: %v\n%s", err, data)
	}
	if want := "cross-package-factory-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	for _, test := range []struct {
		caller string
		callee string
	}{
		{"Add", "Add"},
		{"AddAgain", "Add"},
		{"Discard", "Add"},
		{"NestedAddReturn", "Add"},
		{"NestedAddCall", "Add"},
		{"NestedAddAssign", "Add"},
		{"NestedAddTwice", "Add"},
		{"NestedAddChain", "Add"},
		{"Yield", "Yield"},
	} {
		cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
			"-s", `example.com/corofactory/mid\.`+test.caller+
				`\.coro\.func[0-9]+$`, exe)
		data, err = cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("objdump of %s resume function failed: %v\n%s",
				test.caller, err, data)
		}
		disassembly := string(data)
		factory := "example.com/corofactory/leaf." + test.callee + ".coro"
		if !strings.Contains(disassembly, factory) {
			t.Fatalf("%s resume does not call %s\n%s",
				test.caller, factory, disassembly)
		}
		for _, forbidden := range []string{
			"example.com/corofactory/leaf." + test.callee + "(SB)",
			"runtime.coroRun",
		} {
			if strings.Contains(disassembly, forbidden) {
				t.Errorf("%s resume contains %q\n%s",
					test.caller, forbidden, disassembly)
			}
		}
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `example.com/corofactory/mid\.NestedAddChain`+
			`\.coro\.func[0-9]+$`, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of nested coroutine chain failed: %v\n%s",
			err, data)
	}
	disassembly := string(data)
	if !strings.Contains(disassembly,
		"example.com/corofactory/mid.Add.coro") {
		t.Fatalf("nested coroutine chain does not call mid factory\n%s",
			disassembly)
	}
	if strings.Contains(disassembly,
		"example.com/corofactory/mid.Add(SB)") {
		t.Fatalf("nested coroutine chain calls public mid entry\n%s",
			disassembly)
	}

	pair := filepath.Join(tmp, "pair")
	cmd = testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", pair,
		"-gcflags=example.com/corofactory/...=-l -d=coro=4",
		"./rootpair")
	cmd.Dir = tmp
	cmd.Env = append(cmd.Environ(), env...)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("multi-result factory build failed: %v\n%s", err, data)
	}
	pairBuild := string(data)

	cmd = testenv.Command(t, pair)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("multi-result factory executable failed: %v\n%s", err, data)
	}
	if want := "multi-result-factory-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	for _, caller := range []string{"Pair", "First", "DiscardPair"} {
		cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
			"-s", `example.com/corofactory/mid\.`+caller+
				`\.coro\.func[0-9]+$`, pair)
		data, err = cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("objdump of multi-result %s failed: %v\n%s",
				caller, err, data)
		}
		disassembly := string(data)
		if !strings.Contains(disassembly,
			"example.com/corofactory/leaf.Pair.coro") {
			t.Fatalf("%s does not use the multi-result factory\n%s\nbuild:\n%s",
				caller, disassembly, pairBuild)
		}
		if strings.Contains(disassembly,
			"example.com/corofactory/leaf.Pair(SB)") {
			t.Fatalf("%s uses the public multi-result entry\n%s",
				caller, disassembly)
		}
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.main\.coro\.func[0-9]+$`, pair)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of method factory caller failed: %v\n%s", err, data)
	}
	disassembly = string(data)
	for _, method := range []string{
		"(*Runner).Run",
		"Calculator.Sum",
	} {
		factory := "example.com/corofactory/mid." + method + ".coro"
		if !strings.Contains(disassembly, factory) {
			t.Fatalf("root does not use method factory %s\n%s",
				factory, disassembly)
		}
		if strings.Contains(disassembly,
			"example.com/corofactory/mid."+method+"(SB)") {
			t.Fatalf("root uses public method entry %s\n%s",
				method, disassembly)
		}
	}
	variadicFactory := "example.com/corofactory/mid.Variadic.coro"
	if !strings.Contains(disassembly, variadicFactory) {
		t.Fatalf("root does not use variadic factory %s\n%s",
			variadicFactory, disassembly)
	}
	if strings.Contains(disassembly,
		"example.com/corofactory/mid.Variadic(SB)") {
		t.Fatalf("root uses public variadic entry\n%s", disassembly)
	}
	for _, name := range []string{
		"NestedPair",
		"NestedPairCall",
		"NestedPairAssign",
		"NestedPairList",
	} {
		factory := "example.com/corofactory/mid." + name + ".coro"
		if !strings.Contains(disassembly, factory) {
			t.Fatalf("root does not use nested expression factory %s\n%s",
				factory, disassembly)
		}
		if strings.Contains(disassembly,
			"example.com/corofactory/mid."+name+"(SB)") {
			t.Fatalf("root uses public nested expression entry %s\n%s",
				name, disassembly)
		}
	}

	for _, method := range []struct {
		resume string
		callee string
	}{
		{
			`example.com/corofactory/mid\.\(\*Runner\)\.Run`,
			"example.com/corofactory/leaf.(*Counter).Add",
		},
		{
			`example.com/corofactory/mid\.Calculator\.Sum`,
			"example.com/corofactory/leaf.Base.Sum",
		},
	} {
		cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
			"-s", method.resume+`\.coro\.func[0-9]+$`, pair)
		data, err = cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("objdump of method resume failed: %v\n%s", err, data)
		}
		disassembly = string(data)
		factory := method.callee + ".coro"
		if !strings.Contains(disassembly, factory) {
			t.Fatalf("method resume does not use method factory %s\n%s",
				factory, disassembly)
		}
		if strings.Contains(disassembly, method.callee+"(SB)") {
			t.Fatalf("method resume uses public method entry %s\n%s",
				method.callee, disassembly)
		}
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `example.com/corofactory/mid\.Variadic`+
			`\.coro\.func[0-9]+$`, pair)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of variadic resume failed: %v\n%s", err, data)
	}
	disassembly = string(data)
	if !strings.Contains(disassembly,
		"example.com/corofactory/leaf.Variadic.coro") {
		t.Fatalf("variadic resume does not use leaf factory\n%s",
			disassembly)
	}
	if strings.Contains(disassembly,
		"example.com/corofactory/leaf.Variadic(SB)") {
		t.Fatalf("variadic resume uses public leaf entry\n%s", disassembly)
	}

	for _, name := range []string{
		"NestedPair",
		"NestedPairCall",
		"NestedPairAssign",
		"NestedPairList",
	} {
		cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
			"-s", `example.com/corofactory/mid\.`+name+
				`\.coro\.func[0-9]+$`, pair)
		data, err = cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("objdump of nested expression resume %s failed: %v\n%s",
				name, err, data)
		}
		disassembly = string(data)
		if !strings.Contains(disassembly,
			"example.com/corofactory/leaf.Pair.coro") {
			t.Fatalf("nested expression resume %s does not use leaf factory\n%s",
				name, disassembly)
		}
		if strings.Contains(disassembly,
			"example.com/corofactory/leaf.Pair(SB)") {
			t.Fatalf("nested expression resume %s uses public leaf entry\n%s",
				name, disassembly)
		}
	}

	multiFallback := filepath.Join(tmp, "multi-fallback")
	cmd = testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", multiFallback,
		"-gcflags=example.com/corofactory/...=-l -d=coro=4",
		"./rootfallback")
	cmd.Dir = tmp
	fallbackEnv := []string{
		"GOEXPERIMENT=coro",
		"GOCACHE=" + filepath.Join(tmp, "fallback-gocache"),
		"GOWORK=off",
	}
	cmd.Env = append(cmd.Environ(), fallbackEnv...)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("multi-result fallback build failed: %v\n%s", err, data)
	}
	for _, want := range []string{
		"ComplexPair: await site 1: coroutine call has 2 results without matching assignment",
		"unsupported coroutine dependency example.com/corofactory/mid.ComplexPair",
		"UnsafeNestedAdd: nested await site 1",
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("multi-result fallback output does not contain %q\n%s",
				want, data)
		}
	}
	if unwanted := "NestedPair: nested await site"; strings.Contains(string(data), unwanted) {
		t.Fatalf("multi-result expression output contains %q\n%s",
			unwanted, data)
	}

	cmd = testenv.Command(t, multiFallback)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("multi-result fallback executable failed: %v\n%s", err, data)
	}
	if want := "multi-result-fallback-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `example.com/corofactory/mid\.ComplexPair$`, multiFallback)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of fallback ComplexPair failed: %v\n%s",
			err, data)
	}
	disassembly = string(data)
	if !strings.Contains(disassembly,
		"example.com/corofactory/leaf.Pair(SB)") {
		t.Fatalf("ComplexPair fallback does not use the public entry\n%s",
			disassembly)
	}
	if strings.Contains(disassembly,
		"example.com/corofactory/leaf.Pair.coro") {
		t.Fatalf("ComplexPair fallback uses the private factory\n%s",
			disassembly)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `example.com/corofactory/mid\.UnsafeNestedAdd$`,
		multiFallback)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of fallback UnsafeNestedAdd failed: %v\n%s",
			err, data)
	}
	disassembly = string(data)
	if !strings.Contains(disassembly,
		"example.com/corofactory/leaf.Add(SB)") {
		t.Fatalf("UnsafeNestedAdd fallback does not use the public entry\n%s",
			disassembly)
	}
	if strings.Contains(disassembly,
		"example.com/corofactory/leaf.Add.coro") {
		t.Fatalf("UnsafeNestedAdd fallback uses the private factory\n%s",
			disassembly)
	}

	ordinary := filepath.Join(tmp, "ordinary")
	cmd = testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", ordinary,
		"-gcflags=example.com/corofactory/...=-l -d=coro=4",
		"-gcflags=example.com/corofactory/rootplain=-l -d=coro=2",
		"./rootplain")
	cmd.Dir = tmp
	cmd.Env = append(cmd.Environ(), env...)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ordinary-entry build failed: %v\n%s", err, data)
	}

	cmd = testenv.Command(t, ordinary)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ordinary-entry executable failed: %v\n%s", err, data)
	}
	if want := "ordinary-entry-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `main\.main$`, ordinary)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of ordinary entry failed: %v\n%s", err, data)
	}
	disassembly = string(data)
	if !strings.Contains(disassembly,
		"example.com/corofactory/leaf.Add(SB)") {
		t.Fatalf("ordinary caller does not use the public Go entry\n%s",
			disassembly)
	}
	if strings.Contains(disassembly,
		"example.com/corofactory/leaf.Add.coro") {
		t.Fatalf("ordinary caller uses the private factory entry\n%s",
			disassembly)
	}
	if !strings.Contains(disassembly,
		"example.com/corofactory/mid.(*Runner).Run(SB)") {
		t.Fatalf("ordinary caller does not use the public method entry\n%s",
			disassembly)
	}
	if strings.Contains(disassembly,
		"example.com/corofactory/mid.(*Runner).Run.coro") {
		t.Fatalf("ordinary caller uses the private method factory\n%s",
			disassembly)
	}
	if !strings.Contains(disassembly,
		"example.com/corofactory/mid.Variadic(SB)") {
		t.Fatalf("ordinary caller does not use the public variadic entry\n%s",
			disassembly)
	}
	if strings.Contains(disassembly,
		"example.com/corofactory/mid.Variadic.coro") {
		t.Fatalf("ordinary caller uses the private variadic factory\n%s",
			disassembly)
	}

	fallback := filepath.Join(tmp, "fallback")
	cmd = testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", fallback,
		"-gcflags=example.com/corofactory/...=-l -d=coro=4",
		"-gcflags=example.com/corofactory/leaf=-l -d=coro=2",
		"./root")
	cmd.Dir = tmp
	cmd.Env = append(cmd.Environ(), env...)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("missing-capability build failed: %v\n%s", err, data)
	}
	if want := "unsupported coroutine dependency example.com/corofactory/leaf.Add"; !strings.Contains(string(data), want) {
		t.Fatalf("missing-capability output does not contain %q\n%s",
			want, data)
	}

	cmd = testenv.Command(t, fallback)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("missing-capability executable failed: %v\n%s", err, data)
	}
	if want := "cross-package-factory-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `example.com/corofactory/mid\.Add$`, fallback)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("objdump of missing-capability fallback failed: %v\n%s",
			err, data)
	}
	disassembly = string(data)
	if !strings.Contains(disassembly,
		"example.com/corofactory/leaf.Add(SB)") {
		t.Fatalf("fallback caller does not use the public Go entry\n%s",
			disassembly)
	}
	if strings.Contains(disassembly,
		"example.com/corofactory/leaf.Add.coro") {
		t.Fatalf("fallback caller uses the unavailable factory entry\n%s",
			disassembly)
	}
}

func TestRecursiveFactory(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"),
		[]byte("module example.com/cororecursive\n\ngo 1.28\n"),
		0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "main.go"), []byte(`package main

import "runtime"

const depth = 4096

//go:noinline
func sum(n int) int {
	if n == 0 {
		runtime.Gosched()
		return 0
	}
	child := sum(n - 1)
	return n + child
}

//go:noinline
func even(n int) bool {
	if n == 0 {
		runtime.Gosched()
		return true
	}
	result := odd(n - 1)
	return result
}

//go:noinline
func odd(n int) bool {
	if n == 0 {
		runtime.Gosched()
		return false
	}
	result := even(n - 1)
	return result
}

type counter struct {
	value int
}

//go:noinline
func (c *counter) descend(n int) {
	if n == 0 {
		runtime.Gosched()
		return
	}
	c.value++
	c.descend(n - 1)
}

func main() {
	got := sum(depth)
	want := depth * (depth + 1) / 2
	if got != want {
		panic("bad recursive sum")
	}
	gotEven := even(depth)
	gotOdd := even(depth + 1)
	if !gotEven || gotOdd {
		panic("bad mutual recursion")
	}
	var c counter
	c.descend(depth)
	if c.value != depth {
		panic("bad recursive method")
	}
	println("recursive-factory-ok")
}
`), 0o666); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(tmp, "recursive")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", exe,
		"-gcflags=example.com/cororecursive=-l -d=coro=4", ".")
	cmd.Dir = tmp
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=coro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("recursive factory build failed: %v\n%s", err, data)
	}
	buildOutput := string(data)
	for _, name := range []string{"sum", "even", "odd", "(*counter).descend"} {
		found := false
		for _, line := range strings.Split(buildOutput, "\n") {
			if strings.Contains(line, "coro: func=main."+name+" ") &&
				strings.Contains(line, "effect=may-suspend") &&
				strings.Contains(line, "recursive=true") &&
				strings.Contains(line, "primary=coro") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("build output does not report recursive function %s\n%s",
				name, buildOutput)
		}
	}
	if want := "coro: phase=lower lowered=5 skipped=0"; !strings.Contains(buildOutput, want) {
		t.Errorf("build output does not contain %q\n%s", want, buildOutput)
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("recursive factory executable failed: %v\n%s", err, data)
	}
	if want := "recursive-factory-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	for _, test := range []struct {
		resume  string
		factory string
		public  string
	}{
		{
			`main\.sum\.coro\.func[0-9]+$`,
			"main.sum.coro",
			"main.sum(SB)",
		},
		{
			`main\.even\.coro\.func[0-9]+$`,
			"main.odd.coro",
			"main.odd(SB)",
		},
		{
			`main\.odd\.coro\.func[0-9]+$`,
			"main.even.coro",
			"main.even(SB)",
		},
		{
			`main\.\(\*counter\)\.descend\.coro\.func[0-9]+$`,
			"main.(*counter).descend.coro",
			"main.(*counter).descend(SB)",
		},
	} {
		cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
			"-s", test.resume, exe)
		data, err = cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("objdump of %s failed: %v\n%s",
				test.resume, err, data)
		}
		disassembly := string(data)
		if !strings.Contains(disassembly, test.factory) {
			t.Errorf("%s does not call recursive factory %s\n%s",
				test.resume, test.factory, disassembly)
		}
		for _, forbidden := range []string{test.public, "runtime.coroRun"} {
			if strings.Contains(disassembly, forbidden) {
				t.Errorf("%s contains %q\n%s",
					test.resume, forbidden, disassembly)
			}
		}
	}

	ordinary := filepath.Join(tmp, "ordinary")
	cmd = testenv.Command(t, testenv.GoToolPath(t), "build",
		"-o", ordinary, "-gcflags=example.com/cororecursive=-l", ".")
	cmd.Dir = tmp
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=none",
		"GOCACHE="+filepath.Join(tmp, "ordinary-cache"),
		"GOWORK=off",
	)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ordinary recursive build failed: %v\n%s", err, data)
	}
	cmd = testenv.Command(t, ordinary)
	if data, err = cmd.CombinedOutput(); err != nil {
		t.Fatalf("ordinary recursive executable failed: %v\n%s", err, data)
	}
	if want := "recursive-factory-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("ordinary output does not contain %q\n%s", want, data)
	}
	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", ordinary)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nm of ordinary recursive executable failed: %v\n%s",
			err, data)
	}
	symbols := string(data)
	for _, name := range []string{
		"main.sum.coro",
		"main.even.coro",
		"main.odd.coro",
		"main.(*counter).descend.coro",
	} {
		if strings.Contains(symbols, name) {
			t.Fatalf("ordinary executable contains private symbol %s\n%s",
				name, data)
		}
	}
}

func TestRejectMixedArchive(t *testing.T) {
	testenv.MustHaveGoBuild(t)

	tmp := t.TempDir()
	leafDir := filepath.Join(tmp, "leaf")
	midDir := filepath.Join(tmp, "mid")
	for _, dir := range []string{leafDir, midDir} {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"),
		[]byte("module example.com/coropoc\n\ngo 1.28\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leafDir, "leaf.go"),
		[]byte("package leaf\n\nfunc F() {}\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(midDir, "mid.go"),
		[]byte("package mid\n\nimport \"example.com/coropoc/leaf\"\n\nfunc F() { leaf.F() }\n"), 0o666); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(tmp, "leaf.a")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", archive, "./leaf")
	cmd.Dir = tmp
	cmd.Env = append(cmd.Environ(),
		"GOEXPERIMENT=nocoro",
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOWORK=off",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building archive without summary failed: %v\n%s", err, out)
	}

	importcfg := filepath.Join(tmp, "importcfg")
	cfg := "packagefile example.com/coropoc/leaf=" + archive + "\n"
	if err := os.WriteFile(importcfg, []byte(cfg), 0o666); err != nil {
		t.Fatal(err)
	}
	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "compile",
		"-p=example.com/coropoc/mid",
		"-importcfg="+importcfg,
		"-o", filepath.Join(tmp, "mid.a"),
		filepath.Join(midDir, "mid.go"))
	cmd.Env = append(cmd.Environ(), "GOEXPERIMENT=coro")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("mixed archive compile unexpectedly succeeded\n%s", out)
	}
	for _, want := range []string{
		"could not import example.com/coropoc/leaf",
		"X:coro",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("output does not contain %q\n%s", want, out)
		}
	}
}
