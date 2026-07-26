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
	corort.DirectBlock(&gate)
	corort.AsyncDouble(asyncReadFD, asyncWriteFD, 21, &asyncResult, &asyncErrno)
}

//go:noinline
func replacement() {
	atomic.StoreUint32(&logicalProgress, 1)
	runtime.Gosched()
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
