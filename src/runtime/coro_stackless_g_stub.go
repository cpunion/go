// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.coro

package runtime

import "unsafe"

type stacklessCoroG struct{}

type stacklessCoroSudog struct{}

func (*stacklessCoroSudog) get() unsafe.Pointer {
	return nil
}

func (*stacklessCoroSudog) set(unsafe.Pointer) {
	throw("runtime: stackless coroutine channel waiter without experiment")
}

func (*stacklessCoroSudog) clear() {}

func finishStacklessCoroChannel(unsafe.Pointer, bool) {
	throw("runtime: stackless coroutine channel waiter without experiment")
}

func releaseStacklessCoroSudog(*sudog) {
	throw("runtime: stackless coroutine channel waiter without experiment")
}

func (*g) stackIsFixed() bool {
	return false
}

func stacklessCoroRecover(*g) any {
	return nil
}
