// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.coro

package runtime

type coroTask struct{}
type coroPState struct{}
type coroSudog struct{}

func (s *coroSudog) coroTask() *coroTask { return nil }

func coroClearPools()                  {}
func coroDrainP(pp *p)                 {}
func coroUnhostedCount() uint64        { return 0 }
func coroChanWake(sg *sudog, ok bool)  {}
func coroChanCloseWake(head *sudog)    {}
func coroTryWinSelect(t *coroTask) bool { return false }

//go:nowritebarrier
func coroNetpollWake(taskPtr uintptr) *g { return nil }
