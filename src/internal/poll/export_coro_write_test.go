// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package poll

func (fd *FD) CoroTestTryWriteLock() (bool, error) {
	return fd.tryWriteLock()
}

func (fd *FD) CoroTestWriteUnlock() {
	fd.writeUnlock()
}
