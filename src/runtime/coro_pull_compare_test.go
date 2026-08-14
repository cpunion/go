// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

type stacklessCoroComparisonDriver struct {
	name string
	run  func(unsafe.Pointer, func(unsafe.Pointer) uint8)
}

var stacklessCoroComparisonDrivers = [...]stacklessCoroComparisonDriver{
	{
		name: "Push",
		run:  runtime.RunStacklessCoroPushComparisonFrameForTest,
	},
	{
		name: "Pull",
		run:  runtime.RunStacklessCoroPullFrameForTest,
	},
}

type stacklessCoroComparisonYieldFrame struct {
	remaining int
	completed int
}

func stacklessCoroComparisonYieldResume(ctx unsafe.Pointer) uint8 {
	frame := (*stacklessCoroComparisonYieldFrame)(
		runtime.FrameStacklessCoroForTest(ctx))
	if frame.remaining == 0 {
		return runtime.StacklessCoroActionComplete
	}
	frame.remaining--
	frame.completed++
	return runtime.StacklessCoroActionYield
}

type stacklessCoroComparisonAwaitFrame struct {
	depth  int
	state  uint8
	result int
	child  *stacklessCoroComparisonAwaitFrame
}

// stacklessCoroCompactPullFrame models the frame ownership a pull lowering
// can use when structured children are polled by their root instead of being
// independently scheduled tasks. parent is the explicit stack link that
// keeps polling iterative and bounded.
type stacklessCoroCompactPullFrame struct {
	parent *stacklessCoroCompactPullFrame
	depth  int
	state  uint8
	result int
}

type stacklessCoroCompactPullRoot struct {
	first  stacklessCoroCompactPullFrame
	leaf   *stacklessCoroCompactPullFrame
	result int
}

const stacklessCoroCompactPullBudget = 256
const stacklessCoroComparisonWatchdog = 30 * time.Second

func stacklessCoroCompactPullResume(ctx unsafe.Pointer) uint8 {
	root := (*stacklessCoroCompactPullRoot)(
		runtime.FrameStacklessCoroForTest(ctx))
	for range stacklessCoroCompactPullBudget {
		frame := root.leaf
		if frame == nil {
			return runtime.StacklessCoroActionInvalid
		}
		switch frame.state {
		case 0:
			if frame.depth == 0 {
				frame.state = 1
				// Model one pending leaf. A real event source would ring the
				// root doorbell before the next poll episode.
				return runtime.StacklessCoroActionYield
			}
			child := &stacklessCoroCompactPullFrame{
				parent: frame,
				depth:  frame.depth - 1,
			}
			frame.state = 1
			root.leaf = child
		case 1:
			if frame.depth != 0 {
				return runtime.StacklessCoroActionInvalid
			}
			frame.result = 1
			fallthrough
		case 2:
			if frame.parent == nil {
				root.result = frame.result
				root.leaf = nil
				return runtime.StacklessCoroActionComplete
			}
			parent := frame.parent
			parent.result = frame.result + 1
			parent.state = 2
			root.leaf = parent
		default:
			return runtime.StacklessCoroActionInvalid
		}
	}
	// Bound a ready chain so a deep tree cannot monopolize its P.
	return runtime.StacklessCoroActionYield
}

// stacklessCoroCompactPullFootprintFrame carries the same logical fields as
// stacklessCoroComparisonFootprintFrame. Keeping leaf-only-looking state in
// every recursive frame avoids crediting pull with an unrelated frame-hoist
// optimization.
type stacklessCoroCompactPullFootprintFrame struct {
	parent *stacklessCoroCompactPullFootprintFrame
	depth  int
	state  uint8
	result int
	gate   <-chan int
	parked *atomic.Bool
	value  int
	recv   bool
}

type stacklessCoroCompactPullFootprintRoot struct {
	first  stacklessCoroCompactPullFootprintFrame
	leaf   *stacklessCoroCompactPullFootprintFrame
	result int
}

func stacklessCoroCompactPullFootprintResume(ctx unsafe.Pointer) uint8 {
	root := (*stacklessCoroCompactPullFootprintRoot)(
		runtime.FrameStacklessCoroForTest(ctx))
	for range stacklessCoroCompactPullBudget {
		frame := root.leaf
		if frame == nil {
			return runtime.StacklessCoroActionInvalid
		}
		switch frame.state {
		case 0:
			if frame.depth == 0 {
				frame.state = 1
				runtime.RecvIntStacklessCoroForTest(ctx, frame.gate,
					&frame.value, &frame.recv)
				frame.parked.Store(true)
				return runtime.StacklessCoroActionWait
			}
			child := &stacklessCoroCompactPullFootprintFrame{
				parent: frame,
				depth:  frame.depth - 1,
				gate:   frame.gate,
				parked: frame.parked,
			}
			frame.state = 1
			root.leaf = child
		case 1:
			if frame.depth != 0 || !frame.recv || frame.value != 1 {
				return runtime.StacklessCoroActionInvalid
			}
			frame.result = 1
			fallthrough
		case 2:
			if frame.parent == nil {
				root.result = frame.result
				root.leaf = nil
				return runtime.StacklessCoroActionComplete
			}
			parent := frame.parent
			parent.result = frame.result + 1
			parent.state = 2
			root.leaf = parent
		default:
			return runtime.StacklessCoroActionInvalid
		}
	}
	return runtime.StacklessCoroActionYield
}

func stacklessCoroComparisonAwaitResume(ctx unsafe.Pointer) uint8 {
	frame := (*stacklessCoroComparisonAwaitFrame)(
		runtime.FrameStacklessCoroForTest(ctx))
	switch frame.state {
	case 0:
		if frame.depth == 0 {
			frame.state = 1
			return runtime.StacklessCoroActionYield
		}
		frame.child = &stacklessCoroComparisonAwaitFrame{
			depth: frame.depth - 1,
		}
		frame.state = 2
		runtime.AwaitStacklessCoroFrameForTest(ctx,
			unsafe.Pointer(frame.child), stacklessCoroComparisonAwaitResume)
		return runtime.StacklessCoroActionWait
	case 1:
		frame.result = 1
		return runtime.StacklessCoroActionComplete
	case 2:
		frame.result = frame.child.result + 1
		frame.child = nil
		return runtime.StacklessCoroActionComplete
	default:
		return runtime.StacklessCoroActionInvalid
	}
}

type stacklessCoroComparisonTimerFrame struct {
	remaining            int
	completed            int
	pending              bool
	delay                int64
	armed                chan<- struct{}
	progress             *atomic.Bool
	progressAtCompletion bool
	token                runtime.StacklessCoroTimerTokenForTest
	manualTimer          bool
}

type stacklessCoroComparisonFootprintFrame struct {
	depth  int
	state  uint8
	result int
	child  *stacklessCoroComparisonFootprintFrame
	gate   <-chan int
	parked *atomic.Bool
	value  int
	recv   bool
}

func stacklessCoroComparisonFootprintResume(ctx unsafe.Pointer) uint8 {
	frame := (*stacklessCoroComparisonFootprintFrame)(
		runtime.FrameStacklessCoroForTest(ctx))
	switch frame.state {
	case 0:
		if frame.depth == 0 {
			frame.state = 1
			runtime.RecvIntStacklessCoroForTest(ctx, frame.gate,
				&frame.value, &frame.recv)
			frame.parked.Store(true)
			return runtime.StacklessCoroActionWait
		}
		frame.child = &stacklessCoroComparisonFootprintFrame{
			depth:  frame.depth - 1,
			gate:   frame.gate,
			parked: frame.parked,
		}
		frame.state = 1
		runtime.AwaitStacklessCoroFrameForTest(ctx,
			unsafe.Pointer(frame.child), stacklessCoroComparisonFootprintResume)
		return runtime.StacklessCoroActionWait
	case 1:
		if frame.depth == 0 {
			if !frame.recv || frame.value != 1 {
				return runtime.StacklessCoroActionInvalid
			}
			frame.result = 1
		} else {
			frame.result = frame.child.result + 1
			frame.child = nil
		}
		return runtime.StacklessCoroActionComplete
	default:
		return runtime.StacklessCoroActionInvalid
	}
}

func stacklessCoroComparisonTimerResume(ctx unsafe.Pointer) uint8 {
	frame := (*stacklessCoroComparisonTimerFrame)(
		runtime.FrameStacklessCoroForTest(ctx))
	if frame.pending {
		frame.pending = false
		if frame.progress != nil {
			frame.progressAtCompletion = frame.progress.Load()
		}
		frame.completed++
		frame.remaining--
	}
	if frame.remaining == 0 {
		return runtime.StacklessCoroActionComplete
	}
	frame.pending = true
	if frame.manualTimer {
		frame.token = runtime.StartSleepStacklessCoroForTest(ctx,
			int64(time.Hour))
	} else {
		if !runtime.SleepStacklessCoroForTest(ctx, frame.delay) {
			return runtime.StacklessCoroActionInvalid
		}
	}
	if frame.armed != nil {
		frame.armed <- struct{}{}
		frame.armed = nil
	}
	return runtime.StacklessCoroActionWait
}

type stacklessCoroComparisonFileFrame struct {
	fd        int
	buffer    []byte
	armed     chan<- struct{}
	remaining int
	completed int
	pending   bool
	n         int
	errno     uintptr
}

type stacklessCoroComparisonSocketFrame struct {
	fd        int
	buffer    []byte
	armed     chan<- struct{}
	remaining int
	completed int
	pending   bool
	n         int
	errno     uintptr
}

func stacklessCoroComparisonFileResume(ctx unsafe.Pointer) uint8 {
	frame := (*stacklessCoroComparisonFileFrame)(
		runtime.FrameStacklessCoroForTest(ctx))
	if frame.pending {
		if frame.n != 1 || frame.errno != 0 {
			return runtime.StacklessCoroActionInvalid
		}
		frame.pending = false
		frame.completed++
		frame.remaining--
	}
	if frame.remaining == 0 {
		return runtime.StacklessCoroActionComplete
	}
	frame.n = -1
	frame.errno = ^uintptr(0)
	frame.pending = true
	if frame.armed != nil {
		frame.armed <- struct{}{}
	}
	runtime.PublicFileReadStacklessCoroForTest(ctx, frame.fd, frame.buffer,
		&frame.n, &frame.errno)
	return runtime.StacklessCoroActionWait
}

func stacklessCoroComparisonSocketResume(ctx unsafe.Pointer) uint8 {
	frame := (*stacklessCoroComparisonSocketFrame)(
		runtime.FrameStacklessCoroForTest(ctx))
	if frame.pending {
		if frame.n != 1 || frame.errno != 0 {
			return runtime.StacklessCoroActionInvalid
		}
		frame.pending = false
		frame.completed++
		frame.remaining--
	}
	if frame.remaining == 0 {
		return runtime.StacklessCoroActionComplete
	}
	frame.n = -1
	frame.errno = ^uintptr(0)
	frame.pending = true
	runtime.SocketReadStacklessCoroForTest(ctx, frame.fd, frame.buffer,
		&frame.n, &frame.errno)
	if frame.armed != nil {
		frame.armed <- struct{}{}
	}
	return runtime.StacklessCoroActionWait
}

func runStacklessCoroComparisonYield(driver stacklessCoroComparisonDriver,
	iterations int) int {
	frame := &stacklessCoroComparisonYieldFrame{remaining: iterations}
	driver.run(unsafe.Pointer(frame), stacklessCoroComparisonYieldResume)
	return frame.completed
}

func runStacklessCoroComparisonAwait(driver stacklessCoroComparisonDriver,
	depth int) int {
	frame := &stacklessCoroComparisonAwaitFrame{depth: depth}
	driver.run(unsafe.Pointer(frame), stacklessCoroComparisonAwaitResume)
	return frame.result
}

func runStacklessCoroCompactPullAwait(depth int) int {
	root := &stacklessCoroCompactPullRoot{}
	root.first.depth = depth
	root.leaf = &root.first
	runtime.RunStacklessCoroPullFrameForTest(unsafe.Pointer(root),
		stacklessCoroCompactPullResume)
	return root.result
}

func stacklessCoroComparisonStackfulPark(depth int, gate <-chan int,
	parked *atomic.Bool) int {
	if depth == 0 {
		parked.Store(true)
		return <-gate
	}
	return stacklessCoroComparisonStackfulPark(depth-1, gate, parked) + 1
}

func startStacklessCoroComparisonStackfulFootprint(depth int, gate <-chan int,
	parked *atomic.Bool) <-chan int {
	done := make(chan int, 1)
	go func() {
		done <- stacklessCoroComparisonStackfulPark(depth, gate, parked)
	}()
	return done
}

func startStacklessCoroComparisonFootprint(
	driver stacklessCoroComparisonDriver, depth int, gate <-chan int,
	parked *atomic.Bool) <-chan int {
	done := make(chan int, 1)
	go func() {
		frame := &stacklessCoroComparisonFootprintFrame{
			depth:  depth,
			gate:   gate,
			parked: parked,
		}
		driver.run(unsafe.Pointer(frame),
			stacklessCoroComparisonFootprintResume)
		done <- frame.result
	}()
	return done
}

func startStacklessCoroCompactPullFootprint(depth int, gate <-chan int,
	parked *atomic.Bool) <-chan int {
	done := make(chan int, 1)
	go func() {
		root := &stacklessCoroCompactPullFootprintRoot{}
		root.first.depth = depth
		root.first.gate = gate
		root.first.parked = parked
		root.leaf = &root.first
		runtime.RunStacklessCoroPullFrameForTest(unsafe.Pointer(root),
			stacklessCoroCompactPullFootprintResume)
		done <- root.result
	}()
	return done
}

type stacklessCoroFootprintSample struct {
	heapAlloc  uint64
	heapObject uint64
	stackBytes uint64
	scanHeap   uint64
	scanStack  uint64
}

func readStacklessCoroFootprintSample() stacklessCoroFootprintSample {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	samples := []metrics.Sample{
		{Name: "/gc/scan/heap:bytes"},
		{Name: "/gc/scan/stack:bytes"},
	}
	metrics.Read(samples)
	return stacklessCoroFootprintSample{
		heapAlloc:  memory.HeapAlloc,
		heapObject: memory.HeapObjects,
		stackBytes: memory.StackInuse,
		scanHeap:   samples[0].Value.Uint64(),
		scanStack:  samples[1].Value.Uint64(),
	}
}

func stacklessCoroMetricDelta(after, before uint64) uint64 {
	if after < before {
		return 0
	}
	return after - before
}

func benchmarkStacklessCoroFootprint(b *testing.B, depth int,
	start func(int, <-chan int, *atomic.Bool) <-chan int) {
	if b.N != 1 {
		b.Skip("requires -benchtime=1x")
	}
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)
	oldGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(oldGC)
	debug.FreeOSMemory()
	before := readStacklessCoroFootprintSample()

	gate := make(chan int)
	var parked atomic.Bool
	done := start(depth, gate, &parked)
	deadline := time.Now().Add(10 * time.Second)
	for !parked.Load() {
		if time.Now().After(deadline) {
			b.Fatal("coroutine footprint did not park")
		}
		runtime.Gosched()
	}

	const gcCycles = 5
	gcStart := time.Now()
	for range gcCycles {
		runtime.GC()
	}
	gcElapsed := time.Since(gcStart)
	parkedSample := readStacklessCoroFootprintSample()
	gate <- 1
	if got := <-done; got != depth+1 {
		b.Fatalf("footprint result = %d, want %d", got, depth+1)
	}

	heap := stacklessCoroMetricDelta(parkedSample.heapAlloc,
		before.heapAlloc)
	objects := stacklessCoroMetricDelta(parkedSample.heapObject,
		before.heapObject)
	stacks := stacklessCoroMetricDelta(parkedSample.stackBytes,
		before.stackBytes)
	scanHeap := stacklessCoroMetricDelta(parkedSample.scanHeap,
		before.scanHeap)
	scanStack := stacklessCoroMetricDelta(parkedSample.scanStack,
		before.scanStack)
	b.ReportMetric(float64(heap), "parked-heap-B")
	b.ReportMetric(float64(objects), "parked-objects")
	b.ReportMetric(float64(stacks), "parked-stack-B")
	b.ReportMetric(float64(scanHeap), "scan-heap-B")
	b.ReportMetric(float64(scanStack), "scan-stack-B")
	b.ReportMetric(float64(gcElapsed.Nanoseconds()/gcCycles), "gc-ns/cycle")
	runtime.KeepAlive(parkedSample)
}

func runStacklessCoroComparisonTimer(driver stacklessCoroComparisonDriver,
	iterations int, delay time.Duration) int {
	frame := &stacklessCoroComparisonTimerFrame{
		remaining: iterations,
		delay:     int64(delay),
	}
	driver.run(unsafe.Pointer(frame), stacklessCoroComparisonTimerResume)
	return frame.completed
}

func runStacklessCoroComparisonFile(driver stacklessCoroComparisonDriver,
	fd, iterations int) int {
	return runStacklessCoroComparisonFileArmed(driver, fd, iterations, nil)
}

func runStacklessCoroComparisonFileArmed(driver stacklessCoroComparisonDriver,
	fd, iterations int, armed chan<- struct{}) int {
	frame := &stacklessCoroComparisonFileFrame{
		fd:        fd,
		buffer:    make([]byte, 1),
		armed:     armed,
		remaining: iterations,
	}
	driver.run(unsafe.Pointer(frame), stacklessCoroComparisonFileResume)
	return frame.completed
}

func startStacklessCoroComparisonFileWriter(fd, iterations int,
	armed <-chan struct{}) <-chan error {
	done := make(chan error, 1)
	go func() {
		var err error
		for range iterations {
			<-armed
			_, err = syscall.Write(fd, []byte{1})
			if err != nil {
				break
			}
		}
		done <- err
	}()
	return done
}

func runStacklessCoroComparisonSocket(driver stacklessCoroComparisonDriver,
	fd, iterations int, armed chan<- struct{}) int {
	frame := &stacklessCoroComparisonSocketFrame{
		fd:        fd,
		buffer:    make([]byte, 1),
		armed:     armed,
		remaining: iterations,
	}
	driver.run(unsafe.Pointer(frame), stacklessCoroComparisonSocketResume)
	return frame.completed
}

func startStacklessCoroComparisonSocketWriter(fd, iterations int,
	armed <-chan struct{}, baselineWaiters uint32,
	timedOut *atomic.Bool) <-chan error {
	done := make(chan error, 1)
	go func() {
		var err error
		for range iterations {
			if armed != nil {
				<-armed
				for runtime.StacklessCoroNetpollWaiterCountForTest() <=
					baselineWaiters {
					if timedOut.Load() {
						err = syscall.ETIMEDOUT
						_, _ = syscall.Write(fd, []byte{1})
						break
					}
					runtime.Gosched()
				}
				if err != nil {
					break
				}
			}
			_, err = syscall.Write(fd, []byte{1})
			if err != nil {
				break
			}
		}
		done <- err
	}()
	return done
}

func TestStacklessCoroPushPullComparison(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	fd, err := syscall.Open("/dev/zero", syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)

	for _, driver := range stacklessCoroComparisonDrivers {
		t.Run(driver.name, func(t *testing.T) {
			if got := runStacklessCoroComparisonYield(driver, 3); got != 3 {
				t.Fatalf("yield comparison completed %d transitions, want 3", got)
			}
			if got := runStacklessCoroComparisonAwait(driver, 32); got != 33 {
				t.Fatalf("await comparison result = %d, want 33", got)
			}
			if driver.name == "Pull" {
				if got := runStacklessCoroComparisonAwait(driver, 300); got != 301 {
					t.Fatalf("deep await comparison result = %d, want 301", got)
				}
			}
			if got := runStacklessCoroComparisonTimer(driver, 2,
				time.Nanosecond); got != 2 {
				t.Fatalf("timer comparison completed %d timers, want 2", got)
			}
			timerArmed := make(chan struct{}, 1)
			progressDone := make(chan struct{})
			cancelDone := make(chan bool, 1)
			var progress atomic.Bool
			timerFrame := &stacklessCoroComparisonTimerFrame{
				remaining:   1,
				armed:       timerArmed,
				progress:    &progress,
				manualTimer: true,
			}
			go func() {
				<-timerArmed
				progress.Store(true)
				cancelDone <- runtime.CancelSleepStacklessCoroForTest(
					timerFrame.token)
				close(progressDone)
			}()
			driver.run(unsafe.Pointer(timerFrame),
				stacklessCoroComparisonTimerResume)
			<-progressDone
			if !<-cancelDone {
				t.Fatal("progress goroutine did not cancel pending timer")
			}
			if !timerFrame.progressAtCompletion {
				t.Fatal("runnable goroutine made no progress while timer waited")
			}
			if got := runStacklessCoroComparisonFile(driver, fd, 2); got != 2 {
				t.Fatalf("file comparison completed %d reads, want 2", got)
			}
			pipeReader, pipeWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer pipeReader.Close()
			defer pipeWriter.Close()
			fileWatchdog := time.AfterFunc(stacklessCoroComparisonWatchdog, func() {
				pipeReader.Close()
				pipeWriter.Close()
			})
			fileArmed := make(chan struct{}, 1)
			fileWriteDone := startStacklessCoroComparisonFileWriter(
				int(pipeWriter.Fd()), 2, fileArmed)
			if got := runStacklessCoroComparisonFileArmed(driver,
				int(pipeReader.Fd()), 2, fileArmed); got != 2 {
				t.Fatalf("blocking file comparison completed %d reads, want 2",
					got)
			}
			if err := <-fileWriteDone; err != nil {
				t.Fatal(err)
			}
			if !fileWatchdog.Stop() {
				t.Fatal("blocking file comparison timed out")
			}

			fds, err := syscall.Socketpair(syscall.AF_LOCAL,
				syscall.SOCK_STREAM, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Close(fds[0])
			defer syscall.Close(fds[1])
			if err := syscall.SetNonblock(fds[0], true); err != nil {
				t.Fatal(err)
			}
			baselineWaiters := runtime.StacklessCoroNetpollWaiterCountForTest()
			armed := make(chan struct{}, 1)
			var timedOut atomic.Bool
			watchdog := time.AfterFunc(stacklessCoroComparisonWatchdog, func() {
				timedOut.Store(true)
			})
			writeDone := startStacklessCoroComparisonSocketWriter(fds[1], 2,
				armed, baselineWaiters, &timedOut)
			if got := runStacklessCoroComparisonSocket(driver, fds[0], 2,
				armed); got != 2 {
				t.Fatalf("socket comparison completed %d reads, want 2", got)
			}
			if err := <-writeDone; err != nil {
				t.Fatal(err)
			}
			if !watchdog.Stop() {
				t.Fatal("socket comparison timed out")
			}
		})
	}
	if got := runStacklessCoroCompactPullAwait(32); got != 33 {
		t.Fatalf("compact pull await result = %d, want 33", got)
	}
	if got := runStacklessCoroCompactPullAwait(4096); got != 4097 {
		t.Fatalf("deep compact pull await result = %d, want 4097", got)
	}
}

func stacklessCoroComparisonCompleteResume(unsafe.Pointer) uint8 {
	return runtime.StacklessCoroActionComplete
}

func stacklessCoroComparisonStackfulAwait(depth int) int {
	if depth == 0 {
		runtime.Gosched()
		return 1
	}
	return stacklessCoroComparisonStackfulAwait(depth-1) + 1
}

var stacklessCoroComparisonSink int

func BenchmarkStacklessCoroPushPullEntry(b *testing.B) {
	b.Run("Stackful", func(b *testing.B) {
		done := make(chan struct{})
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			go func() {
				done <- struct{}{}
			}()
			<-done
		}
	})
	for _, driver := range stacklessCoroComparisonDrivers {
		b.Run(driver.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				driver.run(nil, stacklessCoroComparisonCompleteResume)
			}
		})
	}
}

func BenchmarkStacklessCoroPushPullYield(b *testing.B) {
	b.Run("Stackful", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			runtime.Gosched()
		}
	})
	for _, driver := range stacklessCoroComparisonDrivers {
		b.Run(driver.name, func(b *testing.B) {
			b.ReportAllocs()
			if got := runStacklessCoroComparisonYield(driver, b.N); got != b.N {
				b.Fatalf("completed %d yields, want %d", got, b.N)
			}
		})
	}
}

func benchmarkStacklessCoroPushPullAwaitDepth(b *testing.B, depth int) {
	b.Run("Stackful", func(b *testing.B) {
		b.ReportAllocs()
		result := 0
		for range b.N {
			result += stacklessCoroComparisonStackfulAwait(depth)
		}
		stacklessCoroComparisonSink = result
	})
	for _, driver := range stacklessCoroComparisonDrivers {
		b.Run(driver.name, func(b *testing.B) {
			b.ReportAllocs()
			result := 0
			for range b.N {
				result += runStacklessCoroComparisonAwait(driver, depth)
			}
			stacklessCoroComparisonSink = result
		})
	}
	b.Run("CompactPull", func(b *testing.B) {
		b.ReportAllocs()
		result := 0
		for range b.N {
			result += runStacklessCoroCompactPullAwait(depth)
		}
		stacklessCoroComparisonSink = result
	})
}

func BenchmarkStacklessCoroPushPullAwait1(b *testing.B) {
	benchmarkStacklessCoroPushPullAwaitDepth(b, 1)
}

func BenchmarkStacklessCoroPushPullAwait8(b *testing.B) {
	benchmarkStacklessCoroPushPullAwaitDepth(b, 8)
}

func BenchmarkStacklessCoroPushPullAwait64(b *testing.B) {
	benchmarkStacklessCoroPushPullAwaitDepth(b, 64)
}

func BenchmarkStacklessCoroPushPullAwait256(b *testing.B) {
	benchmarkStacklessCoroPushPullAwaitDepth(b, 256)
}

func BenchmarkStacklessCoroPushPullAwait4096(b *testing.B) {
	benchmarkStacklessCoroPushPullAwaitDepth(b, 4096)
}

func BenchmarkStacklessCoroPushPullFootprint4096(b *testing.B) {
	benchmarkStacklessCoroPushPullFootprint(b, 4096)
}

func BenchmarkStacklessCoroPushPullFootprint0(b *testing.B) {
	benchmarkStacklessCoroPushPullFootprint(b, 0)
}

func benchmarkStacklessCoroPushPullFootprint(b *testing.B, depth int) {
	b.Run("Stackful", func(b *testing.B) {
		benchmarkStacklessCoroFootprint(b, depth,
			startStacklessCoroComparisonStackfulFootprint)
	})
	for _, driver := range stacklessCoroComparisonDrivers {
		b.Run(driver.name, func(b *testing.B) {
			benchmarkStacklessCoroFootprint(b, depth,
				func(depth int, gate <-chan int,
					parked *atomic.Bool) <-chan int {
					return startStacklessCoroComparisonFootprint(driver,
						depth, gate, parked)
				})
		})
	}
	b.Run("CompactPull", func(b *testing.B) {
		benchmarkStacklessCoroFootprint(b, depth,
			startStacklessCoroCompactPullFootprint)
	})
}

func BenchmarkStacklessCoroPushPullTimer(b *testing.B) {
	b.Run("Stackful", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			time.Sleep(time.Nanosecond)
		}
	})
	for _, driver := range stacklessCoroComparisonDrivers {
		b.Run(driver.name, func(b *testing.B) {
			b.ReportAllocs()
			if got := runStacklessCoroComparisonTimer(driver, b.N,
				time.Nanosecond); got != b.N {
				b.Fatalf("completed %d timers, want %d", got, b.N)
			}
		})
	}
}

func BenchmarkStacklessCoroPushPullFileReady(b *testing.B) {
	fd, err := syscall.Open("/dev/zero", syscall.O_RDONLY, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer syscall.Close(fd)

	b.Run("Stackful", func(b *testing.B) {
		buffer := make([]byte, 1)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if n, err := syscall.Read(fd, buffer); err != nil || n != 1 {
				b.Fatalf("read = (%d, %v), want (1, nil)", n, err)
			}
		}
	})
	for _, driver := range stacklessCoroComparisonDrivers {
		b.Run(driver.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			if got := runStacklessCoroComparisonFile(driver, fd, b.N); got != b.N {
				b.Fatalf("completed %d reads, want %d", got, b.N)
			}
		})
	}
}

func BenchmarkStacklessCoroPushPullFileBlocked(b *testing.B) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)
	for _, driver := range stacklessCoroComparisonDrivers {
		b.Run(driver.name, func(b *testing.B) {
			reader, writer, err := os.Pipe()
			if err != nil {
				b.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			armed := make(chan struct{}, 1)
			writeDone := startStacklessCoroComparisonFileWriter(
				int(writer.Fd()), b.N, armed)
			b.ReportAllocs()
			b.ResetTimer()
			got := runStacklessCoroComparisonFileArmed(driver,
				int(reader.Fd()), b.N, armed)
			b.StopTimer()
			if got != b.N {
				b.Fatalf("completed %d reads, want %d", got, b.N)
			}
			if err := <-writeDone; err != nil {
				b.Fatal(err)
			}
		})
	}
}

func BenchmarkStacklessCoroPushPullSocketReady(b *testing.B) {
	benchmarkStacklessCoroPushPullSocket(b, false)
}

func BenchmarkStacklessCoroPushPullSocketBlocked(b *testing.B) {
	benchmarkStacklessCoroPushPullSocket(b, true)
}

func benchmarkStacklessCoroPushPullSocket(b *testing.B, wait bool) {
	for _, driver := range stacklessCoroComparisonDrivers {
		b.Run(driver.name, func(b *testing.B) {
			fds, err := syscall.Socketpair(syscall.AF_LOCAL,
				syscall.SOCK_STREAM, 0)
			if err != nil {
				b.Fatal(err)
			}
			defer syscall.Close(fds[0])
			defer syscall.Close(fds[1])
			if err := syscall.SetNonblock(fds[0], true); err != nil {
				b.Fatal(err)
			}

			baselineWaiters := runtime.StacklessCoroNetpollWaiterCountForTest()
			var armed chan struct{}
			if wait {
				armed = make(chan struct{}, 1)
			}
			var timedOut atomic.Bool
			watchdog := time.AfterFunc(30*time.Second, func() {
				timedOut.Store(true)
			})
			defer watchdog.Stop()
			writeDone := startStacklessCoroComparisonSocketWriter(fds[1], b.N,
				armed, baselineWaiters, &timedOut)
			b.ReportAllocs()
			b.ResetTimer()
			got := runStacklessCoroComparisonSocket(driver, fds[0], b.N,
				armed)
			b.StopTimer()
			if got != b.N {
				b.Fatalf("completed %d reads, want %d", got, b.N)
			}
			if err := <-writeDone; err != nil {
				b.Fatal(err)
			}
		})
	}
}
