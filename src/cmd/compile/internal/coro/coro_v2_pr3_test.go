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

const pr3Program = `package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 1. 3-level awaited coroutine panic chain (leaf -> mid -> root)
func leaf(msg string) {
	runtime.Gosched()
	panic(msg)
}

func mid(msg string) {
	runtime.Gosched()
	leaf(msg)
}

func root(msg string) {
	runtime.Gosched()
	mid(msg)
}

// 2. Parked stackless coroutines visible in GoroutineProfile, pprof, and runtime.Stack
func parkChild(gate <-chan struct{}, ready *atomic.Int32) {
	ready.Add(1)
	<-gate
}

func parkParent(gate <-chan struct{}, ready *atomic.Int32, wg *sync.WaitGroup) {
	defer wg.Done()
	parkChild(gate, ready)
}

func testObservability() {
	// 2a. Outer ordinary recover receives exact unwrapped panic payload.
	var caught any
	func() {
		defer func() { caught = recover() }()
		root("exact-payload")
	}()
	if caught != "exact-payload" {
		panic(fmt.Sprintf("recovered=%v want exact-payload", caught))
	}

	// 2b. Park 24 awaited coroutines on a channel + 8 on a sync.Mutex.
	const nChan = 24
	const nMu = 8
	gate := make(chan struct{})
	var ready atomic.Int32
	var wg sync.WaitGroup
	var mu sync.Mutex
	mu.Lock()

	wg.Add(nChan + nMu)
	for i := 0; i < nChan; i++ {
		go parkParent(gate, &ready, &wg)
	}
	for i := 0; i < nMu; i++ {
		go func() {
			defer wg.Done()
			ready.Add(1)
			mu.Lock()
			mu.Unlock()
		}()
	}

	deadline := time.Now().Add(5 * time.Second)
	for ready.Load() < nChan+nMu && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	// Give parked tasks a brief moment to complete transition into parked state.
	time.Sleep(5 * time.Millisecond)

	if ng := runtime.NumGoroutine(); ng < nChan+nMu {
		panic(fmt.Sprintf("NumGoroutine=%d want >=%d", ng, nChan+nMu))
	}

	// Check runtime.GoroutineProfile contains parkChild & parkParent.
	var prof []runtime.StackRecord
	n, ok := runtime.GoroutineProfile(nil)
	for !ok {
		prof = make([]runtime.StackRecord, n+16)
		n, ok = runtime.GoroutineProfile(prof)
	}
	prof = prof[:n]
	if n < nChan+nMu {
		panic(fmt.Sprintf("GoroutineProfile count=%d want >=%d", n, nChan+nMu))
	}
	foundChild, foundParent := 0, 0
	for _, r := range prof {
		frames := runtime.CallersFrames(r.Stack())
		hasC, hasP := false, false
		for {
			f, more := frames.Next()
			if strings.Contains(f.Function, "main.parkChild") {
				hasC = true
			}
			if strings.Contains(f.Function, "main.parkParent") {
				hasP = true
			}
			if !more {
				break
			}
		}
		if hasC {
			foundChild++
		}
		if hasP {
			foundParent++
		}
	}
	if foundChild < nChan/2 || foundParent < nChan/2 {
		panic(fmt.Sprintf("GoroutineProfile foundChild=%d foundParent=%d want >=%d", foundChild, foundParent, nChan/2))
	}

	// Check pprof.Lookup("goroutine") text format includes parked coroutines.
	var pprofBuf bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&pprofBuf, 1); err != nil {
		panic(err)
	}
	pprofStr := pprofBuf.String()
	if !strings.Contains(pprofStr, "main.parkChild") || !strings.Contains(pprofStr, "main.parkParent") {
		panic(fmt.Sprintf("pprof goroutine profile missing parkChild/parkParent:\n%s", pprofStr))
	}

	// Check runtime.Stack(buf, true) includes parked coroutines.
	stackBuf := make([]byte, 256<<10)
	sn := runtime.Stack(stackBuf, true)
	stackStr := string(stackBuf[:sn])
	if !strings.Contains(stackStr, "main.parkChild") || !strings.Contains(stackStr, "main.parkParent") {
		panic(fmt.Sprintf("runtime.Stack missing parkChild/parkParent:\n%s", stackStr))
	}

	// Unblock and drain cleanly.
	close(gate)
	mu.Unlock()
	wg.Wait()
	runtime.GC()
	runtime.GC()
}

// 3. Real-world framed TCP KV server combining Accept, net.Conn, io.Reader/Writer, RWMutex, WaitGroup, generics
type kvStore struct {
	mu   sync.RWMutex
	data map[uint32]uint32
}

func (s *kvStore) put(k, v uint32) {
	s.mu.Lock()
	s.data[k] = v
	s.mu.Unlock()
}

func (s *kvStore) get(k uint32) uint32 {
	s.mu.RLock()
	v := s.data[k]
	s.mu.RUnlock()
	return v
}

func readFrame(r io.Reader, buf []byte) error {
	for off := 0; off < len(buf); {
		n, err := r.Read(buf[off:])
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

func writeFrame(w io.Writer, buf []byte) error {
	for off := 0; off < len(buf); {
		n, err := w.Write(buf[off:])
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

func serveKVConn(c net.Conn, store *kvStore, wg *sync.WaitGroup) {
	defer wg.Done()
	defer c.Close()
	var req [12]byte
	var resp [4]byte
	for {
		if err := readFrame(c, req[:]); err != nil {
			if err == io.EOF {
				return
			}
			panic(fmt.Sprintf("serveKVConn read: %v", err))
		}
		op := binary.LittleEndian.Uint32(req[0:4])
		k := binary.LittleEndian.Uint32(req[4:8])
		v := binary.LittleEndian.Uint32(req[8:12])
		var out uint32
		if op == 1 {
			store.put(k, v)
			out = v
		} else {
			out = store.get(k)
		}
		binary.LittleEndian.PutUint32(resp[:], out)
		if err := writeFrame(c, resp[:]); err != nil {
			panic(fmt.Sprintf("serveKVConn write: %v", err))
		}
	}
}

func acceptKVServer(ln net.Listener, count int, store *kvStore, done *sync.WaitGroup) {
	defer done.Done()
	var cwg sync.WaitGroup
	for i := 0; i < count; i++ {
		c, err := ln.Accept()
		if err != nil {
			panic(fmt.Sprintf("Accept(%d): %v", i, err))
		}
		cwg.Add(1)
		go serveKVConn(c, store, &cwg)
	}
	cwg.Wait()
}

func testRealWorldKVServer() {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer ln.Close()
	store := &kvStore{data: make(map[uint32]uint32)}
	const clients = 8
	var srvWG sync.WaitGroup
	srvWG.Add(1)
	go acceptKVServer(ln, clients, store, &srvWG)

	var cliWG sync.WaitGroup
	for i := 0; i < clients; i++ {
		c, err := net.Dial("tcp4", ln.Addr().String())
		if err != nil {
			panic(err)
		}
		cliWG.Add(1)
		go func(conn net.Conn, id uint32) {
			defer cliWG.Done()
			defer conn.Close()
			var req [12]byte
			var resp [4]byte
			for j := uint32(1); j <= 5; j++ {
				key := id*100 + j
				val := key * 7
				// PUT
				binary.LittleEndian.PutUint32(req[0:4], 1)
				binary.LittleEndian.PutUint32(req[4:8], key)
				binary.LittleEndian.PutUint32(req[8:12], val)
				if err := writeFrame(conn, req[:]); err != nil {
					panic(err)
				}
				if err := readFrame(conn, resp[:]); err != nil {
					panic(err)
				}
				if got := binary.LittleEndian.Uint32(resp[:]); got != val {
					panic(fmt.Sprintf("PUT resp=%d want %d", got, val))
				}
				// GET
				binary.LittleEndian.PutUint32(req[0:4], 2)
				binary.LittleEndian.PutUint32(req[4:8], key)
				binary.LittleEndian.PutUint32(req[8:12], 0)
				if err := writeFrame(conn, req[:]); err != nil {
					panic(err)
				}
				if err := readFrame(conn, resp[:]); err != nil {
					panic(err)
				}
				if got := binary.LittleEndian.Uint32(resp[:]); got != val {
					panic(fmt.Sprintf("GET resp=%d want %d", got, val))
				}
			}
		}(c, uint32(i+1))
	}
	cliWG.Wait()
	srvWG.Wait()
}

func main() {
	mode := "normal"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "crash-root":
		root("leaf-crash-root")
	case "crash-go":
		done := make(chan struct{})
		go func() {
			root("leaf-crash-go")
			close(done)
		}()
		<-done
	default:
		for _, p := range []int{1, 2, 4} {
			runtime.GOMAXPROCS(p)
			testObservability()
			testRealWorldKVServer()
		}
		fmt.Println("CORO_V2_PR3_OK")
	}
}
`

func TestCoroV2ObservabilityAndRealWorldServer(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(pr3Program), 0644); err != nil {
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
				t.Fatalf("expected lowering diagnostics:\n%s", out)
			}

			// 1. Normal mode: observability (GoroutineProfile, pprof, Stack) + real-world KV server.
			run := exec.Command(bin, "normal")
			runOut, err := run.CombinedOutput()
			if err != nil {
				t.Fatalf("normal run failed: %v\n%s", err, runOut)
			}
			if strings.TrimSpace(string(runOut)) != "CORO_V2_PR3_OK" {
				t.Fatalf("unexpected output: %q", runOut)
			}

			// 2. Unrecovered panic from synchronous root->mid->leaf preserves full logical call chain.
			c1 := exec.Command(bin, "crash-root")
			c1Out, err := c1.CombinedOutput()
			if err == nil {
				t.Fatalf("expected crash-root to exit non-zero")
			}
			s1 := string(c1Out)
			if !strings.Contains(s1, "panic: leaf-crash-root") {
				t.Fatalf("crash-root missing panic msg:\n%s", s1)
			}
			if !strings.Contains(s1, "main.leaf") || !strings.Contains(s1, "main.mid") || !strings.Contains(s1, "main.root") {
				t.Fatalf("crash-root missing logical chain leaf/mid/root:\n%s", s1)
			}
			if goexp == "coro" && !strings.Contains(s1, "coroutine logical stack:") {
				t.Fatalf("crash-root missing coroutine logical stack header:\n%s", s1)
			}

			// 3. Unrecovered panic inside independent `go` coroutine preserves full logical call chain.
			c2 := exec.Command(bin, "crash-go")
			c2Out, err := c2.CombinedOutput()
			if err == nil {
				t.Fatalf("expected crash-go to exit non-zero")
			}
			s2 := string(c2Out)
			if !strings.Contains(s2, "panic: leaf-crash-go") {
				t.Fatalf("crash-go missing panic msg:\n%s", s2)
			}
			if !strings.Contains(s2, "main.leaf") || !strings.Contains(s2, "main.mid") || !strings.Contains(s2, "main.root") {
				t.Fatalf("crash-go missing logical chain leaf/mid/root:\n%s", s2)
			}
			if goexp == "coro" && !strings.Contains(s2, "coroutine logical stack:") {
				t.Fatalf("crash-go missing coroutine logical stack header:\n%s", s2)
			}
		})
	}

	runMode("coro_noinline", "coro", []string{"-gcflags=-l -d=coro=4"})
	runMode("coro_inline", "coro", []string{"-gcflags=-d=coro=4"})
	runMode("coro_staticlockranking", "coro,staticlockranking", []string{"-gcflags=-d=coro=4"})
	runMode("coro_race", "coro", []string{"-race", "-gcflags=-d=coro=4"})
	runMode("nocoro", "nocoro", nil)
}
