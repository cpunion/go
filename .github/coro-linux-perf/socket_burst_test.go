// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package corobench

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// The receiver drains each burst after the timed section. The configured
// socket buffers can hold the entire burst without a userspace reader.
func BenchmarkTCPWriteBufferedBurst(b *testing.B) {
	const writes = 32
	reader, writer, cleanup := newTCPPair(b)
	defer cleanup()
	if err := writer.SetWriteBuffer(256 << 10); err != nil {
		b.Fatal(err)
	}
	if err := reader.SetReadBuffer(256 << 10); err != nil {
		b.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x57}, 64)
	want := bytes.Repeat(payload, writes)
	received := make([]byte, len(want))
	deadline := time.Now().Add(30 * time.Second)
	if err := writer.SetWriteDeadline(deadline); err != nil {
		b.Fatal(err)
	}
	if err := reader.SetReadDeadline(deadline); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(want)))
	b.ResetTimer()
	var elapsed time.Duration
	for range b.N {
		start := time.Now()
		written, err := tcpWrites(writer, payload, writes)
		elapsed += time.Since(start)
		if err != nil || written != len(want) {
			b.Fatalf("wrote %d bytes, want %d, error %v", written, len(want), err)
		}
		if _, err := io.ReadFull(reader, received); err != nil {
			b.Fatal(err)
		}
		if !bytes.Equal(received, want) {
			b.Fatal("received bytes differ from payload")
		}
	}
	b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N), "ns/op")
	b.ReportMetric(writes, "writes/op")
}
