// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"unsafe"
)

func BenchmarkStacklessCoroEntry(b *testing.B) {
	resume := func(unsafe.Pointer) uint8 {
		return runtime.StacklessCoroActionComplete
	}
	b.ReportAllocs()
	for range b.N {
		runtime.RunStacklessCoroForTest(resume)
	}
}

func BenchmarkStacklessCoroYield(b *testing.B) {
	iterations := 0
	b.ReportAllocs()
	b.ResetTimer()
	runtime.RunStacklessCoroForTest(func(unsafe.Pointer) uint8 {
		if iterations == b.N {
			return runtime.StacklessCoroActionComplete
		}
		iterations++
		return runtime.StacklessCoroActionYield
	})
}

func BenchmarkStacklessCoroSpawn(b *testing.B) {
	spawned := 0
	var completed atomic.Int64
	child := func(unsafe.Pointer) uint8 {
		completed.Add(1)
		return runtime.StacklessCoroActionComplete
	}
	b.ReportAllocs()
	b.ResetTimer()
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if spawned == b.N {
			if completed.Load() != int64(spawned) {
				return runtime.StacklessCoroActionYield
			}
			return runtime.StacklessCoroActionComplete
		}
		spawned++
		runtime.SpawnStacklessCoroForTest(ctx, child)
		return runtime.StacklessCoroActionYield
	})
}

func BenchmarkStacklessCoroSpawnBurst(b *testing.B) {
	b.Run("1", func(b *testing.B) {
		benchmarkStacklessCoroSpawnBurst(b, 1)
	})
	b.Run("1000", func(b *testing.B) {
		benchmarkStacklessCoroSpawnBurst(b, 1000)
	})
	b.Run("100000", func(b *testing.B) {
		benchmarkStacklessCoroSpawnBurst(b, 100000)
	})
}

func benchmarkStacklessCoroSpawnBurst(b *testing.B, tasks int) {
	b.ReportAllocs()
	for range b.N {
		var completed atomic.Int64
		state := 0
		child := func(unsafe.Pointer) uint8 {
			completed.Add(1)
			return runtime.StacklessCoroActionComplete
		}
		runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
			switch state {
			case 0:
				for range tasks {
					runtime.SpawnStacklessCoroForTest(ctx, child)
				}
				state = 1
				return runtime.StacklessCoroActionYield
			case 1:
				if completed.Load() != int64(tasks) {
					return runtime.StacklessCoroActionYield
				}
				return runtime.StacklessCoroActionComplete
			default:
				return runtime.StacklessCoroActionInvalid
			}
		})
	}
}

func BenchmarkStacklessCoroAwait(b *testing.B) {
	completed := 0
	child := func(unsafe.Pointer) uint8 {
		completed++
		return runtime.StacklessCoroActionComplete
	}
	b.ReportAllocs()
	b.ResetTimer()
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if completed == b.N {
			return runtime.StacklessCoroActionComplete
		}
		runtime.AwaitStacklessCoroForTest(ctx, child)
		return runtime.StacklessCoroActionWait
	})
}

func BenchmarkStacklessCoroTimer(b *testing.B) {
	completed := 0
	pending := false
	b.ReportAllocs()
	b.ResetTimer()
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if pending {
			completed++
			pending = false
		}
		if completed == b.N {
			return runtime.StacklessCoroActionComplete
		}
		pending = true
		if !runtime.SleepStacklessCoroForTest(ctx, 1) {
			b.Fatal("positive sleep did not start a timer")
		}
		return runtime.StacklessCoroActionWait
	})
}

func BenchmarkStacklessCoroChannel(b *testing.B) {
	channel := make(chan int)
	value := 1
	var received atomic.Int64
	receiverPending := false
	receiverValue := 0
	receiverOK := false
	receiver := func(ctx unsafe.Pointer) uint8 {
		if receiverPending {
			if receiverValue != value || !receiverOK {
				b.Fatalf("receive = (%d, %t), want (%d, true)",
					receiverValue, receiverOK, value)
			}
			received.Add(1)
			receiverPending = false
		}
		if received.Load() == int64(b.N) {
			return runtime.StacklessCoroActionComplete
		}
		receiverPending = true
		runtime.RecvIntStacklessCoroForTest(ctx, channel, &receiverValue,
			&receiverOK)
		return runtime.StacklessCoroActionWait
	}

	sent := 0
	sendPending := false
	state := 0
	b.ReportAllocs()
	b.ResetTimer()
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		switch state {
		case 0:
			runtime.SpawnStacklessCoroForTest(ctx, receiver)
			state = 1
			return runtime.StacklessCoroActionYield
		case 1:
			if sendPending {
				sent++
				sendPending = false
			}
			if sent == b.N {
				if received.Load() != int64(b.N) {
					return runtime.StacklessCoroActionYield
				}
				return runtime.StacklessCoroActionComplete
			}
			sendPending = true
			runtime.SendIntStacklessCoroForTest(ctx, channel, &value)
			return runtime.StacklessCoroActionWait
		default:
			return runtime.StacklessCoroActionInvalid
		}
	})
}

func BenchmarkStacklessCoroFileRead(b *testing.B) {
	fd, err := syscall.Open("/dev/zero", syscall.O_RDONLY, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer syscall.Close(fd)

	buffer := make([]byte, 1)
	completed := 0
	pending := false
	var n int
	var errno uintptr
	b.ReportAllocs()
	b.ResetTimer()
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if pending {
			if n != 1 || errno != 0 {
				b.Fatalf("read = (%d, %d), want (1, 0)", n, errno)
			}
			completed++
			pending = false
		}
		if completed == b.N {
			return runtime.StacklessCoroActionComplete
		}
		n = -1
		errno = ^uintptr(0)
		pending = true
		runtime.FileReadStacklessCoroForTest(ctx, fd, buffer, &n, &errno)
		return runtime.StacklessCoroActionWait
	})
}

func BenchmarkStacklessCoroSocketRead(b *testing.B) {
	benchmarkStacklessCoroSocketRead(b, false)
}

func BenchmarkStacklessCoroSocketReadWait(b *testing.B) {
	benchmarkStacklessCoroSocketRead(b, true)
}

func BenchmarkStacklessCoroOrdinaryNetpollReady(b *testing.B) {
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	b.ReportAllocs()
	b.ResetTimer()
	runtime.StacklessCoroOrdinaryNetpollReadyForTest(fds[0], b.N)
}

func benchmarkStacklessCoroSocketRead(b *testing.B, wait bool) {
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		b.Fatal(err)
	}

	ready := make(chan struct{}, 1)
	writeDone := make(chan error, 1)
	go func() {
		var err error
		for range b.N {
			if wait {
				<-ready
			}
			_, err = syscall.Write(fds[1], []byte{1})
			if err != nil {
				break
			}
		}
		writeDone <- err
	}()

	buffer := make([]byte, 1)
	completed := 0
	pending := false
	var n int
	var errno uintptr
	b.ReportAllocs()
	b.ResetTimer()
	runtime.RunStacklessCoroForTest(func(ctx unsafe.Pointer) uint8 {
		if pending {
			if n != 1 || errno != 0 {
				b.Fatalf("read = (%d, %d), want (1, 0)", n, errno)
			}
			completed++
			pending = false
		}
		if completed == b.N {
			return runtime.StacklessCoroActionComplete
		}
		n = -1
		errno = ^uintptr(0)
		pending = true
		runtime.SocketReadStacklessCoroForTest(ctx, fds[0], buffer, &n, &errno)
		if wait {
			ready <- struct{}{}
		}
		return runtime.StacklessCoroActionWait
	})
	b.StopTimer()
	if err := <-writeDone; err != nil {
		b.Fatal(err)
	}
}
