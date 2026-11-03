// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package typecheck

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/types"
	"internal/abi"
)

// MapKeyRuntimeType returns the key parameter type of a fast map runtime call,
// or nil for a slow call that takes the key by address. Both suspension
// normalization and ordinary ordering need this to place key temporaries at
// the same evaluation point.
func MapKeyRuntimeType(t *types.Type) *types.Type {
	if t.Elem().Size() > abi.MapMaxElemBytes {
		return nil
	}
	key := t.Key()
	alg := types.AlgType(key)
	if alg == types.AMEM && (key.Alignment() >= int64(base.Ctxt.Arch.Alignment) || key.Alignment() >= key.Size()) {
		// Under-aligned aggregates cannot use a wider memory access.
		switch key.Size() {
		case 4:
			alg = types.AMEM32
		case 8:
			alg = types.AMEM64
		}
	}
	switch alg {
	case types.AMEM32:
		if !key.HasPointers() {
			return types.Types[types.TUINT32]
		}
		if types.PtrSize != 4 {
			base.Fatalf("small pointer %v", key)
		}
		return types.Types[types.TUNSAFEPTR]
	case types.AMEM64:
		if !key.HasPointers() {
			return types.Types[types.TUINT64]
		}
		if types.PtrSize == 8 {
			return types.Types[types.TUNSAFEPTR]
		}
	case types.ASTRING:
		return types.Types[types.TSTRING]
	}
	return nil
}
