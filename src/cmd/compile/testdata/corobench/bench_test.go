// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package corobench

import (
	"errors"
	"io"
	"math"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"
)

var (
	intSink   int
	uintSink  uint64
	floatSink float64
)

func TestProbes(t *testing.T) {
	if got := yieldLoop(2); got != 2 {
		t.Errorf("yieldLoop(2) = %d, want 2", got)
	}
	if got := yieldEntry(); got != 1 {
		t.Errorf("yieldEntry() = %d, want 1", got)
	}
	if got := recursiveYield(4); got != 5 {
		t.Errorf("recursiveYield(4) = %d, want 5", got)
	}
	if got := mutualYieldA(4); got != 5 {
		t.Errorf("mutualYieldA(4) = %d, want 5", got)
	}
	if got := mutualYieldB(4); got != 5 {
		t.Errorf("mutualYieldB(4) = %d, want 5", got)
	}
	if got := deferYield(); got != 2 {
		t.Errorf("deferYield() = %d, want 2", got)
	}
	if got := recoverYield(); got != 1 {
		t.Errorf("recoverYield() = %d, want 1", got)
	}
	if got := taskSequence(3); got != 3 {
		t.Errorf("taskSequence(3) = %d, want 3", got)
	}
	if got := taskBursts(2, 3); got != 6 {
		t.Errorf("taskBursts(2, 3) = %d, want 6", got)
	}
	if got := taskParkBursts(2, 3); got != 6 {
		t.Errorf("taskParkBursts(2, 3) = %d, want 6", got)
	}
	if got := parallelYieldWork(1, 4, 8); got == 0 {
		t.Error("parallelYieldWork(1, 4, 8) returned zero")
	}
	if got := channelRoundTrips(4); got != 10 {
		t.Errorf("channelRoundTrips(4) = %d, want 10", got)
	}
	if got := readySelects(4); got != 6 {
		t.Errorf("readySelects(4) = %d, want 6", got)
	}
	if got := sleepLoop(2, 0); got != 2 {
		t.Errorf("sleepLoop(2, 0) = %d, want 2", got)
	}
	if got := sleepLoop(2, -1); got != 2 {
		t.Errorf("sleepLoop(2, -1) = %d, want 2", got)
	}

	file, err := os.Open("/dev/zero")
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1)
	if got, err := fileReads(file, buffer, 2); err != nil || got != 2 {
		t.Errorf("fileReads(..., 2) = (%d, %v), want (2, nil)", got, err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	reader, writer, cleanup := newTCPPair(t)
	go writeTCP(writer)
	if got, err := tcpReads(reader, buffer, 2); err != nil || got != 2 {
		t.Errorf("tcpReads(..., 2) = (%d, %v), want (2, nil)", got, err)
	}
	cleanup()

	pipeReader, pipeWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	fileWatchdog := time.AfterFunc(5*time.Second, func() {
		pipeReader.Close()
		pipeWriter.Close()
	})
	if got, err := blockingFileRoundTrips(pipeReader, pipeWriter.Fd(), 2); err != nil || got != 2 {
		t.Errorf("blockingFileRoundTrips(..., 2) = (%d, %v), want (2, nil)",
			got, err)
	}
	if !fileWatchdog.Stop() {
		t.Error("blocking file probe timed out")
	}
	pipeReader.Close()
	pipeWriter.Close()

	contentionProcs := runtime.GOMAXPROCS(1)
	pipeReader, pipeWriter, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	contentionWatchdog := time.AfterFunc(5*time.Second, func() {
		pipeReader.Close()
		pipeWriter.Close()
	})
	if err := contendedFileReads(pipeReader, pipeWriter.Fd()); err != nil {
		t.Errorf("contendedFileReads() = %v, want nil", err)
	}
	if !contentionWatchdog.Stop() {
		t.Error("contended file reads timed out")
	}
	pipeReader.Close()
	pipeWriter.Close()

	reader, writer, cleanup = newTCPPair(t)
	networkWatchdog := time.AfterFunc(5*time.Second, cleanup)
	if got, err := blockingTCPRoundTrips(reader, tcpFD(t, writer), 2); err != nil || got != 2 {
		t.Errorf("blockingTCPRoundTrips(..., 2) = (%d, %v), want (2, nil)",
			got, err)
	}
	if !networkWatchdog.Stop() {
		t.Error("blocking TCP probe timed out")
	}
	cleanup()

	reader, writer, cleanup = newTCPPair(t)
	contentionWatchdog = time.AfterFunc(5*time.Second, cleanup)
	if err := contendedTCPReads(reader, tcpFD(t, writer)); err != nil {
		t.Errorf("contendedTCPReads() = %v, want nil", err)
	}
	if !contentionWatchdog.Stop() {
		t.Error("contended TCP reads timed out")
	}
	cleanup()
	runtime.GOMAXPROCS(contentionProcs)

	if got := cScalarCalls(3); got != 4 {
		t.Errorf("cScalarCalls(3) = %d, want 4", got)
	}
	integer, floating := cPairCalls(3)
	if integer != 7 || floating != 1.25 {
		t.Errorf("cPairCalls(3) = (%d, %g), want (7, 1.25)",
			integer, floating)
	}
	if got, err := cErrnoCalls(3); err != nil || got != 4 {
		t.Errorf("cErrnoCalls(3) = (%d, %v), want (4, nil)", got, err)
	}
	if got := cLibmCalls(3); math.IsNaN(got) {
		t.Errorf("cLibmCalls(3) = NaN")
	}

	old := runtime.GOMAXPROCS(1)
	if got := cBlockingHandoffs(1); got == 0 {
		t.Error("cBlockingHandoffs(1) did not measure progress")
	}
	if elapsed, timeouts := cBlockingGroup(1, 3, 250*time.Millisecond); elapsed == 0 || timeouts != 0 {
		t.Errorf("cBlockingGroup(1, 3) = (%d, %d), want elapsed and no timeout",
			elapsed, timeouts)
	}
	runtime.GOMAXPROCS(old)
}

func TestCBlockingGroupCapacity(t *testing.T) {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	elapsed, timeouts := cBlockingGroup(1, 8, 250*time.Millisecond)
	if elapsed == 0 || timeouts != 0 {
		t.Fatalf("cBlockingGroup(1, 8) = (%d, %d), want elapsed and no timeouts",
			elapsed, timeouts)
	}
}

func TestParallelSpawnProgress(t *testing.T) {
	const workers = 3

	oldProcs := runtime.GOMAXPROCS(workers + 1)
	defer runtime.GOMAXPROCS(oldProcs)
	atomic.StoreUint32(&parallelSpawnTimeout, 0)
	watchdog := time.AfterFunc(5*time.Second, func() {
		atomic.StoreUint32(&parallelSpawnTimeout, 1)
	})
	if !parallelSpawnProgress(workers) {
		t.Fatal("spawned tasks did not run while their parent was active")
	}
	if !watchdog.Stop() {
		t.Fatal("parallel spawn progress required watchdog recovery")
	}
}

func TestProbeEdges(t *testing.T) {
	writeFailure := errors.New("write failure")
	readFailure := errors.New("read failure")
	for _, test := range []struct {
		name  string
		state blockingIOState
		want  error
	}{
		{"success", blockingIOState{writeN: 1, readN: 1, progressed: 1}, nil},
		{"write error", blockingIOState{writeErr: writeFailure}, writeFailure},
		{"read error", blockingIOState{readErr: readFailure}, readFailure},
		{"short write", blockingIOState{readN: 1}, io.ErrShortWrite},
		{"no read progress", blockingIOState{writeN: 1}, io.ErrNoProgress},
		{"no sibling progress", blockingIOState{writeN: 1, readN: 1},
			errBlockingIONoProgress},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := blockingIOError(&test.state, 1); got != test.want {
				t.Errorf("blockingIOError() = %v, want %v", got, test.want)
			}
		})
	}

	if got := yieldLoop(0); got != 0 {
		t.Errorf("yieldLoop(0) = %d, want 0", got)
	}
	if got := taskSequence(0); got != 0 {
		t.Errorf("taskSequence(0) = %d, want 0", got)
	}
	if got := taskBursts(0, 1); got != 0 {
		t.Errorf("taskBursts(0, 1) = %d, want 0", got)
	}
	if got := taskBursts(1, 0); got != 0 {
		t.Errorf("taskBursts(1, 0) = %d, want 0", got)
	}
	if got := taskParkBursts(0, 1); got != 0 {
		t.Errorf("taskParkBursts(0, 1) = %d, want 0", got)
	}
	if got := taskParkBursts(1, 0); got != 0 {
		t.Errorf("taskParkBursts(1, 0) = %d, want 0", got)
	}
	if got := taskParkUntilReleased(0); got != 0 {
		t.Errorf("taskParkUntilReleased(0) = %d, want 0", got)
	}
	if got := parallelYieldWork(0, 1, 1); got != 0 {
		t.Errorf("parallelYieldWork(0, 1, 1) = %d, want 0", got)
	}
	if got := parallelYieldWork(1, 0, 1); got != 0 {
		t.Errorf("parallelYieldWork(1, 0, 1) = %d, want 0", got)
	}
	if got := parallelYieldWork(1, 1, 0); got != 0 {
		t.Errorf("parallelYieldWork(1, 1, 0) = %d, want 0", got)
	}
	if got := channelRoundTrips(0); got != 0 {
		t.Errorf("channelRoundTrips(0) = %d, want 0", got)
	}
	if got := sleepLoop(0, 0); got != 0 {
		t.Errorf("sleepLoop(0, 0) = %d, want 0", got)
	}
	if got := cBlockingHandoffs(0); got != 0 {
		t.Errorf("cBlockingHandoffs(0) = %d, want 0", got)
	}
	if elapsed, timeouts := cBlockingGroup(0, 1, time.Second); elapsed != 0 || timeouts != 0 {
		t.Errorf("cBlockingGroup(0, 1, 1s) = (%d, %d), want (0, 0)",
			elapsed, timeouts)
	}
	if elapsed, timeouts := cBlockingGroup(1, 0, time.Second); elapsed != 0 || timeouts != 0 {
		t.Errorf("cBlockingGroup(1, 0, 1s) = (%d, %d), want (0, 0)",
			elapsed, timeouts)
	}
	if elapsed, timeouts := cBlockingGroup(1, 1, 0); elapsed != 0 || timeouts != 0 {
		t.Errorf("cBlockingGroup(1, 1, 0) = (%d, %d), want (0, 0)",
			elapsed, timeouts)
	}
	timeoutState := &cBlockingGroupState{epoch: 1, timeout: 1}
	activeCBlockingGroup = timeoutState
	cBlockingGroupWorker()
	if entered, done, timeouts := atomic.LoadUint64(&timeoutState.entered),
		atomic.LoadUint64(&timeoutState.done),
		atomic.LoadUint64(&timeoutState.timeouts); entered != 1 || done != 1 || timeouts != 1 {
		t.Errorf("timed blocking C call = (%d, %d, %d), want (1, 1, 1)",
			entered, done, timeouts)
	}
	if got, err := blockingFileRoundTrips(nil, 0, 0); got != 0 || err != nil {
		t.Errorf("blockingFileRoundTrips(nil, 0, 0) = (%d, %v), want (0, nil)",
			got, err)
	}
	if got, err := blockingTCPRoundTrips(nil, 0, 0); got != 0 || err != nil {
		t.Errorf("blockingTCPRoundTrips(nil, 0, 0) = (%d, %v), want (0, nil)",
			got, err)
	}

	done := make(chan uint64, 1)
	go func() {
		done <- taskParkUntilReleased(4)
	}()
	readyDeadline := time.Now().Add(5 * time.Second)
	for atomic.LoadUint64(&taskParkReady) < 4 {
		if time.Now().After(readyDeadline) {
			t.Fatal("taskParkUntilReleased(4) did not park its tasks")
		}
		runtime.Gosched()
	}
	atomic.StoreUint64(&taskParkRelease, 1)
	select {
	case got := <-done:
		if got != 4 {
			t.Errorf("taskParkUntilReleased(4) = %d, want 4", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("taskParkUntilReleased(4) did not finish")
	}

	file, err := os.Open("/dev/zero")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fileReads(file, make([]byte, 1), 1); err == nil {
		t.Error("fileReads on a closed file succeeded")
	}
	if _, err := fileReads(nil, make([]byte, 1), 1); err == nil {
		t.Error("fileReads on a nil file succeeded")
	}

	file, err = os.Open("/dev/zero")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fileReads(file, nil, 1); !errors.Is(err, io.ErrNoProgress) {
		t.Errorf("zero-length fileReads error = %v, want io.ErrNoProgress", err)
	}
	file.Close()

	deadlineReader, deadlineWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := deadlineReader.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		deadlineReader.Close()
		deadlineWriter.Close()
		t.Fatal(err)
	}
	if _, err := fileReads(deadlineReader, make([]byte, 1), 1); err == nil {
		t.Error("fileReads after an expired deadline succeeded")
	}
	deadlineReader.Close()
	deadlineWriter.Close()

	file, err = os.Open("/dev/zero")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := blockingFileRoundTrips(file, writer.Fd(), 1); got != 0 || err == nil {
		t.Errorf("blockingFileRoundTrips on a closed reader = (%d, %v), want error",
			got, err)
	}
	invalidFD := writer.Fd()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := writeProbeByte(invalidFD, 1); err == nil {
		t.Error("writeProbeByte on a closed descriptor succeeded")
	}

	reader, tcpWriter, cleanup := newTCPPair(t)
	if err := reader.Close(); err != nil {
		cleanup()
		t.Fatal(err)
	}
	if _, err := tcpReads(reader, make([]byte, 1), 1); err == nil {
		t.Error("tcpReads on a closed connection succeeded")
	}
	cleanup()

	if _, err := tcpReads(new(net.TCPConn), make([]byte, 1), 1); err == nil {
		t.Error("tcpReads on a zero connection succeeded")
	}

	reader, tcpWriter, cleanup = newTCPPair(t)
	if _, err := tcpReads(reader, nil, 1); !errors.Is(err, io.ErrNoProgress) {
		t.Errorf("zero-length tcpReads error = %v, want io.ErrNoProgress", err)
	}
	cleanup()

	reader, tcpWriter, cleanup = newTCPPair(t)
	if err := reader.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		cleanup()
		t.Fatal(err)
	}
	if got, err := blockingTCPRoundTrips(reader, tcpFD(t, tcpWriter), 1); got != 0 || err == nil {
		t.Errorf("blockingTCPRoundTrips after an expired deadline = (%d, %v), want error",
			got, err)
	}
	cleanup()
}

func TestHeterogeneousFrameCacheReplacement(t *testing.T) {
	if got := recursiveYield(4096); got != 4097 {
		t.Fatalf("recursiveYield(4096) = %d, want 4097", got)
	}
	allocs := testing.AllocsPerRun(100, func() {
		if got := mutualYieldA(64); got != 65 {
			t.Fatalf("mutualYieldA(64) = %d, want 65", got)
		}
	})
	if allocs != 0 {
		t.Fatalf("mutual recursion after a deep homogeneous call allocated %.2f objects per run", allocs)
	}
}

func BenchmarkYieldBatch(b *testing.B) {
	b.ReportAllocs()
	if got := yieldLoop(b.N); got != b.N {
		b.Fatalf("yieldLoop(%d) = %d", b.N, got)
	}
}

func BenchmarkYieldEntry(b *testing.B) {
	b.ReportAllocs()
	total := 0
	for i := 0; i < b.N; i++ {
		total += yieldEntry()
	}
	intSink = total
}

func BenchmarkRecursiveYield64(b *testing.B) {
	b.ReportAllocs()
	total := 0
	for i := 0; i < b.N; i++ {
		total += recursiveYield(64)
	}
	intSink = total
}

func BenchmarkRecursiveYield256(b *testing.B) {
	b.ReportAllocs()
	total := 0
	for i := 0; i < b.N; i++ {
		total += recursiveYield(256)
	}
	intSink = total
}

func BenchmarkRecursiveYield262(b *testing.B) {
	b.ReportAllocs()
	total := 0
	for i := 0; i < b.N; i++ {
		total += recursiveYield(262)
	}
	intSink = total
}

func BenchmarkRecursiveYield264(b *testing.B) {
	b.ReportAllocs()
	total := 0
	for i := 0; i < b.N; i++ {
		total += recursiveYield(264)
	}
	intSink = total
}

func BenchmarkRecursiveYield267(b *testing.B) {
	b.ReportAllocs()
	total := 0
	for i := 0; i < b.N; i++ {
		total += recursiveYield(267)
	}
	intSink = total
}

func BenchmarkRecursiveYield4096(b *testing.B) {
	b.ReportAllocs()
	total := 0
	for i := 0; i < b.N; i++ {
		total += recursiveYield(4096)
	}
	intSink = total
}

func BenchmarkMutualYield64(b *testing.B) {
	b.ReportAllocs()
	total := 0
	for i := 0; i < b.N; i++ {
		total += mutualYieldA(64)
	}
	intSink = total
}

func BenchmarkMutualYield4096(b *testing.B) {
	b.ReportAllocs()
	total := 0
	for i := 0; i < b.N; i++ {
		total += mutualYieldA(4096)
	}
	intSink = total
}

func BenchmarkDeferYield(b *testing.B) {
	b.ReportAllocs()
	total := 0
	for i := 0; i < b.N; i++ {
		total += deferYield()
	}
	intSink = total
}

func BenchmarkRecoverYield(b *testing.B) {
	b.ReportAllocs()
	total := 0
	for i := 0; i < b.N; i++ {
		total += recoverYield()
	}
	intSink = total
}

func BenchmarkTaskSequence(b *testing.B) {
	b.ReportAllocs()
	if got := taskSequence(b.N); got != uint64(b.N) {
		b.Fatalf("taskSequence(%d) = %d", b.N, got)
	}
}

func BenchmarkTaskBurst100(b *testing.B) {
	b.ReportAllocs()
	if got := taskBursts(b.N, 100); got != uint64(100*b.N) {
		b.Fatalf("taskBursts(%d, 100) = %d", b.N, got)
	}
}

func BenchmarkTaskPark100(b *testing.B) {
	b.ReportAllocs()
	if got := taskParkBursts(b.N, 100); got != uint64(100*b.N) {
		b.Fatalf("taskParkBursts(%d, 100) = %d", b.N, got)
	}
}

func BenchmarkParallelYieldWork(b *testing.B) {
	b.ReportAllocs()
	if got := parallelYieldWork(b.N, 64, 1<<12); got == 0 {
		b.Fatal("parallelYieldWork returned zero")
	}
}

func BenchmarkTaskParkFootprint10000(b *testing.B) {
	if b.N != 1 {
		b.Skip("requires -benchtime=1x")
	}
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)
	oldGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(oldGC)
	runtime.GC()
	var before, parked runtime.MemStats
	runtime.ReadMemStats(&before)
	b.ReportAllocs()
	b.ResetTimer()
	atomic.StoreUint64(&taskParkReady, 0)
	atomic.StoreUint64(&taskParkRelease, 0)
	sessionDone := make(chan uint64, 1)
	go func() {
		sessionDone <- taskParkUntilReleased(10000)
	}()
	for atomic.LoadUint64(&taskParkReady) < 10000 {
		runtime.Gosched()
	}
	runtime.Gosched()
	runtime.ReadMemStats(&parked)
	atomic.StoreUint64(&taskParkRelease, 1)
	completed := <-sessionDone
	b.StopTimer()
	if completed != 10000 {
		b.Fatalf("completed %d tasks, want 10000", completed)
	}
	b.ReportMetric(float64(memoryDelta(parked.HeapAlloc, before.HeapAlloc))/10000,
		"live-heap-B/task")
	b.ReportMetric(float64(memoryDelta(parked.HeapObjects, before.HeapObjects))/10000,
		"live-objects/task")
	b.ReportMetric(float64(memoryDelta(parked.StackInuse, before.StackInuse))/10000,
		"live-stack-B/task")
}

func memoryDelta(after, before uint64) uint64 {
	if after < before {
		return 0
	}
	return after - before
}

func BenchmarkChannelRoundTrip(b *testing.B) {
	b.ReportAllocs()
	intSink = channelRoundTrips(b.N)
}

func BenchmarkReadySelect(b *testing.B) {
	b.ReportAllocs()
	intSink = readySelects(b.N)
}

func BenchmarkSleepZero(b *testing.B) {
	b.ReportAllocs()
	if got := sleepLoop(b.N, 0); got != b.N {
		b.Fatalf("sleepLoop(%d, 0) = %d", b.N, got)
	}
}

func BenchmarkSleepNanosecond(b *testing.B) {
	b.ReportAllocs()
	if got := sleepLoop(b.N, time.Nanosecond); got != b.N {
		b.Fatalf("sleepLoop(%d, 1ns) = %d", b.N, got)
	}
}

func BenchmarkFileRead(b *testing.B) {
	file, err := os.Open("/dev/zero")
	if err != nil {
		b.Fatal(err)
	}
	defer file.Close()
	buffer := make([]byte, 1)
	b.ReportAllocs()
	b.ResetTimer()
	if got, err := fileReads(file, buffer, b.N); err != nil || got != b.N {
		b.Fatalf("fileReads(..., %d) = (%d, %v)", b.N, got, err)
	}
}

func BenchmarkTCPRead(b *testing.B) {
	reader, writer, cleanup := newTCPPair(b)
	defer cleanup()
	go writeTCP(writer)
	buffer := make([]byte, 1)
	b.ReportAllocs()
	b.ResetTimer()
	if got, err := tcpReads(reader, buffer, b.N); err != nil || got != b.N {
		b.Fatalf("tcpReads(..., %d) = (%d, %v)", b.N, got, err)
	}
}

func BenchmarkFileBlockingProgress(b *testing.B) {
	reader, writer, err := os.Pipe()
	if err != nil {
		b.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	watchdog := time.AfterFunc(30*time.Second, func() {
		reader.Close()
		writer.Close()
	})
	defer watchdog.Stop()
	b.ReportAllocs()
	b.ResetTimer()
	if got, err := blockingFileRoundTrips(reader, writer.Fd(), b.N); err != nil || got != b.N {
		b.Fatalf("blockingFileRoundTrips(..., %d) = (%d, %v)",
			b.N, got, err)
	}
}

func BenchmarkTCPBlockingProgress(b *testing.B) {
	reader, writer, cleanup := newTCPPair(b)
	defer cleanup()
	watchdog := time.AfterFunc(30*time.Second, cleanup)
	defer watchdog.Stop()
	writerFD := tcpFD(b, writer)
	b.ReportAllocs()
	b.ResetTimer()
	if got, err := blockingTCPRoundTrips(reader, writerFD, b.N); err != nil || got != b.N {
		b.Fatalf("blockingTCPRoundTrips(..., %d) = (%d, %v)",
			b.N, got, err)
	}
}

func BenchmarkCScalar(b *testing.B) {
	b.ReportAllocs()
	uintSink = cScalarCalls(b.N)
}

func BenchmarkCAggregate(b *testing.B) {
	b.ReportAllocs()
	uintSink, floatSink = cPairCalls(b.N)
}

func BenchmarkCErrno(b *testing.B) {
	b.ReportAllocs()
	value, err := cErrnoCalls(b.N)
	if err != nil {
		b.Fatal(err)
	}
	uintSink = uint64(value)
}

func BenchmarkCLibm(b *testing.B) {
	b.ReportAllocs()
	floatSink = cLibmCalls(b.N)
}

func BenchmarkCBlockingHandoff(b *testing.B) {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	b.ReportAllocs()
	b.ResetTimer()
	elapsed := cBlockingHandoffs(b.N)
	b.StopTimer()
	b.ReportMetric(float64(elapsed)/float64(b.N), "ns/progress")
}

func benchmarkCBlockingGroup(b *testing.B, calls int) {
	const timeout = 250 * time.Millisecond
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	_, timeouts := cBlockingGroup(1, calls, timeout)
	if timeouts != 0 {
		b.Skipf("%d concurrent blocking C calls exceeded executor capacity",
			calls)
	}
	b.ReportAllocs()
	b.ResetTimer()
	entryElapsed, timeouts := cBlockingGroup(b.N, calls, timeout)
	b.StopTimer()
	if timeouts != 0 {
		b.Fatalf("%d blocking C calls timed out", timeouts)
	}
	b.ReportMetric(float64(entryElapsed)/float64(b.N*calls),
		"entry-ns/call")
}

func BenchmarkCBlockingGroup3(b *testing.B) {
	benchmarkCBlockingGroup(b, 3)
}

func BenchmarkCBlockingGroup8(b *testing.B) {
	benchmarkCBlockingGroup(b, 8)
}

type fataler interface {
	Helper()
	Fatal(...any)
}

func tcpFD(t fataler, conn *net.TCPConn) uintptr {
	t.Helper()
	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
		return 0
	}
	var fd uintptr
	if err := raw.Control(func(value uintptr) {
		fd = value
	}); err != nil {
		t.Fatal(err)
		return 0
	}
	return fd
}

func newTCPPair(t fataler) (reader, writer *net.TCPConn, cleanup func()) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{
		IP: net.IPv4(127, 0, 0, 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan *net.TCPConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptTCP()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	reader, err = net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	select {
	case writer = <-accepted:
	case err := <-acceptErr:
		reader.Close()
		listener.Close()
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		reader.Close()
		writer.Close()
		t.Fatal(err)
	}
	return reader, writer, func() {
		reader.Close()
		writer.Close()
	}
}

func writeTCP(conn *net.TCPConn) {
	buffer := make([]byte, 32<<10)
	for {
		if _, err := conn.Write(buffer); err != nil {
			return
		}
	}
}
