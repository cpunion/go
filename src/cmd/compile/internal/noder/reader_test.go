// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"reflect"
	"testing"
)

func TestPartitionCgoPragmas(t *testing.T) {
	pragmas := [][]string{
		{"cgo_import_static", "_cgo_add"},
		{"cgo_direct", "v1", "_Cfunc_add", "_Cdirect_add", "add",
			"mayblock", "u64,u64", "u64", "-"},
		nil,
		{"cgo_ldflag", "-lm"},
	}
	linker, compiler := partitionCgoPragmas(pragmas)
	wantLinker := [][]string{
		{"cgo_import_static", "_cgo_add"},
		nil,
		{"cgo_ldflag", "-lm"},
	}
	wantCompiler := [][]string{pragmas[1]}
	if !reflect.DeepEqual(linker, wantLinker) {
		t.Errorf("linker pragmas = %v, want %v", linker, wantLinker)
	}
	if !reflect.DeepEqual(compiler, wantCompiler) {
		t.Errorf("compiler pragmas = %v, want %v", compiler, wantCompiler)
	}
}
