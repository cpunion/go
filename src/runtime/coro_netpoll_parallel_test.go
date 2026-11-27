// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"internal/testenv"
	"os"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestStacklessCoroNetpollParallelDrain(t *testing.T) {
	const child = "GO_TEST_CORO_NETPOLL_PARALLEL"
	if os.Getenv(child) != "1" {
		cmd := testenv.CleanCmdEnv(testenv.Command(t, testenv.Executable(t),
			"-test.run=^TestStacklessCoroNetpollParallelDrain$"))
		cmd.Env = append(cmd.Env, child+"=1")
		if debug := os.Getenv("GODEBUG"); debug != "" {
			cmd.Env = append(cmd.Env, "GODEBUG="+debug)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("netpoll parallel drain subprocess: %v\n%s", err, out)
		}
		return
	}
	if msg := runtime.CheckStacklessCoroNetpollPopOneForTest(); msg != "" {
		t.Fatal(msg)
	}
}

func TestStacklessCoroNetpollFanoutWake(t *testing.T) {
	const child = "GO_TEST_CORO_NETPOLL_FANOUT"
	if os.Getenv(child) != "1" {
		cmd := testenv.CleanCmdEnv(testenv.Command(t, testenv.Executable(t),
			"-test.run=^TestStacklessCoroNetpollFanoutWake$"))
		cmd.Env = append(cmd.Env, child+"=1", "GOMAXPROCS=4")
		if debug := os.Getenv("GODEBUG"); debug != "" {
			cmd.Env = append(cmd.Env, "GODEBUG="+debug)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("netpoll fanout subprocess: %v\n%s", err, out)
		}
		return
	}
	if msg := runtime.CheckStacklessCoroNetpollFanoutWakeForTest(); msg != "" {
		t.Fatal(msg)
	}
}

func TestStacklessCoroNetpollDualSchedulerFanout(t *testing.T) {
	const child = "GO_TEST_CORO_NETPOLL_DUAL_FANOUT"
	if os.Getenv(child) != "1" {
		cmd := testenv.CleanCmdEnv(testenv.Command(t, testenv.Executable(t),
			"-test.run=^TestStacklessCoroNetpollDualSchedulerFanout$"))
		cmd.Env = append(cmd.Env, child+"=1", "GOMAXPROCS=4")
		if debug := os.Getenv("GODEBUG"); debug != "" {
			cmd.Env = append(cmd.Env, "GODEBUG="+debug)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("dual-scheduler netpoll fanout subprocess: %v\n%s", err, out)
		}
		return
	}
	if msg := runtime.CheckStacklessCoroNetpollDualSchedulerFanoutForTest(); msg != "" {
		t.Fatal(msg)
	}
}

func TestStacklessCoroOperationRegistryShards(t *testing.T) {
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)
	if msg := runtime.CheckStacklessCoroOperationRegistryShardsForTest(8, 256); msg != "" {
		t.Fatal(msg)
	}
}

// TestStacklessCoroNetpollDualSchedulerRealIO verifies that two independent
// coroutine schedulers sharing the global netpoll ready queue (a single-task
// scheduler with zero idle replacement executors and an 8-task multi-executor
// scheduler) complete interleaved bursts of real pipe I/O cleanly under P=4.
func TestStacklessCoroNetpollDualSchedulerRealIO(t *testing.T) {
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)

	const (
		s2Conns = 8
		bursts  = 40
	)
	type fdPair struct{ r, w int }
	makePipe := func() fdPair {
		var fds [2]int
		if err := syscall.Pipe(fds[:]); err != nil {
			t.Fatalf("Pipe failed: %v", err)
		}
		if err := syscall.SetNonblock(fds[0], true); err != nil {
			syscall.Close(fds[0])
			syscall.Close(fds[1])
			t.Fatalf("SetNonblock failed: %v", err)
		}
		return fdPair{r: fds[0], w: fds[1]}
	}
	s1Pipe := makePipe()
	defer syscall.Close(s1Pipe.r)
	defer syscall.Close(s1Pipe.w)

	s2Pipes := make([]fdPair, s2Conns)
	for i := range s2Conns {
		s2Pipes[i] = makePipe()
		p := s2Pipes[i]
		defer syscall.Close(p.r)
		defer syscall.Close(p.w)
	}

	s1Ack := make(chan byte, 1)
	s2Ack := make([]chan byte, s2Conns)
	for i := range s2Conns {
		s2Ack[i] = make(chan byte, 1)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	// Scheduler 1: single coroutine (executorCount=1, waitingExecutors=0).
	go func() {
		defer wg.Done()
		var buf [1]byte
		var n int
		var errno uintptr
		round := 0
		phase := 0
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			for {
				if phase == 0 {
					if round >= bursts {
						return runtime.StacklessCoroActionComplete
					}
					phase = 1
					runtime.SocketReadStacklessCoroForTest(ctx, s1Pipe.r, buf[:], &n, &errno)
					return runtime.StacklessCoroActionWait
				}
				if errno != 0 || n != 1 {
					t.Errorf("s1 round %d read = (%d,%d)", round, n, errno)
					return runtime.StacklessCoroActionComplete
				}
				s1Ack <- buf[0]
				round++
				phase = 0
			}
		})
	}()

	// Scheduler 2: 8 concurrent coroutines with idle multi-P executors.
	go func() {
		defer wg.Done()
		doneCh := make(chan int, s2Conns)
		rootState := 0
		collected := 0
		var val int
		var ok bool
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			if rootState == 0 {
				rootState = 1
				for i := range s2Conns {
					idx := i
					rfd := s2Pipes[idx].r
					ack := s2Ack[idx]
					var buf [1]byte
					var n int
					var errno uintptr
					round := 0
					phase := 0
					sendID := idx
					runtime.SpawnStacklessCoroForTest(ctx, func(wctx unsafe.Pointer) uint8 {
						for {
							switch phase {
							case 0:
								if round >= bursts {
									phase = 2
									if runtime.SendIntStacklessCoroForTest(wctx, doneCh, &sendID) {
										return runtime.StacklessCoroActionWait
									}
									return runtime.StacklessCoroActionComplete
								}
								phase = 1
								runtime.SocketReadStacklessCoroForTest(wctx, rfd, buf[:], &n, &errno)
								return runtime.StacklessCoroActionWait
							case 1:
								if errno != 0 || n != 1 {
									t.Errorf("s2[%d] round %d read = (%d,%d)", idx, round, n, errno)
									return runtime.StacklessCoroActionComplete
								}
								ack <- buf[0]
								round++
								phase = 0
							case 2:
								return runtime.StacklessCoroActionComplete
							}
						}
					})
				}
			}
			for collected < s2Conns {
				if rootState == 1 {
					rootState = 2
					if runtime.RecvIntStacklessCoroForTest(ctx, doneCh, &val, &ok) {
						return runtime.StacklessCoroActionWait
					}
				}
				collected++
				rootState = 1
			}
			return runtime.StacklessCoroActionComplete
		})
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		var b [1]byte
		for r := range bursts {
			b[0] = byte(r + 1)
			if _, err := syscall.Write(s1Pipe.w, b[:]); err != nil {
				t.Errorf("s1 write round %d: %v", r, err)
				return
			}
			for i := range s2Conns {
				b[0] = byte(r ^ i)
				if _, err := syscall.Write(s2Pipes[i].w, b[:]); err != nil {
					t.Errorf("s2[%d] write round %d: %v", i, r, err)
					return
				}
			}
			if got := <-s1Ack; got != byte(r+1) {
				t.Errorf("s1 ack round %d = %d, want %d", r, got, byte(r+1))
				return
			}
			for i := range s2Conns {
				if got := <-s2Ack[i]; got != byte(r^i) {
					t.Errorf("s2[%d] ack round %d = %d, want %d", i, r, got, byte(r^i))
					return
				}
			}
		}
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for dual-scheduler real netpoll I/O")
	}
}
