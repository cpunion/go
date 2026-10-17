// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"testing"
)

func TestReadCoroBodyUnavailable(t *testing.T) {
	pkg := types.NewPkg("example.com/coro/unavailable", "unavailable")
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("synthetic"),
		types.NewSignature(nil, nil, nil))
	if body := ReadCoroBody(fn); body != nil {
		t.Fatal("synthetic function acquired an ordinary source body")
	}
}
