// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// TestStacklessCoroMultiPInterleavedIOGCShrink exercises the non-race multi-P
// fast path (per-P op caches, per-P runq, atomic wait/ready transitions) while
// concurrently interleaving real netpoll I/O completions, STW GC cycles
// (clearStacklessCoroPStates), and GOMAXPROCS shrink/expand transitions
// (p.destroy -> drainStacklessCoroPState), verifying zero lost tasks, zero
// duplicate executions, and bounded completion without deadlock.
func TestStacklessCoroMultiPInterleavedIOGCShrink(t *testing.T) {
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)

	const (
		numPipes      = 16
		roundsPerPipe = 40
	)
	type pipePair struct {
		rfd int
		wfd int
	}
	pipes := make([]pipePair, numPipes)
	for i := range numPipes {
		var fds [2]int
		if err := syscall.Pipe(fds[:]); err != nil {
			t.Fatalf("Pipe %d failed: %v", i, err)
		}
		if err := syscall.SetNonblock(fds[0], true); err != nil {
			syscall.Close(fds[0])
			syscall.Close(fds[1])
			t.Fatalf("SetNonblock %d failed: %v", i, err)
		}
		pipes[i] = pipePair{rfd: fds[0], wfd: fds[1]}
	}
	defer func() {
		for _, p := range pipes {
			syscall.Close(p.rfd)
			syscall.Close(p.wfd)
		}
	}()

	var execCount [numPipes][roundsPerPipe]atomic.Int32
	ackChans := make([]chan int, numPipes)
	for i := range numPipes {
		ackChans[i] = make(chan int, 1)
	}
	allDone := make(chan int, numPipes)
	var errMsg atomic.Pointer[string]
	recordErr := func(msg string) {
		m := msg
		errMsg.CompareAndSwap(nil, &m)
	}

	// Start concurrent chaos loop interleaving GC and GOMAXPROCS 4->1->4->2->1->4.
	var stopChaos atomic.Bool
	var gcCycles atomic.Int32
	var shrinkCycles atomic.Int32
	var chaosWG sync.WaitGroup
	chaosWG.Add(1)
	go func() {
		defer chaosWG.Done()
		seq := []int{4, 1, 4, 2, 1, 4}
		step := 0
		for !stopChaos.Load() {
			nextP := seq[step%len(seq)]
			prevP := runtime.GOMAXPROCS(nextP)
			if nextP < prevP {
				shrinkCycles.Add(1)
			}
			runtime.GC()
			gcCycles.Add(1)
			step++
			time.Sleep(500 * time.Microsecond)
		}
	}()

	// External writers feed each pipe one byte per round after waiting for
	// the coroutine's previous round ack, so reads genuinely block in netpoll
	// and complete asynchronously on arbitrary Ps.
	var writerWG sync.WaitGroup
	for i := range numPipes {
		idx := i
		wfd := pipes[idx].wfd
		ack := ackChans[idx]
		writerWG.Add(1)
		go func() {
			defer writerWG.Done()
			var b [1]byte
			for r := range roundsPerPipe {
				b[0] = byte(idx ^ r)
				for {
					n, err := syscall.Write(wfd, b[:])
					if err == syscall.EINTR {
						continue
					}
					if err != nil || n != 1 {
						recordErr(fmt.Sprintf("pipe %d round %d write failed: n=%d err=%v", idx, r, n, err))
						return
					}
					break
				}
				gotRound := <-ack
				if gotRound != r {
					recordErr(fmt.Sprintf("pipe %d ack mismatch: got %d want %d", idx, gotRound, r))
					return
				}
			}
		}()
	}

	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		rootState := 0
		collected := 0
		var recvVal int
		var recvOk bool
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if rootState == 0 {
				rootState = 1
				for i := range numPipes {
					idx := i
					rfd := pipes[idx].rfd
					ack := ackChans[idx]
					var buf [1]byte
					var n int
					var errno uintptr
					round := 0
					phase := 0 // 0=start read, 1=check read+start ack send, 2=next round
					sendRound := 0
					runtime.SpawnStacklessCoroForTest(ctx, func(wctx unsafe.Pointer) uint8 {
						for {
							switch phase {
							case 0:
								if round >= roundsPerPipe {
									sendRound = idx
									phase = 3
									if runtime.SendIntStacklessCoroForTest(wctx, allDone, &sendRound) {
										return runtime.StacklessCoroActionWait
									}
									return runtime.StacklessCoroActionComplete
								}
								phase = 1
								runtime.SocketReadStacklessCoroForTest(wctx, rfd, buf[:], &n, &errno)
								return runtime.StacklessCoroActionWait
							case 1:
								if errno != 0 || n != 1 || buf[0] != byte(idx^round) {
									recordErr(fmt.Sprintf("pipe %d round %d read = (%d,%d,%d) want (1,0,%d)",
										idx, round, n, errno, buf[0], byte(idx^round)))
								}
								if c := execCount[idx][round].Add(1); c != 1 {
									recordErr(fmt.Sprintf("pipe %d round %d executed %d times", idx, round, c))
								}
								sendRound = round
								phase = 2
								if runtime.SendIntStacklessCoroForTest(wctx, ack, &sendRound) {
									return runtime.StacklessCoroActionWait
								}
							case 2:
								round++
								phase = 0
							case 3:
								return runtime.StacklessCoroActionComplete
							}
						}
					})
				}
			}
			for collected < numPipes {
				if rootState == 1 {
					rootState = 2
					if runtime.RecvIntStacklessCoroForTest(ctx, allDone, &recvVal, &recvOk) {
						return runtime.StacklessCoroActionWait
					}
				}
				collected++
				rootState = 1
			}
			return runtime.StacklessCoroActionComplete
		})
	}()

	select {
	case <-schedDone:
	case <-time.After(15 * time.Second):
		stopChaos.Store(true)
		t.Fatal("timed out waiting for interleaved multi-P I/O + GC + GOMAXPROCS shrink scheduler")
	}

	stopChaos.Store(true)
	chaosWG.Wait()
	writerWG.Wait()

	if msg := errMsg.Load(); msg != nil {
		t.Fatal(*msg)
	}
	for i := range numPipes {
		for r := range roundsPerPipe {
			if got := execCount[i][r].Load(); got != 1 {
				t.Fatalf("pipe %d round %d execution count = %d, want 1", i, r, got)
			}
		}
	}
	if gcCycles.Load() == 0 || shrinkCycles.Load() == 0 {
		t.Fatalf("chaos loop did not interleave GC (%d) and GOMAXPROCS shrinks (%d)",
			gcCycles.Load(), shrinkCycles.Load())
	}
}
