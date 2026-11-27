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

const e2eProgram = `package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync/atomic"
	"time"
)

// 1. Channels, close, comma-ok, range over chan
func testChannels() {
	ch := make(chan int)
	go func() {
		for i := 1; i <= 10; i++ {
			runtime.Gosched()
			ch <- i
		}
		close(ch)
	}()
	sum := 0
	for v := range ch {
		sum += v
	}
	if sum != 55 {
		panic(fmt.Sprintf("range chan sum=%d want 55", sum))
	}
	v, ok := <-ch
	if v != 0 || ok {
		panic(fmt.Sprintf("closed chan got (%d, %v)", v, ok))
	}
}

// 2. Multi-case select with sends, recvs, nil chan, default, closed chan
func testSelect() {
	c1 := make(chan int, 1)
	c2 := make(chan int)
	var nilCh chan int
	gotDefault := false
	select {
	case <-nilCh:
		panic("nil chan selected")
	case <-c2:
		panic("empty c2 selected")
	default:
		gotDefault = true
	}
	if !gotDefault {
		panic("default not taken")
	}

	go func() {
		time.Sleep(2 * time.Millisecond)
		c2 <- 99
	}()
	var got int
	var ok bool
	select {
	case <-nilCh:
		panic("nil chan selected")
	case got, ok = <-c2:
	}
	if got != 99 || !ok {
		panic(fmt.Sprintf("select recv got (%d, %v)", got, ok))
	}

	// Select send
	select {
	case c1 <- 42:
	case nilCh <- 1:
		panic("nil send selected")
	}
	if x := <-c1; x != 42 {
		panic(fmt.Sprintf("select send got %d", x))
	}
}

// 3. Real os.Pipe and net.TCPConn I/O across suspension points
func pipeReadFull(f *os.File, b []byte) error {
	for off := 0; off < len(b); {
		n, err := f.Read(b[off:])
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

func pipeWriteFull(f *os.File, b []byte) error {
	for off := 0; off < len(b); {
		n, err := f.Write(b[off:])
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

func runPipeEcho(r1, w1, r2, w2 *os.File) error {
	done := make(chan error, 2)
	go func() {
		var buf [64]byte
		for i := 0; i < 8; i++ {
			if err := pipeReadFull(r1, buf[:]); err != nil {
				done <- err
				return
			}
			buf[0] ^= 0x55
			if err := pipeWriteFull(w2, buf[:]); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	go func() {
		var sbuf, rbuf [64]byte
		for i := 0; i < 8; i++ {
			sbuf[0] = byte(i + 1)
			if err := pipeWriteFull(w1, sbuf[:]); err != nil {
				done <- err
				return
			}
			if err := pipeReadFull(r2, rbuf[:]); err != nil {
				done <- err
				return
			}
			if rbuf[0] != sbuf[0]^0x55 {
				done <- io.ErrUnexpectedEOF
				return
			}
		}
		done <- nil
	}()
	if err := <-done; err != nil {
		return err
	}
	return <-done
}

func tcpReadFull(c *net.TCPConn, b []byte) error {
	for off := 0; off < len(b); {
		n, err := c.Read(b[off:])
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

func tcpWriteFull(c *net.TCPConn, b []byte) error {
	for off := 0; off < len(b); {
		n, err := c.Write(b[off:])
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

func runTCPEcho(c, s *net.TCPConn) error {
	done := make(chan error, 2)
	go func() {
		var buf [64]byte
		for i := 0; i < 8; i++ {
			if err := tcpReadFull(s, buf[:]); err != nil {
				done <- err
				return
			}
			buf[0] ^= 0xaa
			if err := tcpWriteFull(s, buf[:]); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	go func() {
		var sbuf, rbuf [64]byte
		for i := 0; i < 8; i++ {
			sbuf[0] = byte(i + 7)
			if err := tcpWriteFull(c, sbuf[:]); err != nil {
				done <- err
				return
			}
			if err := tcpReadFull(c, rbuf[:]); err != nil {
				done <- err
				return
			}
			if rbuf[0] != sbuf[0]^0xaa {
				done <- io.ErrUnexpectedEOF
				return
			}
		}
		done <- nil
	}()
	if err := <-done; err != nil {
		return err
	}
	return <-done
}

// 4. Open interface & dynamic func value dispatch + opaque fallback
type workerIface interface {
	Work(x int, out chan int)
}

type addWorker struct{ base int }

func (w *addWorker) Work(x int, out chan int) {
	runtime.Gosched()
	out <- w.base + x
}

type mulWorker struct{ factor int }

func (w *mulWorker) Work(x int, out chan int) {
	runtime.Gosched()
	out <- w.factor * x
}

func dynTopFunc(x int, out chan int) {
	runtime.Gosched()
	out <- x * 10
}

func plainTopFunc(x int, out chan int) {
	// Does not suspend directly if we only do atomic? Wait: sending on ch suspends!
	// Use atomic for a truly plain unlowered function tested via coroSpawnOpaque.
	_ = x
	_ = out
}

func testDynamicDispatch() {
	out := make(chan int, 8)
	ws := []workerIface{&addWorker{base: 100}, &mulWorker{factor: 3}}
	for i, w := range ws {
		go w.Work(i+2, out)
	}
	fn := dynTopFunc
	go fn(5, out)

	var plainDone atomic.Int32
	plainFn := func(v int32) { plainDone.Add(v) }
	go plainFn(7)

	sum := 0
	for i := 0; i < 3; i++ {
		sum += <-out
	}
	// (100+2) + (3*3) + (5*10) = 102 + 9 + 50 = 161
	if sum != 161 {
		panic(fmt.Sprintf("dynamic dispatch sum=%d want 161", sum))
	}
	for plainDone.Load() != 7 {
		runtime.Gosched()
	}
}

// 5. Structured await, multi-return, recursion, closures (by-val & by-ref)
func fibCoro(n int) (int, int) {
	runtime.Gosched()
	if n <= 1 {
		return n, 1
	}
	a, ca := fibCoro(n - 1)
	b, cb := fibCoro(n - 2)
	return a + b, ca + cb + 1
}

func testClosuresAndRecursion() {
	val, calls := fibCoro(7)
	if val != 13 || calls != 41 {
		panic(fmt.Sprintf("fibCoro(7)=(%d,%d) want (13,41)", val, calls))
	}
	// Mutated by-ref capture + loopvar capture
	ch := make(chan int, 10)
	for i := 0; i < 10; i++ {
		go func() {
			runtime.Gosched()
			ch <- i
		}()
	}
	sum := 0
	for i := 0; i < 10; i++ {
		sum += <-ch
	}
	if sum != 45 {
		panic(fmt.Sprintf("loopvar capture sum=%d want 45", sum))
	}
}

// 6. Control flow across suspension: range (slice, int, string, map), switch, fallthrough, type switch, &&/||, goto
func yieldTrue(cnt *int) bool {
	*cnt++
	runtime.Gosched()
	return true
}

func yieldFalse(cnt *int) bool {
	*cnt++
	runtime.Gosched()
	return false
}

func testControlFlow() {
	// Range over slice, int, string (UTF-8), map (with delete during iteration)
	sl := []int{10, 20, 30}
	sSum := 0
	for i, v := range sl {
		runtime.Gosched()
		sSum += i*100 + v
	}
	if sSum != 360 {
		panic(fmt.Sprintf("slice range %d", sSum))
	}

	iSum := 0
	for i := range 5 {
		runtime.Gosched()
		iSum += i
	}
	if iSum != 10 {
		panic(fmt.Sprintf("int range %d", iSum))
	}

	runes := 0
	for _, r := range "a世b" {
		runtime.Gosched()
		runes += int(r)
	}
	if runes != int('a')+int('世')+int('b') {
		panic(fmt.Sprintf("string range %d", runes))
	}

	m := map[string]int{"a": 1, "b": 2, "c": 3}
	mSum := 0
	for k, v := range m {
		runtime.Gosched()
		mSum += v
		if k == "b" {
			delete(m, "b")
		}
	}
	if mSum != 6 || len(m) != 2 {
		panic(fmt.Sprintf("map range sum=%d len=%d", mSum, len(m)))
	}

	// Switch with fallthrough & labeled break
	swAcc := 0
outer:
	for k := 0; k < 3; k++ {
		switch k {
		case 0:
			runtime.Gosched()
			swAcc += 1
			fallthrough
		case 1:
			runtime.Gosched()
			swAcc += 10
		default:
			break outer
		}
	}
	if swAcc != 21 {
		panic(fmt.Sprintf("switch acc=%d want 21", swAcc))
	}

	// Type switch across suspension
	var items = []any{int(5), "ok", nil}
	tsAcc := 0
	for _, it := range items {
		switch v := it.(type) {
		case int:
			runtime.Gosched()
			tsAcc += v
		case string:
			runtime.Gosched()
			tsAcc += len(v)
		case nil:
			runtime.Gosched()
			tsAcc += 100
		}
	}
	if tsAcc != 107 {
		panic(fmt.Sprintf("type switch %d", tsAcc))
	}

	// Short-circuit && and ||
	cnt := 0
	if yieldFalse(&cnt) && yieldTrue(&cnt) {
		panic("short-circuit && failed")
	}
	if cnt != 1 {
		panic(fmt.Sprintf("&& evaluated rhs: cnt=%d", cnt))
	}
	if !(yieldTrue(&cnt) || yieldFalse(&cnt)) {
		panic("short-circuit || failed")
	}
	if cnt != 2 {
		panic(fmt.Sprintf("|| evaluated rhs: cnt=%d", cnt))
	}

	// Goto across suspension
	gStep := 0
	goto mid
end:
	if gStep != 2 {
		panic("goto failed")
	}
	return
mid:
	runtime.Gosched()
	gStep = 2
	goto end
}

// 7. Defer (LIFO, arg eval order, named return mutation, suspending defer), Panic (explicit + runtime + cross-await), Recover, Goexit
func deferNamedRet() (res int) {
	x := 10
	defer func(snap int) {
		res += snap
	}(x)
	x = 20
	defer func() {
		res += x
	}()
	runtime.Gosched()
	res = 1
	return
}

func panicChild() {
	runtime.Gosched()
	panic("child-boom")
}

func recoverCrossAwait() (got any) {
	defer func() {
		got = recover()
	}()
	panicChild()
	return "unreachable"
}

func recoverRuntimePanic() (got any) {
	defer func() {
		got = recover()
	}()
	runtime.Gosched()
	s := []int{1, 2}
	_ = s[5]
	return nil
}

func suspendingDeferRecover(out chan any) {
	defer func() {
		r := recover()
		runtime.Gosched()
		out <- r
	}()
	runtime.Gosched()
	panic("susp-defer")
}

func testDeferPanicRecoverGoexit() {
	if got := deferNamedRet(); got != 31 {
		panic(fmt.Sprintf("deferNamedRet=%d want 31", got))
	}
	if got := recoverCrossAwait(); got != "child-boom" {
		panic(fmt.Sprintf("recoverCrossAwait=%v", got))
	}
	if got := recoverRuntimePanic(); got == nil {
		panic("recoverRuntimePanic missed slice bounds panic")
	}
	ch := make(chan any, 1)
	suspendingDeferRecover(ch)
	if got := <-ch; got != "susp-defer" {
		panic(fmt.Sprintf("suspendingDeferRecover=%v", got))
	}

	// runtime.Goexit runs defers without crashing
	goexitDone := make(chan string, 1)
	go func() {
		defer func() {
			goexitDone <- "clean-goexit"
		}()
		runtime.Gosched()
		runtime.Goexit()
		goexitDone <- "unreachable"
	}()
	if got := <-goexitDone; got != "clean-goexit" {
		panic(fmt.Sprintf("goexit=%q", got))
	}
}

func main() {
	for _, p := range []int{1, 2, 4} {
		runtime.GOMAXPROCS(p)
		testChannels()
		testSelect()
		testDynamicDispatch()
		testClosuresAndRecursion()
		testControlFlow()
		testDeferPanicRecoverGoexit()

		r1, w1, err := os.Pipe()
		if err != nil {
			panic(err)
		}
		r2, w2, err := os.Pipe()
		if err != nil {
			panic(err)
		}
		if err := runPipeEcho(r1, w1, r2, w2); err != nil {
			panic(err)
		}
		r1.Close()
		w1.Close()
		r2.Close()
		w2.Close()

		ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			panic(err)
		}
		sch := make(chan *net.TCPConn, 1)
		go func() {
			s, _ := ln.AcceptTCP()
			sch <- s
		}()
		c, err := net.DialTCP("tcp4", nil, ln.Addr().(*net.TCPAddr))
		if err != nil {
			panic(err)
		}
		s := <-sch
		if err := runTCPEcho(c, s); err != nil {
			panic(err)
		}
		c.Close()
		s.Close()
		ln.Close()
	}
	fmt.Println("CORO_V2_E2E_OK")
}
`

func TestCoroV2EndToEnd(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(e2eProgram), 0644); err != nil {
		t.Fatal(err)
	}
	goBin := testenv.GoToolPath(t)

	runMode := func(name, goexp string, extra []string) {
		t.Run(name, func(t *testing.T) {
			bin := filepath.Join(dir, "app_"+name)
			args := append([]string{"build"}, extra...)
			args = append(args, "-o", bin, src)
			cmd := exec.Command(goBin, args...)
			cmd.Env = append(os.Environ(), "GOEXPERIMENT="+goexp)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build failed: %v\n%s", err, out)
			}
			if goexp == "coro" && !strings.Contains(string(out), "coro: phase=lower") {
				t.Fatalf("expected lowering diagnostics in build output:\n%s", out)
			}
			run := exec.Command(bin)
			runOut, err := run.CombinedOutput()
			if err != nil {
				t.Fatalf("run failed: %v\n%s", err, runOut)
			}
			if strings.TrimSpace(string(runOut)) != "CORO_V2_E2E_OK" {
				t.Fatalf("unexpected output: %q", runOut)
			}
		})
	}

	runMode("coro_noinline", "coro", []string{"-gcflags=-l -d=coro=4"})
	runMode("coro_inline", "coro", []string{"-gcflags=-d=coro=4"})
	runMode("coro_staticlockranking", "coro,staticlockranking", []string{"-gcflags=-d=coro=4"})
	runMode("coro_race", "coro", []string{"-race", "-gcflags=-d=coro=4"})
	runMode("nocoro", "nocoro", nil)
}
