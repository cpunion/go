// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"fmt"
)

// Verify checks the frozen package plan before escape analysis and lowering.
func (p *Plan) Verify() error {
	if p == nil {
		return fmt.Errorf("nil coroutine plan")
	}
	for fn, function := range p.Functions {
		if fn == nil || function == nil || function.Func != fn {
			return fmt.Errorf("invalid coroutine function entry")
		}
		wantPrimary := (FuncSummary{
			Effect:   function.Effect,
			Exec:     function.Exec,
			Terminal: function.Terminal,
		}).Primary()
		if function.Primary != wantPrimary {
			return fmt.Errorf("%s: primary %s does not match effect %s, exec %s, and terminal %s",
				ir.PkgFuncName(fn), function.Primary, function.Effect,
				function.Exec, function.Terminal)
		}
		seen := make(map[SiteID]bool, len(function.Sites))
		for _, site := range function.Sites {
			if site.ID == 0 {
				return fmt.Errorf("%s: site has zero ID", ir.PkgFuncName(fn))
			}
			if seen[site.ID] {
				return fmt.Errorf("%s: duplicate site ID %d", ir.PkgFuncName(fn), site.ID)
			}
			seen[site.ID] = true
			if site.Kind == SiteInvalid || site.Node == nil {
				return fmt.Errorf("%s: site %d is incomplete", ir.PkgFuncName(fn), site.ID)
			}
			if site.Kind == SitePanic && function.Terminal&MayPanic == 0 {
				return fmt.Errorf("%s: panic site %d lacks panic terminal flag",
					ir.PkgFuncName(fn), site.ID)
			}
			if site.Kind == SiteGoexit && function.Terminal&MayGoexit == 0 {
				return fmt.Errorf("%s: Goexit site %d lacks Goexit terminal flag",
					ir.PkgFuncName(fn), site.ID)
			}
			if site.Foreign == AsyncOperation && function.Effect != MaySuspend {
				return fmt.Errorf("%s: async foreign site %d is not suspending",
					ir.PkgFuncName(fn), site.ID)
			}
			if site.Foreign != NotForeign && function.Exec&NeedsSystemABI == 0 {
				return fmt.Errorf("%s: foreign site %d lacks System ABI execution flag",
					ir.PkgFuncName(fn), site.ID)
			}
		}
		if function.Primary == PlainPrimary {
			for _, site := range function.Sites {
				if siteMaySuspend(site) {
					return fmt.Errorf("%s: plain primary contains suspending site %d",
						ir.PkgFuncName(fn), site.ID)
				}
			}
		}
	}
	return nil
}

func siteMaySuspend(site Site) bool {
	switch site.Kind {
	case SiteYield, SiteAwait, SiteChannel, SiteTimer, SiteFile, SitePoll:
		return true
	case SiteForeign:
		return site.Foreign == AsyncOperation
	}
	return false
}
