// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package corobench

import (
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
	runtime.GOMAXPROCS(old)
}

func TestProbeEdges(t *testing.T) {
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
	if got := channelRoundTrips(0); got != 0 {
		t.Errorf("channelRoundTrips(0) = %d, want 0", got)
	}
	if got := sleepLoop(0, 0); got != 0 {
		t.Errorf("sleepLoop(0, 0) = %d, want 0", got)
	}
	if got := cBlockingHandoffs(0); got != 0 {
		t.Errorf("cBlockingHandoffs(0) = %d, want 0", got)
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

	reader, _, cleanup := newTCPPair(t)
	if err := reader.Close(); err != nil {
		cleanup()
		t.Fatal(err)
	}
	if _, err := tcpReads(reader, make([]byte, 1), 1); err == nil {
		t.Error("tcpReads on a closed connection succeeded")
	}
	cleanup()
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

func BenchmarkRecursiveYield4096(b *testing.B) {
	b.ReportAllocs()
	total := 0
	for i := 0; i < b.N; i++ {
		total += recursiveYield(4096)
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

type fataler interface {
	Helper()
	Fatal(...any)
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
