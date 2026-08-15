// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ssa"
	"fmt"
	"io"
	"sync"
)

var dumpMu sync.Mutex

// DumpPreLowerSSA reports the machine-independent SSA presented at the target
// lowering boundary.
func DumpPreLowerSSA(w io.Writer, f *ssa.Func) {
	values := 0
	for _, block := range f.Blocks {
		values += len(block.Values)
	}

	// Backend compilation is parallel, so keep each diagnostic line intact.
	dumpMu.Lock()
	defer dumpMu.Unlock()
	fmt.Fprintf(w, "coro: phase=pre-lower-ssa func=%s blocks=%d values=%d action=continue-native\n",
		f.NameABI(), len(f.Blocks), values)
}
