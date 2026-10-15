// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package escape

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
)

// HeapLocals reports variables that require independently owned storage before
// a transformation changes their containing allocation. It uses the ordinary
// escape graph but does not rewrite expressions or retain escape annotations.
// Funcs must still be called after the transformation.
func HeapLocals(all []*ir.Func) map[*ir.Name]bool {
	a := &heapAnalysis{
		locals: make(map[*ir.Name]bool),
		funcs:  make(map[*ir.Func]heapFunctionState),
		tags:   make(map[*types.Field]string),
	}
	defer a.restore()
	funcs(all, a)
	return a.locals
}

// Function states and parameter tags are temporary interprocedural summaries.
// Locations use Name.Opt as usual and are removed at the end of each batch.
// No other source annotations are changed by heap analysis.
type heapAnalysis struct {
	locals map[*ir.Name]bool
	funcs  map[*ir.Func]heapFunctionState
	tags   map[*types.Field]string
}

type heapFunctionState struct {
	esc                uint16
	closureResultsLost bool
}

func (a *heapAnalysis) restore() {
	for fn, state := range a.funcs {
		fn.SetEsc(state.esc)
		fn.SetClosureResultsLost(state.closureResultsLost)
	}
	for param, tag := range a.tags {
		param.Note = tag
	}
}

func (b *batch) finishHeapAnalysis(fns []*ir.Func) {
	for _, fn := range fns {
		fn.SetEsc(escFuncTagged)
		for i, param := range fn.Type().RecvParams() {
			if _, saved := b.heapAnalysis.tags[param]; !saved {
				b.heapAnalysis.tags[param] = param.Note
			}
			param.Note = b.paramTag(fn, 1+i, param)
		}
	}
	for _, loc := range b.allLocs {
		if name, ok := loc.n.(*ir.Name); ok {
			name.Opt = nil
			if loc.hasAttr(attrEscapes) {
				b.heapAnalysis.locals[name] = true
			}
		}
	}
}
