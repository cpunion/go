// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin || linux

package corobench

import (
	"io"
	"net"
	"os"
)

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
