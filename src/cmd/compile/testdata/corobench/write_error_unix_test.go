// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package corobench

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
)

// Broken pipes other than standard output and standard error return EPIPE;
// they must not terminate the program with SIGPIPE. The public adapters must
// preserve both the underlying errno and the normal error wrapper layers.
func TestPublicWriteBrokenPipe(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		defer writer.Close()
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		n, err := fileWrite(writer, []byte("broken pipe"))
		var pathErr *os.PathError
		if n != 0 || !errors.Is(err, syscall.EPIPE) || !errors.As(err, &pathErr) ||
			pathErr.Op != "write" || pathErr.Path != writer.Name() {
			t.Fatalf("broken pipe Write = (%d, %v), want write PathError wrapping EPIPE", n, err)
		}
	})

	t.Run("tcp", func(t *testing.T) {
		_, writer, cleanup := newTCPPair(t)
		defer cleanup()
		if err := writer.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		n, err := tcpWrite(writer, []byte("closed write half"))
		var opErr *net.OpError
		var syscallErr *os.SyscallError
		if n != 0 || !errors.Is(err, syscall.EPIPE) || !errors.As(err, &opErr) ||
			opErr.Op != "write" || !errors.As(err, &syscallErr) || syscallErr.Syscall != "write" {
			t.Fatalf("closed write half Write = (%d, %v), want write OpError wrapping write SyscallError and EPIPE", n, err)
		}
	})
}
