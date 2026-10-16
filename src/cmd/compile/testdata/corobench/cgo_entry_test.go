// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && (darwin || linux)

package corobench

import "testing"

func TestForeignEntryFrameReuse(t *testing.T) {
	for _, test := range []struct {
		name string
		call func()
	}{
		{"scalar", func() {
			// Keep the controller on the ordinary Go entry, as in the
			// public Mutex tests. Only the foreign callee is under test.
			for range 1 {
				uintSink = cScalarCalls(1)
			}
		}},
		{"libm", func() {
			for range 1 {
				floatSink = cLibmCalls(1)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := testing.AllocsPerRun(1000, test.call); got != 0 {
				t.Errorf("warm foreign entry allocates %g objects per call, want zero", got)
			}
		})
	}
}
