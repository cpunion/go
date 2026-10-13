// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin || linux

package corobench

import (
	"io"
	"net"
	"os"
	"sync/atomic"
)

func fileWrite(file *os.File, buffer []byte) (int, error) {
	n, err := file.Write(buffer)
	return n, err
}

func tcpWrite(conn *net.TCPConn, buffer []byte) (int, error) {
	n, err := conn.Write(buffer)
	return n, err
}

func udpWrite(conn *net.UDPConn, buffer []byte) (int, error) {
	n, err := conn.Write(buffer)
	return n, err
}

func fileWriteDiscard(file *os.File, buffer []byte) {
	file.Write(buffer)
	_, _ = file.Write(buffer)
}

func fileWrites(file *os.File, buffer []byte, iterations int) (int, error) {
	total := 0
	for i := 0; i < iterations; i++ {
		n, err := file.Write(buffer)
		total += n
		if err != nil {
			return total, err
		}
		if n != len(buffer) {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func tcpWrites(conn *net.TCPConn, buffer []byte, iterations int) (int, error) {
	total := 0
	for i := 0; i < iterations; i++ {
		n, err := conn.Write(buffer)
		total += n
		if err != nil {
			return total, err
		}
		if n != len(buffer) {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

type writeResult struct {
	n   int
	err error
}

type blockingWriteState struct {
	file      *os.File
	tcp       *net.TCPConn
	buffer    []byte
	entered   uint64
	completed uint64
	done      chan writeResult
	ready     chan struct{}
	finished  chan struct{}
	first     writeResult
	second    writeResult
}

var blockingWriteProbe *blockingWriteState

func blockingFileWrite() {
	state := blockingWriteProbe
	buffer := state.buffer
	atomic.AddUint64(&state.entered, 1)
	n, err := state.file.Write(buffer)
	atomic.AddUint64(&state.completed, 1)
	state.done <- writeResult{n, err}
}

func blockingTCPWrite() {
	state := blockingWriteProbe
	buffer := state.buffer
	atomic.AddUint64(&state.entered, 1)
	n, err := state.tcp.Write(buffer)
	atomic.AddUint64(&state.completed, 1)
	state.done <- writeResult{n, err}
}

func startFileWrites(state *blockingWriteState) {
	blockingWriteProbe = state
	go blockingFileWrite()
	waitForEpoch(&state.entered, 1)
	go blockingFileWrite()
	waitForEpoch(&state.entered, 2)
	state.ready <- struct{}{}
	first := <-state.done
	second := <-state.done
	state.first = first
	state.second = second
	state.finished <- struct{}{}
}

func startTCPWrites(state *blockingWriteState) {
	blockingWriteProbe = state
	go blockingTCPWrite()
	waitForEpoch(&state.entered, 1)
	go blockingTCPWrite()
	waitForEpoch(&state.entered, 2)
	state.ready <- struct{}{}
	first := <-state.done
	second := <-state.done
	state.first = first
	state.second = second
	state.finished <- struct{}{}
}
