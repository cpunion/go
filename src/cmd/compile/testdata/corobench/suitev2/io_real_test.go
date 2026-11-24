// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package suitev2

import (
	"io"
	"net"
	"os"
	"testing"
	"time"
)

const ioPayloadSize = 256

// Direct file/TCP full-read and full-write loops keep every I/O suspension
// directly on os.(*File).Read/Write and net.(*TCPConn).Read/Write so the
// compiler lowers them to coroReadStart/Finish and coroWriteStart/Finish
// without crossing unlowered io helper wrappers.

func fileReadFull(f *os.File, buf []byte) error {
	for off := 0; off < len(buf); {
		n, err := f.Read(buf[off:])
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

func fileWriteFull(f *os.File, buf []byte) error {
	for off := 0; off < len(buf); {
		n, err := f.Write(buf[off:])
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

func tcpReadFull(c *net.TCPConn, buf []byte) error {
	for off := 0; off < len(buf); {
		n, err := c.Read(buf[off:])
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

func tcpWriteFull(c *net.TCPConn, buf []byte) error {
	for off := 0; off < len(buf); {
		n, err := c.Write(buf[off:])
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

type pipePair struct {
	reqR, reqW   *os.File
	respR, respW *os.File
}

func newPipePairs(tb testing.TB, count int) ([]pipePair, func()) {
	tb.Helper()
	pairs := make([]pipePair, count)
	for i := range pairs {
		rr, rw, err := os.Pipe()
		if err != nil {
			tb.Fatal(err)
		}
		sr, sw, err := os.Pipe()
		if err != nil {
			rr.Close()
			rw.Close()
			tb.Fatal(err)
		}
		pairs[i] = pipePair{reqR: rr, reqW: rw, respR: sr, respW: sw}
	}
	cleanup := func() {
		for _, p := range pairs {
			p.reqR.Close()
			p.reqW.Close()
			p.respR.Close()
			p.respW.Close()
		}
	}
	return pairs, cleanup
}

func pipeEchoServer(reqR, respW *os.File, rounds int, done chan error) {
	var buf [ioPayloadSize]byte
	for i := 0; i < rounds; i++ {
		if err := fileReadFull(reqR, buf[:]); err != nil {
			done <- err
			return
		}
		buf[0] ^= 0x5a
		if err := fileWriteFull(respW, buf[:]); err != nil {
			done <- err
			return
		}
	}
	done <- nil
}

func pipeEchoClient(reqW, respR *os.File, id, rounds int, latencies []int64, done chan error) {
	var sendBuf, recvBuf [ioPayloadSize]byte
	sendBuf[0] = byte(id)
	for i := 0; i < rounds; i++ {
		sendBuf[1] = byte(i)
		start := time.Now()
		if err := fileWriteFull(reqW, sendBuf[:]); err != nil {
			done <- err
			return
		}
		if err := fileReadFull(respR, recvBuf[:]); err != nil {
			done <- err
			return
		}
		latencies[i] = time.Since(start).Nanoseconds()
		if recvBuf[0] != sendBuf[0]^0x5a || recvBuf[1] != sendBuf[1] {
			done <- io.ErrUnexpectedEOF
			return
		}
	}
	done <- nil
}

func runPipeRoundTrips(pairs []pipePair, roundsPerPair int, latencies [][]int64) error {
	n := len(pairs)
	done := make(chan error, n*2)
	for i := 0; i < n; i++ {
		p := pairs[i]
		go pipeEchoServer(p.reqR, p.respW, roundsPerPair, done)
		go pipeEchoClient(p.reqW, p.respR, i, roundsPerPair, latencies[i], done)
	}
	var firstErr error
	for i := 0; i < n*2; i++ {
		if err := <-done; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

type tcpPair struct {
	client *net.TCPConn
	server *net.TCPConn
}

func newTCPPairs(tb testing.TB, count int) ([]tcpPair, func()) {
	tb.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		tb.Fatal(err)
	}
	defer listener.Close()
	addr := listener.Addr().(*net.TCPAddr)
	pairs := make([]tcpPair, count)
	for i := range pairs {
		accepted := make(chan *net.TCPConn, 1)
		acceptErr := make(chan error, 1)
		go func() {
			c, err := listener.AcceptTCP()
			if err != nil {
				acceptErr <- err
				return
			}
			accepted <- c
		}()
		client, err := net.DialTCP("tcp4", nil, addr)
		if err != nil {
			tb.Fatal(err)
		}
		_ = client.SetNoDelay(true)
		var server *net.TCPConn
		select {
		case server = <-accepted:
		case err := <-acceptErr:
			client.Close()
			tb.Fatal(err)
		}
		_ = server.SetNoDelay(true)
		pairs[i] = tcpPair{client: client, server: server}
	}
	cleanup := func() {
		for _, p := range pairs {
			p.client.Close()
			p.server.Close()
		}
	}
	return pairs, cleanup
}

func tcpEchoServer(conn *net.TCPConn, rounds int, done chan error) {
	var buf [ioPayloadSize]byte
	for i := 0; i < rounds; i++ {
		if err := tcpReadFull(conn, buf[:]); err != nil {
			done <- err
			return
		}
		buf[0] ^= 0xa5
		if err := tcpWriteFull(conn, buf[:]); err != nil {
			done <- err
			return
		}
	}
	done <- nil
}

func tcpEchoClient(conn *net.TCPConn, id, rounds int, latencies []int64, done chan error) {
	var sendBuf, recvBuf [ioPayloadSize]byte
	sendBuf[0] = byte(id)
	for i := 0; i < rounds; i++ {
		sendBuf[1] = byte(i)
		start := time.Now()
		if err := tcpWriteFull(conn, sendBuf[:]); err != nil {
			done <- err
			return
		}
		if err := tcpReadFull(conn, recvBuf[:]); err != nil {
			done <- err
			return
		}
		latencies[i] = time.Since(start).Nanoseconds()
		if recvBuf[0] != sendBuf[0]^0xa5 || recvBuf[1] != sendBuf[1] {
			done <- io.ErrUnexpectedEOF
			return
		}
	}
	done <- nil
}

func runTCPEchoRounds(pairs []tcpPair, roundsPerConn int, latencies [][]int64) error {
	n := len(pairs)
	done := make(chan error, n*2)
	for i := 0; i < n; i++ {
		p := pairs[i]
		go tcpEchoServer(p.server, roundsPerConn, done)
		go tcpEchoClient(p.client, i, roundsPerConn, latencies[i], done)
	}
	var firstErr error
	for i := 0; i < n*2; i++ {
		if err := <-done; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

type tcpFanoutReply struct {
	sum uint64
	err error
}

func tcpFanoutBackend(conn *net.TCPConn, totalRequests int, done chan error) {
	var buf [ioPayloadSize]byte
	for i := 0; i < totalRequests; i++ {
		if err := tcpReadFull(conn, buf[:]); err != nil {
			done <- err
			return
		}
		buf[0] ^= 0x3c
		if err := tcpWriteFull(conn, buf[:]); err != nil {
			done <- err
			return
		}
	}
	done <- nil
}

func tcpFanoutLeaf(conn *net.TCPConn, reqID, leafID int, replies chan tcpFanoutReply) {
	var sendBuf, recvBuf [ioPayloadSize]byte
	sendBuf[0] = byte(reqID + leafID)
	sendBuf[1] = byte(leafID)
	if err := tcpWriteFull(conn, sendBuf[:]); err != nil {
		replies <- tcpFanoutReply{err: err}
		return
	}
	if err := tcpReadFull(conn, recvBuf[:]); err != nil {
		replies <- tcpFanoutReply{err: err}
		return
	}
	if recvBuf[0] != sendBuf[0]^0x3c || recvBuf[1] != sendBuf[1] {
		replies <- tcpFanoutReply{err: io.ErrUnexpectedEOF}
		return
	}
	replies <- tcpFanoutReply{sum: uint64(recvBuf[0]) + uint64(recvBuf[1])}
}

func runTCPFanoutBatches(pairs []tcpPair, batches int, batchLatencies []int64) (uint64, error) {
	fanout := len(pairs)
	serverDone := make(chan error, fanout)
	for i := 0; i < fanout; i++ {
		go tcpFanoutBackend(pairs[i].server, batches, serverDone)
	}
	replies := make(chan tcpFanoutReply, fanout)
	var total uint64
	var firstErr error
	for r := 0; r < batches; r++ {
		start := time.Now()
		for i := 0; i < fanout; i++ {
			go tcpFanoutLeaf(pairs[i].client, r, i, replies)
		}
		for i := 0; i < fanout; i++ {
			rep := <-replies
			if rep.err != nil && firstErr == nil {
				firstErr = rep.err
			}
			total += rep.sum
		}
		batchLatencies[r] = time.Since(start).Nanoseconds()
	}
	for i := 0; i < fanout; i++ {
		if err := <-serverDone; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return total, firstErr
}

func BenchmarkRealIOPipeRoundTrip16(b *testing.B) {
	const workers = 16
	pairs, cleanup := newPipePairs(b, workers)
	defer cleanup()

	latGrid := make([][]int64, workers)
	flat := make([]int64, workers*b.N)
	for i := range latGrid {
		latGrid[i] = flat[i*b.N : (i+1)*b.N]
	}
	rssBefore, _ := processRSS()
	b.ReportAllocs()
	b.ResetTimer()
	if err := runPipeRoundTrips(pairs, b.N, latGrid); err != nil {
		b.Fatal(err)
	}
	b.StopTimer()
	rssAfter, rssPeak := processRSS()
	reportBenchMetrics(b, flat, workers*b.N, rssBefore, rssAfter, rssPeak)
}

func BenchmarkRealIOTCPEcho16(b *testing.B) {
	const conns = 16
	pairs, cleanup := newTCPPairs(b, conns)
	defer cleanup()

	latGrid := make([][]int64, conns)
	flat := make([]int64, conns*b.N)
	for i := range latGrid {
		latGrid[i] = flat[i*b.N : (i+1)*b.N]
	}
	rssBefore, _ := processRSS()
	b.ReportAllocs()
	b.ResetTimer()
	if err := runTCPEchoRounds(pairs, b.N, latGrid); err != nil {
		b.Fatal(err)
	}
	b.StopTimer()
	rssAfter, rssPeak := processRSS()
	reportBenchMetrics(b, flat, conns*b.N, rssBefore, rssAfter, rssPeak)
}

func BenchmarkRealIOTCPFanout8(b *testing.B) {
	const fanout = 8
	pairs, cleanup := newTCPPairs(b, fanout)
	defer cleanup()

	latencies := make([]int64, b.N)
	rssBefore, _ := processRSS()
	b.ReportAllocs()
	b.ResetTimer()
	sum, err := runTCPFanoutBatches(pairs, b.N, latencies)
	b.StopTimer()
	if err != nil {
		b.Fatal(err)
	}
	sinkUint64 = sum
	rssAfter, rssPeak := processRSS()
	reportBenchMetrics(b, latencies, fanout*b.N, rssBefore, rssAfter, rssPeak)
}
