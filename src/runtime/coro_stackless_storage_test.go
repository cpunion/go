// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"runtime"
	"testing"
)

func TestStacklessCoroPlainStorage(t *testing.T) {
	if !runtime.CheckStacklessCoroPlainStorageForTest() {
		t.Fatal("plain operation allocated source-specific storage")
	}
}
