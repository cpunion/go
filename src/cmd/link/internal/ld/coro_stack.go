// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"cmd/internal/obj"
	"cmd/internal/objabi"
	"cmd/link/internal/loader"
	"cmd/link/internal/sym"
	"flag"
	"fmt"
	"internal/abi"
	"internal/buildcfg"
	"strings"
)

var flagDebugCoroStack = flag.Bool("debugcorostack", false, "print coroutine machine-stack eligibility")

// A proof covers one machine activation and every ordinary Go helper it can
// call. Existing coroutine runtime operations are explicit stack boundaries;
// their scheduler, system-stack and foreign ABI protocols are unchanged.
// Use the ordinary nosplit reserve as a conservative application-code budget,
// leaving the rest of the native thread stack for those runtime protocols.
const coroStackLimit = abi.StackNosplitBase

type coroStackProof struct {
	ctxt    *Link
	ldr     *loader.Loader
	nosplit *stackCheck
	heights map[loader.Sym]int
	active  map[loader.Sym]bool
	failure loader.Sym
}

func (ctxt *Link) doCoroStackCheck() {
	if !buildcfg.Experiment.Coro {
		return
	}
	ldr := ctxt.loader
	guard := ldr.Lookup("runtime.coroStackUnsafe", 0)
	if guard == 0 || !ldr.AttrReachable(guard) {
		return
	}
	proof := coroStackProof{ctxt: ctxt, ldr: ldr, nosplit: newStackCheck(ctxt, false), heights: make(map[loader.Sym]int), active: make(map[loader.Sym]bool)}
	roots := 0
	for _, s := range ctxt.Textp {
		info := ldr.FuncInfo(s)
		if !info.Valid() || info.FuncFlag()&abi.FuncFlagCoroStack == 0 {
			continue
		}
		roots++
		if proof.height(s) > coroStackLimit {
			sb := ldr.MakeSymbolUpdater(guard)
			sb.SetType(sym.SNOPTRDATA)
			sb.SetData([]byte{1})
			sb.SetSize(1)
			if *flagDebugCoroStack {
				fmt.Printf("coro-stack: ordinary fallback: %s exceeds %d bytes (at %s)\n", ldr.SymName(s), coroStackLimit, ldr.SymName(proof.failure))
			}
			return
		}
	}
	if *flagDebugCoroStack {
		fmt.Printf("coro-stack: %d native machine entries fit %d bytes\n", roots, coroStackLimit)
	}
}

func (p *coroStackProof) height(s loader.Sym) int {
	if n, ok := p.heights[s]; ok {
		return n
	}
	if p.active[s] {
		p.failure = s
		return coroStackLimit + 1
	}
	name := p.ldr.SymName(s)
	if strings.HasPrefix(name, "runtime.") {
		return 0
	}
	info := p.ldr.FuncInfo(s)
	if !info.Valid() {
		p.failure = s
		return coroStackLimit + 1
	}
	// Assembly and the existing raw C bridges retain the runtime's established
	// ABI boundary. The ordinary linker check still verifies nosplit chains.
	if info.FuncFlag()&abi.FuncFlagAsm != 0 {
		return p.nosplit.check(s)
	}
	p.active[s] = true
	maximum := 0
	callSize := p.nosplit.callSize
	relocs := p.ldr.Relocs(s)
	ri := 0
	pcsp := obj.NewPCIter(uint32(p.ctxt.Arch.MinLC))
	for pcsp.Init(p.ldr.Data(p.ldr.Pcsp(s))); !pcsp.Done; pcsp.Next() {
		height := int(pcsp.Value)
		if height > maximum {
			maximum = height
		}
		for ; ri < relocs.Count(); ri++ {
			r := relocs.At(ri)
			if uint32(r.Off()) >= pcsp.NextPC {
				break
			}
			if r.Type().IsDirectCall() {
				next := height + callSize + p.height(r.Sym())
				if next > maximum {
					maximum = next
				}
			} else if r.Type() == objabi.R_CALLIND {
				p.failure = s
				maximum = coroStackLimit + 1
				break
			}
			if maximum > coroStackLimit {
				break
			}
		}
		if maximum > coroStackLimit {
			break
		}
	}
	delete(p.active, s)
	if maximum > coroStackLimit {
		if p.failure == 0 {
			p.failure = s
		}
		maximum = coroStackLimit + 1
	}
	p.heights[s] = maximum
	return maximum
}
