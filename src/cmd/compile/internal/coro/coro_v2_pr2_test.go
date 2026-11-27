// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const pr2MainProgram = `package main

import (
	"bytes"
	"example.com/coropr2/pkgb"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// 1. Coroutine-aware sync.Mutex, sync.RWMutex, sync.WaitGroup, sync.Cond
func testSyncMutexAndRWMutex() {
	var mu sync.Mutex
	var wg sync.WaitGroup
	var counter int
	const coros = 16
	const iters = 100

	// Mix stackless coroutines and an ordinary goroutine (via channel handoff).
	wg.Add(coros)
	for i := 0; i < coros; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				mu.Lock()
				v := counter
				if id == 0 && j == 5 {
					// Trigger starvation-mode handoff (>1ms) at least once.
					time.Sleep(2 * time.Millisecond)
				} else {
					runtime.Gosched()
				}
				counter = v + 1
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if counter != coros*iters {
		panic(fmt.Sprintf("mutex counter=%d want %d", counter, coros*iters))
	}

	// RWMutex: concurrent readers + exclusive writers.
	var rw sync.RWMutex
	var activeWriters, activeReaders atomic.Int32
	var rwSum int
	var rwg sync.WaitGroup
	const writers = 6
	const readers = 12
	rwg.Add(writers + readers)
	for i := 0; i < writers; i++ {
		go func() {
			defer rwg.Done()
			for j := 0; j < 40; j++ {
				rw.Lock()
				if activeWriters.Add(1) != 1 || activeReaders.Load() != 0 {
					panic("rwmutex writer exclusivity violated")
				}
				rwSum++
				runtime.Gosched()
				activeWriters.Add(-1)
				rw.Unlock()
			}
		}()
	}
	for i := 0; i < readers; i++ {
		go func() {
			defer rwg.Done()
			for j := 0; j < 40; j++ {
				rw.RLock()
				if activeWriters.Load() != 0 {
					panic("rwmutex reader observed active writer")
				}
				activeReaders.Add(1)
				_ = rwSum
				runtime.Gosched()
				activeReaders.Add(-1)
				rw.RUnlock()
			}
		}()
	}
	rwg.Wait()
	if rwSum != writers*40 {
		panic(fmt.Sprintf("rwSum=%d want %d", rwSum, writers*40))
	}
}

func testSyncCond() {
	// Cond with *sync.Mutex (Signal + Broadcast)
	var mu sync.Mutex
	cond := sync.NewCond(&mu)
	ready := 0
	const N = 12
	var arrived atomic.Int32
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(id int) {
			defer wg.Done()
			mu.Lock()
			arrived.Add(1)
			for ready == 0 {
				cond.Wait()
			}
			mu.Unlock()
		}(i)
	}
	for arrived.Load() < N {
		runtime.Gosched()
	}
	mu.Lock()
	ready = 1
	cond.Signal()
	mu.Unlock()
	runtime.Gosched()
	mu.Lock()
	cond.Broadcast()
	mu.Unlock()
	wg.Wait()

	// Cond with *sync.RWMutex and RLocker
	var rw sync.RWMutex
	rwCond := sync.NewCond(rw.RLocker())
	var open bool
	var rwArrived atomic.Int32
	wg.Add(4)
	for i := 0; i < 4; i++ {
		go func() {
			defer wg.Done()
			rw.RLock()
			rwArrived.Add(1)
			for !open {
				rwCond.Wait()
			}
			rw.RUnlock()
		}()
	}
	for rwArrived.Load() < 4 {
		runtime.Gosched()
	}
	rw.Lock()
	open = true
	rw.Unlock()
	rwCond.Broadcast()
	wg.Wait()
}

// 2. Interface io.Reader/io.Writer/net.Conn + net.Listener.Accept (TCP & Unix)
func ifaceReadFull(r io.Reader, b []byte) error {
	for off := 0; off < len(b); {
		n, err := r.Read(b[off:])
		off += n
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func ifaceWriteFull(w io.Writer, b []byte) error {
	for off := 0; off < len(b); {
		n, err := w.Write(b[off:])
		off += n
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

type chanReader struct {
	ch chan byte
}

func (cr *chanReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b, ok := <-cr.ch
	if !ok {
		return 0, io.EOF
	}
	p[0] = b
	return 1, nil
}

func handleNetConn(c net.Conn, wg *sync.WaitGroup) {
	defer wg.Done()
	defer c.Close()
	var buf [32]byte
	for i := 0; i < 4; i++ {
		if err := ifaceReadFull(c, buf[:]); err != nil {
			panic(fmt.Sprintf("server read: %v", err))
		}
		buf[0] ^= 0x3c
		if err := ifaceWriteFull(c, buf[:]); err != nil {
			panic(fmt.Sprintf("server write: %v", err))
		}
	}
}

func runAcceptLoop(ln net.Listener, count int, wg *sync.WaitGroup) {
	defer wg.Done()
	var hwg sync.WaitGroup
	for i := 0; i < count; i++ {
		c, err := ln.Accept()
		if err != nil {
			panic(fmt.Sprintf("Accept(%d): %v", i, err))
		}
		hwg.Add(1)
		go handleNetConn(c, &hwg)
	}
	hwg.Wait()
}

func testInterfaceIOAndAccept(tmpDir string) {
	// 2a. User-defined suspending io.Reader + *bytes.Reader inline fallback.
	cr := &chanReader{ch: make(chan byte)}
	go func() {
		for _, b := range []byte("coro") {
			runtime.Gosched()
			cr.ch <- b
		}
		close(cr.ch)
	}()
	var got [4]byte
	if err := ifaceReadFull(cr, got[:]); err != nil || string(got[:]) != "coro" {
		panic(fmt.Sprintf("chanReader got=%q err=%v", got, err))
	}
	br := bytes.NewReader([]byte("fast"))
	if err := ifaceReadFull(br, got[:]); err != nil || string(got[:]) != "fast" {
		panic(fmt.Sprintf("bytes.Reader got=%q err=%v", got, err))
	}

	// 2b. *os.File pipe behind io.Reader / io.Writer interfaces.
	pr, pw, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	var pipeWG sync.WaitGroup
	pipeWG.Add(2)
	go func() {
		defer pipeWG.Done()
		var r io.Reader = pr
		var buf [16]byte
		if err := ifaceReadFull(r, buf[:]); err != nil {
			panic(err)
		}
		if string(buf[:]) != "0123456789abcdef" {
			panic(fmt.Sprintf("pipe iface read=%q", buf))
		}
	}()
	go func() {
		defer pipeWG.Done()
		var w io.Writer = pw
		if err := ifaceWriteFull(w, []byte("0123456789abcdef")); err != nil {
			panic(err)
		}
	}()
	pipeWG.Wait()
	pr.Close()
	pw.Close()

	// 2c. net.Listener (TCP) Accept loop inside a stackless coroutine + net.Conn handlers.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	const clients = 6
	var srvWG sync.WaitGroup
	srvWG.Add(1)
	go runAcceptLoop(ln, clients, &srvWG)

	var cliWG sync.WaitGroup
	for i := 0; i < clients; i++ {
		c, err := net.Dial("tcp4", ln.Addr().String())
		if err != nil {
			panic(err)
		}
		cliWG.Add(1)
		go func(conn net.Conn, id byte) {
			defer cliWG.Done()
			defer conn.Close()
			var sbuf, rbuf [32]byte
			for r := 0; r < 4; r++ {
				sbuf[0] = id + byte(r)
				if err := ifaceWriteFull(conn, sbuf[:]); err != nil {
					panic(err)
				}
				if err := ifaceReadFull(conn, rbuf[:]); err != nil {
					panic(err)
				}
				if rbuf[0] != sbuf[0]^0x3c {
					panic(fmt.Sprintf("tcp echo mismatch: got %x want %x", rbuf[0], sbuf[0]^0x3c))
				}
			}
		}(c, byte(i+1))
	}
	cliWG.Wait()
	srvWG.Wait()
	ln.Close()

	// 2d. Concrete *net.UnixListener.AcceptUnix inside a coroutine.
	sockPath := filepath.Join(tmpDir, fmt.Sprintf("u%d.s", time.Now().UnixNano()%100000))
	uln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		panic(err)
	}
	uDone := make(chan error, 1)
	go func() {
		uc, err := uln.AcceptUnix()
		if err != nil {
			uDone <- err
			return
		}
		defer uc.Close()
		var buf [8]byte
		if err := ifaceReadFull(uc, buf[:]); err != nil {
			uDone <- err
			return
		}
		buf[0] ^= 0xff
		uDone <- ifaceWriteFull(uc, buf[:])
	}()
	uc, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		panic(err)
	}
	var ub [8]byte
	ub[0] = 0x5a
	if err := ifaceWriteFull(uc, ub[:]); err != nil {
		panic(err)
	}
	if err := ifaceReadFull(uc, ub[:]); err != nil || ub[0] != 0xa5 {
		panic(fmt.Sprintf("unix echo got=%x err=%v", ub[0], err))
	}
	uc.Close()
	if err := <-uDone; err != nil {
		panic(err)
	}
	uln.Close()
}

// 3. Cross-package & dynamic zero-stack await + cross-package go spawn
type extRunner interface {
	Do(ch chan int, x int) int
	DoVal(ch chan int, x int) int
}

func testCrossPkgAndDynAwait() {
	ch := make(chan int, 8)
	// Cross-package go spawn of top-level func and pointer/value methods.
	go pkgb.SendVal(ch, 10)
	pw := &pkgb.Worker{Base: 100}
	go pw.Send(ch, 20)
	vw := pkgb.ValWorker{Bias: 7}
	go vw.SendVal(ch, 3)

	sum := 0
	for i := 0; i < 3; i++ {
		sum += <-ch
	}
	if sum != (10 + 120 + 10) {
		panic(fmt.Sprintf("cross-pkg go sum=%d want 140", sum))
	}

	// Cross-package synchronous zero-stack await (multi-return func, ptr method, val method).
	c1 := make(chan int, 1)
	go func() {
		runtime.Gosched()
		c1 <- 5
	}()
	v, tag := pkgb.Compute(c1, 3)
	if v != 15 || tag != "ok:15" {
		panic(fmt.Sprintf("pkgb.Compute=(%d,%q)", v, tag))
	}

	// Dynamic func variable holding cross-package coroutine.
	var dynFn func(chan int, int) (int, string) = pkgb.Compute
	go func() {
		runtime.Gosched()
		c1 <- 6
	}()
	v2, tag2 := dynFn(c1, 4)
	if v2 != 24 || tag2 != "ok:24" {
		panic(fmt.Sprintf("dynFn=(%d,%q)", v2, tag2))
	}

	// Cross-package ptr & value method await (direct and via interface).
	go func() {
		runtime.Gosched()
		c1 <- 9
	}()
	if got := pw.Do(c1, 2); got != 111 {
		panic(fmt.Sprintf("pw.Do=%d want 111", got))
	}
	go func() {
		runtime.Gosched()
		c1 <- 11
	}()
	if got := vw.DoVal(c1, 4); got != 22 {
		panic(fmt.Sprintf("vw.DoVal=%d want 22", got))
	}

	var r extRunner = pw
	go func() {
		runtime.Gosched()
		c1 <- 8
	}()
	if got := r.Do(c1, 1); got != 109 {
		panic(fmt.Sprintf("iface Do=%d want 109", got))
	}
	go func() {
		runtime.Gosched()
		c1 <- 13
	}()
	if got := r.DoVal(c1, 5); got != 118 {
		panic(fmt.Sprintf("iface DoVal=%d want 118", got))
	}

	// Value receiver stored in interface (verifies (*T).M wrapper registration).
	type valOnly interface {
		DoVal(ch chan int, x int) int
	}
	var vr valOnly = vw
	go func() {
		runtime.Gosched()
		c1 <- 20
	}()
	if got := vr.DoVal(c1, 3); got != 30 {
		panic(fmt.Sprintf("val iface DoVal=%d want 30", got))
	}
}

// 4. Generic shape coroutines (value & pointer shapes + nested closure)
func mapChan[T, U any](in <-chan T, fn func(T) U) <-chan U {
	out := make(chan U)
	go func() {
		defer close(out)
		for v := range in {
			runtime.Gosched()
			out <- fn(v)
		}
	}()
	return out
}

func collectChan[T any](in <-chan T) []T {
	var res []T
	for v := range in {
		res = append(res, v)
	}
	return res
}

func testGenerics() {
	// Value shape: int -> int
	inInt := make(chan int)
	go func() {
		for i := 1; i <= 5; i++ {
			inInt <- i
		}
		close(inInt)
	}()
	doubled := collectChan(mapChan(inInt, func(x int) int { return x * 2 }))
	if len(doubled) != 5 || doubled[0] != 2 || doubled[4] != 10 {
		panic(fmt.Sprintf("generic int=%v", doubled))
	}

	// Pointer shape: *string -> int (exercises GC pointer bitmap on shape frames)
	inPtr := make(chan *string)
	go func() {
		for _, s := range []string{"a", "bb", "ccc"} {
			v := s
			inPtr <- &v
		}
		close(inPtr)
	}()
	lens := collectChan(mapChan(inPtr, func(s *string) int { return len(*s) }))
	if len(lens) != 3 || lens[0] != 1 || lens[1] != 2 || lens[2] != 3 {
		panic(fmt.Sprintf("generic ptr=%v", lens))
	}
}

// 5. Preemption fairness across tight non-Gosched channel ping-pong
func testPreemptionFairness() {
	ping := make(chan int)
	pong := make(chan int)
	stop := make(chan struct{})
	var done atomic.Bool

	go func() {
		for {
			select {
			case v := <-ping:
				pong <- v + 1
			case <-stop:
				return
			}
		}
	}()
	go func() {
		for i := 0; !done.Load(); i++ {
			ping <- i
			<-pong
		}
		close(stop)
	}()

	// Independent coroutine must make progress even under GOMAXPROCS=1.
	var ticks atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			time.Sleep(time.Millisecond)
			ticks.Add(1)
		}
		done.Store(true)
	}()
	wg.Wait()
	if ticks.Load() != 20 {
		panic(fmt.Sprintf("preemption ticks=%d", ticks.Load()))
	}
}

func main() {
	tmpDir := os.Args[1]
	for _, p := range []int{1, 2, 4} {
		runtime.GOMAXPROCS(p)
		testSyncMutexAndRWMutex()
		testSyncCond()
		testInterfaceIOAndAccept(tmpDir)
		testCrossPkgAndDynAwait()
		testGenerics()
		testPreemptionFairness()
	}
	fmt.Println("CORO_V2_PR2_OK")
}
`

const pr2PkgBProgram = `package pkgb

import (
	"fmt"
	"runtime"
)

func SendVal(ch chan int, x int) {
	runtime.Gosched()
	ch <- x
}

func Compute(ch chan int, mul int) (int, string) {
	v := <-ch
	runtime.Gosched()
	res := v * mul
	return res, fmt.Sprintf("ok:%d", res)
}

type Worker struct {
	Base int
}

func (w *Worker) Send(ch chan int, x int) {
	runtime.Gosched()
	ch <- w.Base + x
}

func (w *Worker) Do(ch chan int, x int) int {
	v := <-ch
	runtime.Gosched()
	return w.Base + v + x
}

func (w *Worker) DoVal(ch chan int, x int) int {
	v := <-ch
	runtime.Gosched()
	return w.Base + v + x
}

type ValWorker struct {
	Bias int
}

func (w ValWorker) SendVal(ch chan int, x int) {
	runtime.Gosched()
	ch <- w.Bias + x
}

func (w ValWorker) DoVal(ch chan int, x int) int {
	v := <-ch
	runtime.Gosched()
	return w.Bias + v + x
}
`

func TestCoroV2SyncIOAcceptCrossPkgGenerics(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	sockDir, err := os.MkdirTemp("", "cu")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/coropr2\n\ngo 1.25\n"), 0644); err != nil {
		t.Fatal(err)
	}
	pkgBDir := filepath.Join(dir, "pkgb")
	if err := os.MkdirAll(pkgBDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgBDir, "pkgb.go"), []byte(pr2PkgBProgram), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(pr2MainProgram), 0644); err != nil {
		t.Fatal(err)
	}
	goBin := testenv.GoToolPath(t)

	runMode := func(name, goexp string, extra []string) {
		t.Run(name, func(t *testing.T) {
			bin := filepath.Join(dir, "app_"+name)
			args := append([]string{"build"}, extra...)
			args = append(args, "-o", bin, ".")
			cmd := exec.Command(goBin, args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOEXPERIMENT="+goexp)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build failed: %v\n%s", err, out)
			}
			if goexp == "coro" {
				s := string(out)
				if strings.Count(s, "coro: phase=lower") < 2 {
					t.Fatalf("expected lowering diagnostics for both pkgb and main:\n%s", s)
				}
			}
			run := exec.Command(bin, sockDir)
			runOut, err := run.CombinedOutput()
			if err != nil {
				t.Fatalf("run failed: %v\n%s", err, runOut)
			}
			if strings.TrimSpace(string(runOut)) != "CORO_V2_PR2_OK" {
				t.Fatalf("unexpected output: %q", runOut)
			}
		})
	}

	runMode("coro_noinline", "coro", []string{"-gcflags=./...=-l -d=coro=4"})
	runMode("coro_inline", "coro", []string{"-gcflags=./...=-d=coro=4"})
	runMode("coro_staticlockranking", "coro,staticlockranking", []string{"-gcflags=./...=-d=coro=4"})
	runMode("coro_race", "coro", []string{"-race", "-gcflags=./...=-d=coro=4"})
	runMode("nocoro", "nocoro", nil)
}
