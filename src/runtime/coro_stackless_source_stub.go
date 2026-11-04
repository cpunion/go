// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.coro

package runtime

type stacklessCoroTracebackState struct{}

type stacklessCoroPanicState struct{}

func initStacklessCoroPanicTrace(*_panic, uintptr) {}

func stacklessCoroTracebackText(*unwinder, *stacklessCoroTracebackState, bool, bool, func() (bool, bool)) (bool, bool) {
	return false, false
}

func stacklessCoroSourceName(funcInfo) string { return "" }

func stacklessCoroTracebackPCs(*unwinder, int, []uintptr) (int, bool) {
	return 0, false
}
