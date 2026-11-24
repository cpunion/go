// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import (
	"internal/goarch"
	"unsafe"
)

func referenceStacklessCoroSourcePC(source funcInfo, file string, line int32) uintptr {
	entry := source.entry()
	if file == "" || line <= 0 {
		return entry
	}
	for pc := entry; ; pc += goarch.PCQuantum {
		f := findfunc(pc)
		if !f.valid() || f.entry() != entry {
			break
		}
		atFile, atLine := funcline1(source, pc, false)
		if atLine == line && atFile == file {
			return pc
		}
	}
	return entry
}

func CheckStacklessCoroSourcePCRangeForTest(limitFuncs int) (checkedFuncs, checkedQueries, mismatches int, firstMismatch string) {
	for _, datap := range activeModules() {
		for i := 0; i < len(datap.ftab)-1; i++ {
			f := funcInfo{(*_func)(unsafe.Pointer(&datap.pclntable[datap.ftab[i].funcoff])), datap}
			if !f.valid() || f.pcfile == 0 || f.pcln == 0 {
				continue
			}
			checkedFuncs++
			entry := f.entry()
			end := datap.textAddr(datap.ftab[i+1].entryoff)
			if end > entry+4096 {
				end = entry + 4096
			}
			seen := make(map[int32]bool)
			for pc := entry; pc < end; pc += goarch.PCQuantum * 8 {
				file, line := funcline1(f, pc, false)
				if file == "" || file == "?" || line <= 0 || seen[line] {
					continue
				}
				seen[line] = true
				want := referenceStacklessCoroSourcePC(f, file, line)
				got := stacklessCoroSourcePC(f, file, line)
				checkedQueries++
				if got != want {
					mismatches++
					if firstMismatch == "" {
						firstMismatch = funcname(f)
					}
				}
			}
			if got := stacklessCoroSourcePC(f, "", 1); got != entry {
				mismatches++
				if firstMismatch == "" {
					firstMismatch = "empty-file:" + funcname(f)
				}
			}
			if got := stacklessCoroSourcePC(f, "nonexistent_coro_source.go", 999999); got != entry {
				mismatches++
				if firstMismatch == "" {
					firstMismatch = "missing-line:" + funcname(f)
				}
			}
			if checkedFuncs >= limitFuncs {
				return
			}
		}
	}
	return
}
