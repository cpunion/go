// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin || linux

package corobench

import (
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var errBlockingIONoProgress = errors.New("blocking I/O sibling did not progress")
var errContendedIOData = errors.New("contended I/O returned unexpected data")

func fileReads(file *os.File, buffer []byte, iterations int) (int, error) {
	total := 0
	for i := 0; i < iterations; i++ {
		n, err := file.Read(buffer)
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
	return total, nil
}

func tcpReads(conn *net.TCPConn, buffer []byte, iterations int) (int, error) {
	total := 0
	for i := 0; i < iterations; i++ {
		n, err := conn.Read(buffer)
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
	return total, nil
}

type blockingIOState struct {
	entered    uint64
	progressed uint64
	readN      int
	writeN     int
	readErr    error
	writeErr   error
	done       chan struct{}
}

func blockingIOError(state *blockingIOState, epoch uint64) error {
	if state.writeErr != nil {
		return state.writeErr
	}
	if state.readErr != nil {
		return state.readErr
	}
	if state.writeN != 1 {
		return io.ErrShortWrite
	}
	if state.readN != 1 {
		return io.ErrNoProgress
	}
	if atomic.LoadUint64(&state.progressed) < epoch {
		return errBlockingIONoProgress
	}
	return nil
}

func waitForEpoch(value *uint64, epoch uint64) {
	for atomic.LoadUint64(value) < epoch {
		runtime.Gosched()
	}
}

type contendedReadResult struct {
	n     int
	value byte
	err   error
}

type contendedReadState struct {
	entered uint64
	done    chan contendedReadResult
}

func validateContendedReads(results [2]contendedReadResult) error {
	var first, second bool
	for _, result := range results {
		if result.err != nil {
			return result.err
		}
		if result.n != 1 {
			return io.ErrNoProgress
		}
		switch result.value {
		case '1':
			first = true
		case '2':
			second = true
		default:
			return errContendedIOData
		}
	}
	if !first || !second {
		return errContendedIOData
	}
	return nil
}

func writeContendedBytes(fd uintptr) error {
	if n, err := writeProbeByte(fd, '1'); err != nil {
		return err
	} else if n != 1 {
		return io.ErrShortWrite
	}
	if n, err := writeProbeByte(fd, '2'); err != nil {
		return err
	} else if n != 1 {
		return io.ErrShortWrite
	}
	return nil
}

func writeProbeByte(fd uintptr, value byte) (int, error) {
	buffer := [1]byte{value}
	written, _, errno := syscall.Syscall(syscall.SYS_WRITE, fd,
		uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	runtime.KeepAlive(&buffer)
	if errno != 0 {
		return int(written), errno
	}
	return int(written), nil
}

var blockingFileProbe struct {
	state    *blockingIOState
	reader   *os.File
	writerFD uintptr
	epoch    uint64
}

var contendedFileProbe struct {
	state  *contendedReadState
	reader *os.File
}

func contendedFileRead(index uint64) {
	state := contendedFileProbe.state
	var buffer [1]byte
	atomic.StoreUint64(&state.entered, index+1)
	n, err := contendedFileProbe.reader.Read(buffer[:])
	state.done <- contendedReadResult{n: n, value: buffer[0], err: err}
}

func contendedFileRead1() {
	contendedFileRead(0)
}

func contendedFileRead2() {
	contendedFileRead(1)
}

func contendedFileReads(reader *os.File, writerFD uintptr) error {
	state := &contendedReadState{done: make(chan contendedReadResult, 2)}
	contendedFileProbe.state = state
	contendedFileProbe.reader = reader
	go contendedFileRead1()
	waitForEpoch(&state.entered, 1)
	go contendedFileRead2()
	waitForEpoch(&state.entered, 2)
	err := writeContendedBytes(writerFD)
	if err == nil {
		first := <-state.done
		second := <-state.done
		results := [2]contendedReadResult{first, second}
		err = validateContendedReads(results)
	}
	return err
}

func blockingFileRead() {
	state := blockingFileProbe.state
	epoch := blockingFileProbe.epoch
	var buffer [1]byte
	atomic.StoreUint64(&state.entered, epoch)
	n, err := blockingFileProbe.reader.Read(buffer[:])
	state.readN = n
	state.readErr = err
	state.done <- struct{}{}
}

func blockingFileRelease() {
	state := blockingFileProbe.state
	epoch := blockingFileProbe.epoch
	time.Sleep(0)
	atomic.StoreUint64(&state.progressed, epoch)
	n, err := writeProbeByte(blockingFileProbe.writerFD, byte(epoch))
	state.writeN = n
	state.writeErr = err
	state.done <- struct{}{}
}

func blockingFileRoundTrips(reader *os.File, writerFD uintptr, iterations int) (int, error) {
	if iterations <= 0 {
		return 0, nil
	}
	state := &blockingIOState{done: make(chan struct{}, 2)}
	blockingFileProbe.state = state
	blockingFileProbe.reader = reader
	blockingFileProbe.writerFD = writerFD
	for iteration := 0; iteration < iterations; iteration++ {
		epoch := uint64(iteration + 1)
		blockingFileProbe.epoch = epoch
		go blockingFileRead()
		waitForEpoch(&state.entered, epoch)
		go blockingFileRelease()
		<-state.done
		<-state.done
		if err := blockingIOError(state, epoch); err != nil {
			return iteration, err
		}
	}
	return iterations, nil
}

var blockingTCPProbe struct {
	state    *blockingIOState
	reader   *net.TCPConn
	writerFD uintptr
	epoch    uint64
}

var contendedTCPProbe struct {
	state  *contendedReadState
	reader *net.TCPConn
}

func contendedTCPRead(index uint64) {
	state := contendedTCPProbe.state
	var buffer [1]byte
	atomic.StoreUint64(&state.entered, index+1)
	n, err := contendedTCPProbe.reader.Read(buffer[:])
	state.done <- contendedReadResult{n: n, value: buffer[0], err: err}
}

func contendedTCPRead1() {
	contendedTCPRead(0)
}

func contendedTCPRead2() {
	contendedTCPRead(1)
}

func contendedTCPReads(reader *net.TCPConn, writerFD uintptr) error {
	state := &contendedReadState{done: make(chan contendedReadResult, 2)}
	contendedTCPProbe.state = state
	contendedTCPProbe.reader = reader
	go contendedTCPRead1()
	waitForEpoch(&state.entered, 1)
	go contendedTCPRead2()
	waitForEpoch(&state.entered, 2)
	err := writeContendedBytes(writerFD)
	if err == nil {
		first := <-state.done
		second := <-state.done
		results := [2]contendedReadResult{first, second}
		err = validateContendedReads(results)
	}
	return err
}

func blockingTCPRead() {
	state := blockingTCPProbe.state
	epoch := blockingTCPProbe.epoch
	var buffer [1]byte
	atomic.StoreUint64(&state.entered, epoch)
	n, err := blockingTCPProbe.reader.Read(buffer[:])
	state.readN = n
	state.readErr = err
	state.done <- struct{}{}
}

func blockingTCPRelease() {
	state := blockingTCPProbe.state
	epoch := blockingTCPProbe.epoch
	time.Sleep(0)
	atomic.StoreUint64(&state.progressed, epoch)
	n, err := writeProbeByte(blockingTCPProbe.writerFD, byte(epoch))
	state.writeN = n
	state.writeErr = err
	state.done <- struct{}{}
}

func blockingTCPRoundTrips(reader *net.TCPConn, writerFD uintptr, iterations int) (int, error) {
	if iterations <= 0 {
		return 0, nil
	}
	state := &blockingIOState{done: make(chan struct{}, 2)}
	blockingTCPProbe.state = state
	blockingTCPProbe.reader = reader
	blockingTCPProbe.writerFD = writerFD
	for iteration := 0; iteration < iterations; iteration++ {
		epoch := uint64(iteration + 1)
		blockingTCPProbe.epoch = epoch
		go blockingTCPRead()
		waitForEpoch(&state.entered, epoch)
		go blockingTCPRelease()
		<-state.done
		<-state.done
		if err := blockingIOError(state, epoch); err != nil {
			return iteration, err
		}
	}
	return iterations, nil
}
