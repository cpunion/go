// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"internal/testenv"
	"os"
	"runtime"
	"testing"
)

func TestStacklessCoroNetpollReadyAccounting(t *testing.T) {
	const child = "GO_TEST_CORO_NETPOLL_ACCOUNTING"
	if os.Getenv(child) != "1" {
		// The probe owns the completion queue and must not run alongside
		// the completion goroutine started by other coroutine tests.
		cmd := testenv.CleanCmdEnv(testenv.Command(t, testenv.Executable(t),
			"-test.run=^TestStacklessCoroNetpollReadyAccounting$"))
		cmd.Env = append(cmd.Env, child+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("netpoll accounting subprocess: %v\n%s", err, out)
		}
		return
	}
	for _, test := range []struct {
		name string
		mode int32
	}{{"read", 'r'}, {"write", 'w'}, {"read-write", 'r' + 'w'}} {
		for state, name := range []string{"ready", "prepark", "parked"} {
			t.Run(test.name+"/"+name, func(t *testing.T) {
				delta, before, after, runnable, queued :=
					runtime.StacklessCoroNetpollReadyAccountingForTest(test.mode, state)
				wantDelta := int32(0)
				if state == 2 {
					// A duplex completion still readies only one G.
					wantDelta = -1
				}
				if delta != wantDelta || before != after || runnable != (state == 2) || !queued {
					t.Fatalf("accounting = (delta %d, waiters %d -> %d, runnable %t, queued %t), want (%d, unchanged, %t, true)",
						delta, before, after, runnable, queued, wantDelta, state == 2)
				}
			})
		}
	}
}
