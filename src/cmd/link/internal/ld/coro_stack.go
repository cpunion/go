// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"cmd/internal/obj"
	"cmd/internal/objabi"
	"cmd/link/internal/loader"
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

// trustedCoroStackBoundary lists the existing runtime operation ABI. These
// operations own their scheduler/system/foreign-stack protocols. Application
// helpers, including those in other packages, still require a machine proof.
func trustedCoroStackBoundary(name string) bool {
	if strings.HasPrefix(name, "runtime.") {
		return true
	}
	switch name {
	case "os.(*File).coroReadStart", "os.(*File).coroReadFinish",
		"os.(*File).coroWriteStart", "os.(*File).coroWriteFinish",
		"net.(*conn).coroReadStart", "net.(*conn).coroReadFinish",
		"net.(*conn).coroWriteStart", "net.(*conn).coroWriteFinish",
		"net.(*TCPConn).coroReadStart", "net.(*TCPConn).coroReadFinish",
		"net.(*TCPConn).coroWriteStart", "net.(*TCPConn).coroWriteFinish":
		return true
	}
	return false
}

func (ctxt *Link) doCoroStackCheck() {
	if !buildcfg.Experiment.Coro {
		return
	}
	ldr := ctxt.loader
	proof := coroStackProof{ctxt: ctxt, ldr: ldr, nosplit: newStackCheck(ctxt, false), heights: make(map[loader.Sym]int), active: make(map[loader.Sym]bool)}
	roots := make(map[loader.Sym][]loader.Sym)
	gates := make(map[loader.Sym]bool)
	unsafe := make(map[loader.Sym]bool)
	for _, s := range ctxt.Textp {
		info := ldr.FuncInfo(s)
		if !info.Valid() || info.FuncFlag()&abi.FuncFlagCoroStack == 0 {
			continue
		}
		fd := ldr.Funcdata(s, nil)
		if len(fd) <= abi.FUNCDATA_CoroStackGates || fd[abi.FUNCDATA_CoroStackGates] == 0 {
			ctxt.Errorf(s, "native machine entry has no stack gates")
			continue
		}
		metadata := ldr.Relocs(fd[abi.FUNCDATA_CoroStackGates])
		if metadata.Count() == 0 || metadata.Count()%2 != 0 {
			ctxt.Errorf(s, "malformed native stack activation metadata")
			continue
		}
		for i := 0; i < metadata.Count(); i += 2 {
			r, code := metadata.At(i), metadata.At(i+1)
			if r.Type() != objabi.R_ADDROFF || r.Siz() != 4 || code.Type() != objabi.R_ADDROFF || code.Siz() != 4 {
				ctxt.Errorf(s, "malformed native stack activation metadata")
				continue
			}
			gate, entry := r.Sym(), code.Sym()
			roots[entry] = append(roots[entry], gate)
			gates[gate] = true
		}
	}
	for entry := range roots {
		proof.failure = 0
		unsafe[entry] = proof.height(entry) > coroStackLimit || ctxt.DynlinkingGo()
		if unsafe[entry] && *flagDebugCoroStack {
			fmt.Printf("coro-stack: ordinary fallback: %s exceeds %d bytes or module proof unavailable (at %s)\n", ldr.SymName(entry), coroStackLimit, ldr.SymName(proof.failure))
		}
	}
	// A gate owns a factory, not every cleanup function shared with other gates.
	// Only its factory inherits the gate's rejection; otherwise one unsafe caller
	// would incorrectly reject an independent caller of the same safe cleanup.
	factories := make(map[loader.Sym]loader.Sym)
	for gate := range gates {
		relocs := ldr.Relocs(gate)
		for i := 0; i < relocs.Count(); i++ {
			r := relocs.At(i)
			if r.Type() == objabi.R_ADDR && r.Off() == int32(ctxt.Arch.PtrSize) {
				factories[gate] = r.Sym()
			}
		}
		if factories[gate] == 0 {
			ctxt.Errorf(gate, "native stack gate has no factory")
		}
	}

	// Factories and resumes execute in separate scheduler activations. Their
	// stack heights do not add, but an unsafe child makes every native parent
	// unsafe, including imported factories and address-taken cleanup entries.
	// Cycles here are finite state-machine recursion, not machine recursion.
	dependencies := make(map[loader.Sym][]loader.Sym)
	for root := range roots {
		seen := make(map[loader.Sym]bool)
		var visit func(loader.Sym)
		visit = func(s loader.Sym) {
			if seen[s] || trustedCoroStackBoundary(ldr.SymName(s)) {
				return
			}
			seen[s] = true
			relocs := ldr.Relocs(s)
			for i := 0; i < relocs.Count(); i++ {
				r := relocs.At(i)
				target := r.Sym()
				if target == 0 {
					continue
				}
				if !r.Type().IsDirectCall() {
					// A static func value is one code pointer. Do not treat type descriptors
					// or source Code comparisons as execution of arbitrary ordinary bodies.
					if ldr.SymSize(target) == int64(ctxt.Arch.PtrSize) {
						refs := ldr.Relocs(target)
						if refs.Count() == 1 && refs.At(0).Type() == objabi.R_ADDR && refs.At(0).Off() == 0 {
							target = refs.At(0).Sym()
						}
					}
					if _, native := roots[target]; !native {
						continue
					}
				}
				if _, native := roots[target]; native {
					dependencies[root] = append(dependencies[root], target)
				}
				if r.Type().IsDirectCall() {
					visit(target)
				}
			}
		}
		visit(root)
	}
	for changed := true; changed; {
		changed = false
		for root, group := range roots {
			bad := unsafe[root]
			for _, child := range dependencies[root] {
				bad = bad || unsafe[child]
			}
			if bad {
				if !unsafe[root] {
					unsafe[root] = true
					changed = true
				}
				for _, gate := range group {
					if !unsafe[gate] {
						unsafe[gate] = true
						changed = true
					}
				}
			}
		}
		for gate, factory := range factories {
			if unsafe[gate] && !unsafe[factory] {
				unsafe[factory] = true
				changed = true
			}
		}
	}

	rejected := 0
	for gate := range gates {
		if !unsafe[gate] {
			continue
		}
		rejected++
		// Only this entry's flag changes. Its code-address word and relocations
		// remain intact, so recovery can inspect the exact factory binding.
		sb := ldr.MakeSymbolUpdater(gate)
		sb.MakeWritable()
		sb.SetUint(ctxt.Arch, 0, 1)
	}
	if *flagDebugCoroStack {
		fmt.Printf("coro-stack: %d native machine entries, %d gates, %d ordinary gates\n", len(roots), len(gates), rejected)
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
	if trustedCoroStackBoundary(name) {
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
