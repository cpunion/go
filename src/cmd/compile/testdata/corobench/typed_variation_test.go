// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package corobench

import "testing"

func BenchmarkAlternatingTyped100000(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if got := alternatingTypedTasks(100000); got != 100000 {
			b.Fatalf("typed tasks %d", got)
		}
	}
}
func TestAlternatingTypedTasks(t *testing.T) {
	if got := alternatingTypedTasks(1025); got != 1025 {
		t.Fatal(got)
	}
}
