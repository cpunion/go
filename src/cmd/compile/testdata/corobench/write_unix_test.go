// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package corobench

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublicFileWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "write")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	if n, err := fileWrite(file, []byte("write")); n != 5 || err != nil {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
	if n, err := fileWrite(file, nil); n != 0 || err != nil {
		t.Fatalf("empty Write = (%d, %v), want (0, nil)", n, err)
	}
	fileWriteDiscard(file, []byte("!"))
	if n, err := fileWrites(file, []byte("!"), 2); n != 2 || err != nil {
		t.Fatalf("fileWrites = (%d, %v), want (2, nil)", n, err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "write!!!!" {
		t.Fatalf("file contents = (%q, %v), want write!!!!", data, err)
	}
	if n, err := fileWrites(file, nil, 1); n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Errorf("closed fileWrites = (%d, %v), want closed error", n, err)
	}
	if n, err := fileWrites(nil, nil, 0); n != 0 || err != nil {
		t.Errorf("zero-iteration fileWrites = (%d, %v), want (0, nil)", n, err)
	}
	for _, buffer := range [][]byte{nil, []byte("x")} {
		if n, err := fileWrite(file, buffer); n != 0 || !errors.Is(err, os.ErrClosed) {
			t.Errorf("closed Write = (%d, %v), want closed error", n, err)
		}
		if n, err := fileWrite(nil, buffer); n != 0 || !errors.Is(err, os.ErrInvalid) {
			t.Errorf("nil Write = (%d, %v), want invalid error", n, err)
		}
	}
	readonly, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	for _, buffer := range [][]byte{nil, []byte("x")} {
		n, err := fileWrite(readonly, buffer)
		var pathErr *os.PathError
		if n != 0 || !errors.As(err, &pathErr) || pathErr.Op != "write" || pathErr.Path != path {
			t.Errorf("read-only Write = (%d, %v), want write PathError", n, err)
		}
	}
}

func TestPublicTCPWrite(t *testing.T) {
	reader, writer, cleanup := newTCPPair(t)
	defer cleanup()
	if n, err := tcpWrite(writer, nil); n != 0 || err != nil {
		t.Fatalf("empty Write = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := tcpWrite(writer, []byte("write")); n != 5 || err != nil {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
	if n, err := tcpWrites(writer, []byte("!"), 2); n != 2 || err != nil {
		t.Fatalf("tcpWrites = (%d, %v), want (2, nil)", n, err)
	}
	buffer := make([]byte, 7)
	if _, err := io.ReadFull(reader, buffer); err != nil || string(buffer) != "write!!" {
		t.Fatalf("ReadFull = (%q, %v), want write!!", buffer, err)
	}
	writer.Close()
	if n, err := tcpWrites(writer, nil, 1); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Errorf("closed tcpWrites = (%d, %v), want closed error", n, err)
	}
	if n, err := tcpWrites(nil, nil, 0); n != 0 || err != nil {
		t.Errorf("zero-iteration tcpWrites = (%d, %v), want (0, nil)", n, err)
	}
	for _, buffer := range [][]byte{nil, []byte("x")} {
		n, err := tcpWrite(writer, buffer)
		var opErr *net.OpError
		if n != 0 || !errors.Is(err, net.ErrClosed) || !errors.As(err, &opErr) || opErr.Op != "write" {
			t.Errorf("closed Write = (%d, %v), want write OpError", n, err)
		}
		if n, err := tcpWrite(new(net.TCPConn), buffer); n != 0 || err == nil {
			t.Errorf("zero connection Write = (%d, %v), want error", n, err)
		}
	}
}

func TestPublicUDPWrite(t *testing.T) {
	reader, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	writer, err := net.DialUDP("udp4", nil, reader.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := reader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, message := range [][]byte{nil, []byte("datagram")} {
		if n, err := udpWrite(writer, message); n != len(message) || err != nil {
			t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(message))
		}
		buffer := make([]byte, 32)
		n, _, err := reader.ReadFromUDP(buffer)
		if err != nil || !bytes.Equal(buffer[:n], message) {
			t.Fatalf("ReadFromUDP = (%q, %v), want %q", buffer[:n], err, message)
		}
	}
}

// publicWritePair shares the setup between progress and deadline probes. The
// coroutine entry itself remains a statically resolved File or TCPConn method.
type publicWritePair struct {
	reader   io.Reader
	file     *os.File
	tcp      *net.TCPConn
	deadline func(time.Time) error
	close    func()
}

func newPublicWritePair(t *testing.T, network bool) publicWritePair {
	t.Helper()
	if network {
		reader, writer, cleanup := newTCPPair(t)
		t.Cleanup(cleanup)
		if err := writer.SetWriteBuffer(4096); err != nil {
			t.Fatal(err)
		}
		if err := reader.SetReadBuffer(4096); err != nil {
			t.Fatal(err)
		}
		return publicWritePair{reader: reader, tcp: writer, deadline: writer.SetWriteDeadline, close: cleanup}
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() { reader.Close(); writer.Close() }
	t.Cleanup(cleanup)
	return publicWritePair{reader: reader, file: writer, deadline: writer.SetWriteDeadline, close: cleanup}
}

func TestPublicWriteBlockingProgress(t *testing.T) {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	for _, network := range []bool{false, true} {
		name := "file"
		if network {
			name = "tcp"
		}
		t.Run(name, func(t *testing.T) {
			pair := newPublicWritePair(t, network)
			watchdog := time.AfterFunc(20*time.Second, pair.close)
			defer watchdog.Stop()
			const size = 256 << 10
			state := &blockingWriteState{
				file: pair.file, tcp: pair.tcp,
				buffer: bytes.Repeat([]byte{0x5a}, size),
				done:   make(chan writeResult, 2), ready: make(chan struct{}, 1),
				finished: make(chan struct{}, 1),
			}
			if network {
				go startTCPWrites(state)
			} else {
				go startFileWrites(state)
			}
			<-state.ready
			// Both writers have reached their operation. The second must not
			// hold the logical executor while waiting for the FD write lock.
			if completed := atomic.LoadUint64(&state.completed); completed != 0 {
				t.Fatalf("%d writes completed before the reader", completed)
			}
			state.buffer = nil
			runtime.GC()
			data := make([]byte, 2*size)
			if _, err := io.ReadFull(pair.reader, data); err != nil {
				t.Fatal(err)
			}
			<-state.finished
			for _, result := range []writeResult{state.first, state.second} {
				if result.n != size || result.err != nil {
					t.Errorf("Write = (%d, %v), want (%d, nil)", result.n, result.err, size)
				}
			}
			if !bytes.Equal(data, bytes.Repeat([]byte{0x5a}, len(data))) {
				t.Error("write data changed while parked across GC")
			}
			if !watchdog.Stop() {
				t.Fatal("writers blocked sibling progress")
			}
		})
	}
}

func TestPublicWriteDeadline(t *testing.T) {
	for _, network := range []bool{false, true} {
		name := "file"
		if network {
			name = "tcp"
		}
		t.Run(name, func(t *testing.T) {
			pair := newPublicWritePair(t, network)
			buffer := make([]byte, 1<<20)
			for _, expired := range []bool{true, false} {
				deadline := time.Now().Add(75 * time.Millisecond)
				if expired {
					deadline = time.Now().Add(-time.Second)
				}
				if err := pair.deadline(deadline); err != nil {
					t.Fatal(err)
				}
				var n int
				var err error
				if network {
					n, err = tcpWrite(pair.tcp, buffer)
				} else {
					n, err = fileWrite(pair.file, buffer)
				}
				if !errors.Is(err, os.ErrDeadlineExceeded) || n < 0 || n >= len(buffer) {
					t.Fatalf("Write = (%d, %v), want incomplete write with deadline error", n, err)
				}
				if expired && n != 0 || !expired && n == 0 {
					t.Errorf("expired=%v: Write count = %d, want bytes only before a future deadline", expired, n)
				}
			}
		})
	}
}

func BenchmarkFileWrite(b *testing.B) {
	file, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer file.Close()
	buffer := make([]byte, 64)
	b.SetBytes(int64(len(buffer)))
	b.ReportAllocs()
	b.ResetTimer()
	if n, err := fileWrites(file, buffer, b.N); n != b.N*len(buffer) || err != nil {
		b.Fatalf("fileWrites = (%d, %v), want (%d, nil)", n, err, b.N*len(buffer))
	}
}

// BenchmarkTCPWrite measures small-write throughput with a continuously
// draining peer, not the latency of a write parked behind backpressure.
func BenchmarkTCPWrite(b *testing.B) {
	reader, writer, cleanup := newTCPPair(b)
	done := make(chan struct{})
	go func() {
		io.Copy(io.Discard, reader)
		close(done)
	}()
	defer func() {
		cleanup()
		<-done
	}()
	buffer := make([]byte, 64)
	b.SetBytes(int64(len(buffer)))
	b.ReportAllocs()
	b.ResetTimer()
	if n, err := tcpWrites(writer, buffer, b.N); n != b.N*len(buffer) || err != nil {
		b.Fatalf("tcpWrites = (%d, %v), want (%d, nil)", n, err, b.N*len(buffer))
	}
}
