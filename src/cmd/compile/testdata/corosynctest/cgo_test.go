// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo

package corosynctest

import (
	"testing"
	"testing/synctest"
)

func TestForeign(t *testing.T) {
	for range 1 {
		if got := foreign(35); got != 42 {
			t.Fatalf("ordinary context C result = %d", got)
		}
		synctest.Test(t, func(t *testing.T) {
			if got := foreign(35); got != 42 {
				t.Fatalf("bubble context C result = %d", got)
			}
		})
	}
}
