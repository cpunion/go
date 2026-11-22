// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestOrdinaryIOCompletionAfterWait(t *testing.T) {
	const source = `package main

import (
	"os"
	"time"
)

func readFileAtSameSite(file *os.File, payload []byte) error {
	for i := range payload {
		n, err := file.Read(payload[i : i+1])
		if err != nil {
			return err
		}
		if n != 1 {
			panic("short read")
		}
	}
	return nil
}

func main() {
	for range 12 {
		reader, writer, err := os.Pipe()
		if err != nil {
			panic(err)
		}
		if err := reader.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			panic(err)
		}
		done := make(chan error, 1)
		go func() {
			// Let the first read pend, then make both bytes available in
			// one write. The second read completes at the same call site.
			time.Sleep(8 * time.Millisecond)
			_, err := writer.Write([]byte{2, 7})
			done <- err
		}()
		var payload [2]byte
		if err := readFileAtSameSite(reader, payload[:]); err != nil {
			panic(err)
		}
		if err := <-done; err != nil {
			panic(err)
		}
		reader.Close()
		writer.Close()
		if payload != [2]byte{2, 7} {
			panic("read result changed")
		}
	}
	println("switch-return-ok")
}
`
	names := []string{"readFileAtSameSite"}
	testSwitchReturnFilesNative(t, map[string]string{"main.go": source}, "", nil, names, names)
}
