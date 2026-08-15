// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"bytes"
	"cmd/compile/internal/abi"
	"cmd/compile/internal/ssa"
	"testing"
)

func TestDumpPreLowerSSA(t *testing.T) {
	function := &ssa.Func{
		Name:    "f",
		ABISelf: abi.NewABIConfig(0, 0, 0, 0),
		Blocks: []*ssa.Block{
			{Values: []*ssa.Value{{}, {}}},
			{Values: []*ssa.Value{{}}},
		},
	}
	var output bytes.Buffer
	DumpPreLowerSSA(&output, function)
	const want = "coro: phase=pre-lower-ssa func=f,0 blocks=2 values=3 action=continue-native\n"
	if got := output.String(); got != want {
		t.Fatalf("DumpPreLowerSSA output = %q, want %q", got, want)
	}
}
