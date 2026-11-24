// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

// TestDirectIOFinishResetAcrossLoop verifies that reusing the same static I/O
// call site across a suspension followed by a synchronous completion clears
// the pending direct-finish flag so CoroReadFinish/CoroWriteFinish never
// unlocks poll.fdMutex a second time.
func TestDirectIOFinishResetAcrossLoop(t *testing.T) {
	const source = `package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"runtime"
	"time"
)

//go:noinline
func readFilePair(r *os.File, buf []byte) int {
	sum := 0
	for i := 0; i < 2; i++ {
		n, err := r.Read(buf[:1])
		if n != 1 || err != nil {
			panic("readFilePair failed")
		}
		sum += int(buf[0])
	}
	return sum
}

//go:noinline
func writeFilePair(w *os.File, chunks [][]byte) int {
	total := 0
	for i := 0; i < len(chunks); i++ {
		n, err := w.Write(chunks[i])
		if err != nil || n != len(chunks[i]) {
			panic("writeFilePair failed")
		}
		total += n
	}
	return total
}

//go:noinline
func readConnPair(c *net.TCPConn, buf []byte) int {
	sum := 0
	for i := 0; i < 2; i++ {
		n, err := c.Read(buf[:1])
		if n != 1 || err != nil {
			panic("readConnPair failed")
		}
		sum += int(buf[0])
	}
	return sum
}

//go:noinline
func writeConnPair(c *net.TCPConn, chunks [][]byte) int {
	total := 0
	for i := 0; i < len(chunks); i++ {
		n, err := c.Write(chunks[i])
		if err != nil || n != len(chunks[i]) {
			panic("writeConnPair failed")
		}
		total += n
	}
	return total
}

func checkFileRead() {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	defer r.Close()
	defer w.Close()
	go func() {
		time.Sleep(15 * time.Millisecond)
		if _, err := w.Write([]byte("ab")); err != nil {
			panic(err)
		}
	}()
	if sum := readFilePair(r, make([]byte, 1)); sum != int('a'+'b') {
		panic(sum)
	}
	// Verify fdMutex remains clean for subsequent operations.
	if _, err := w.Write([]byte("c")); err != nil {
		panic(err)
	}
	var tail [1]byte
	if n, err := r.Read(tail[:]); n != 1 || err != nil || tail[0] != 'c' {
		panic("post-loop file read failed")
	}
}

func checkFileWrite() {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	defer r.Close()
	defer w.Close()
	// First chunk exceeds pipe buffer (16-64 KiB) so iteration 0 suspends on
	// EAGAIN; after the reader drains it, iteration 1 completes synchronously.
	big := bytes.Repeat([]byte{'w'}, 256<<10)
	small := []byte("ok")
	done := make(chan error, 1)
	go func() {
		time.Sleep(15 * time.Millisecond)
		got := make([]byte, len(big)+len(small))
		if _, err := io.ReadFull(r, got); err != nil {
			done <- err
			return
		}
		if !bytes.Equal(got[:len(big)], big) || !bytes.Equal(got[len(big):], small) {
			done <- io.ErrUnexpectedEOF
			return
		}
		done <- nil
	}()
	if n := writeFilePair(w, [][]byte{big, small}); n != len(big)+len(small) {
		panic(n)
	}
	if err := <-done; err != nil {
		panic(err)
	}
	// Verify fdMutex remains clean for subsequent writes.
	tailDone := make(chan error, 1)
	go func() {
		var tail [1]byte
		_, err := io.ReadFull(r, tail[:])
		tailDone <- err
	}()
	if n, err := w.Write([]byte("z")); n != 1 || err != nil {
		panic("post-loop file write failed")
	}
	if err := <-tailDone; err != nil {
		panic(err)
	}
}

func checkConnRead() {
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		panic(err)
	}
	defer ln.Close()
	errCh := make(chan error, 1)
	go func() {
		srv, err := ln.AcceptTCP()
		if err != nil {
			errCh <- err
			return
		}
		defer srv.Close()
		time.Sleep(15 * time.Millisecond)
		_, err = srv.Write([]byte("xy"))
		errCh <- err
	}()
	cli, err := net.DialTCP("tcp4", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		panic(err)
	}
	defer cli.Close()
	if sum := readConnPair(cli, make([]byte, 1)); sum != int('x'+'y') {
		panic(sum)
	}
	if err := <-errCh; err != nil {
		panic(err)
	}
}

func checkConnWrite() {
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		panic(err)
	}
	defer ln.Close()
	// First chunk exceeds 64 KiB socket buffers so iteration 0 suspends on
	// EAGAIN; after peer drains, iteration 1 completes synchronously.
	big := bytes.Repeat([]byte{'t'}, 1<<20)
	small := []byte("ok")
	done := make(chan error, 1)
	tailDone := make(chan error, 1)
	go func() {
		srv, err := ln.AcceptTCP()
		if err != nil {
			done <- err
			tailDone <- err
			return
		}
		defer srv.Close()
		_ = srv.SetReadBuffer(64 << 10)
		time.Sleep(15 * time.Millisecond)
		got := make([]byte, len(big)+len(small))
		if _, err := io.ReadFull(srv, got); err != nil {
			done <- err
			tailDone <- err
			return
		}
		if !bytes.Equal(got[:len(big)], big) || !bytes.Equal(got[len(big):], small) {
			done <- io.ErrUnexpectedEOF
			tailDone <- io.ErrUnexpectedEOF
			return
		}
		done <- nil
		var tail [1]byte
		_, err = io.ReadFull(srv, tail[:])
		tailDone <- err
	}()
	cli, err := net.DialTCP("tcp4", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		panic(err)
	}
	defer cli.Close()
	_ = cli.SetWriteBuffer(64 << 10)
	if n := writeConnPair(cli, [][]byte{big, small}); n != len(big)+len(small) {
		panic(n)
	}
	if err := <-done; err != nil {
		panic(err)
	}
	// Verify fdMutex remains clean for subsequent writes.
	if n, err := cli.Write([]byte("!")); n != 1 || err != nil {
		panic("post-loop conn write failed")
	}
	if err := <-tailDone; err != nil {
		panic(err)
	}
}

func main() {
	for _, procs := range []int{1, 4} {
		runtime.GOMAXPROCS(procs)
		for range 3 {
			checkFileRead()
			checkFileWrite()
			checkConnRead()
			checkConnWrite()
		}
	}
	println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, nil, []string{"readFilePair", "writeFilePair", "readConnPair", "writeConnPair"})
}
