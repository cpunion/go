// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"fmt"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
		"coro: site=1 func=p.yielding kind=yield foreign=-",
		"coro: site=1 func=p.yieldCaller kind=await foreign=-",
		"coro: site=1 func=p.sleeping kind=timer foreign=-",
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
	atomic.StoreUint32(&gate, 1)
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
		asyncResult != 42 || asyncErrno != 0 {
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
	if (!coro_on_native_thread_stack()) {
		return UINT64_MAX;
	}
	return a + b;
}

void coro_block_until(uint32_t *gate) {
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
	if want := "coro: phase=lower lowered=2"; !strings.Contains(out, want) {
		t.Fatalf("output does not contain %q\n%s", want, out)
	}

	cmd = testenv.Command(t, exe)
	data, err = cmd.CombinedOutput()
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

//go:noinline
func ComplexPair(value *int) int {
	results := []int{0}
	index := 0
	results[index], index = leaf.Pair(value)
	return results[0] + index
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
	if nestedValue != 42 || complexValue != 42 ||
		nested != 3 || complex != 3 {
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
	disassembly := string(data)
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
		"NestedPair: nested await site 1",
		"ComplexPair: await site 1: coroutine call has 2 results without matching assignment",
		"unsupported coroutine dependency example.com/corofactory/mid.ComplexPair",
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("multi-result fallback output does not contain %q\n%s",
				want, data)
		}
	}

	cmd = testenv.Command(t, multiFallback)
	data, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("multi-result fallback executable failed: %v\n%s", err, data)
	}
	if want := "multi-result-fallback-ok"; !strings.Contains(string(data), want) {
		t.Fatalf("output does not contain %q\n%s", want, data)
	}

	for _, caller := range []string{"NestedPair", "ComplexPair"} {
		cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
			"-s", `example.com/corofactory/mid\.`+caller+`$`, multiFallback)
		data, err = cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("objdump of fallback %s failed: %v\n%s",
				caller, err, data)
		}
		disassembly := string(data)
		if !strings.Contains(disassembly,
			"example.com/corofactory/leaf.Pair(SB)") {
			t.Fatalf("%s fallback does not use the public entry\n%s",
				caller, disassembly)
		}
		if strings.Contains(disassembly,
			"example.com/corofactory/leaf.Pair.coro") {
			t.Fatalf("%s fallback uses the private factory\n%s",
				caller, disassembly)
		}
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
