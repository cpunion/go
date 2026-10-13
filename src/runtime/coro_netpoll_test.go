// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime_test

import (
	"runtime"
	"testing"
)

func TestStacklessCoroPollArmErrorRace(t *testing.T) {
	for _, mode := range []int32{'r', 'w'} {
		for _, test := range []struct {
			name string
			err  int
		}{
			{"ready", 0},
			{"close", runtime.StacklessCoroPollErrClosing},
			{"deadline", runtime.StacklessCoroPollErrTimeout},
		} {
			for completion, name := range []string{"pending", "error consumed", "ready consumed"} {
				t.Run(string(mode)+"/"+test.name+"/"+name, func(t *testing.T) {
					waiting, errno, tokenMatches, statePreserved :=
						runtime.StacklessCoroPollArmRaceForTest(mode, test.err, completion)
					wantWait := test.err == 0 || completion != 0
					wantErr := 0
					if !wantWait {
						wantErr = test.err
					}
					wantToken := test.err == 0 && completion == 0
					if waiting != wantWait || errno != wantErr || tokenMatches != wantToken || !statePreserved {
						t.Fatalf("arm check = (%t, %d, %t, %t), want (%t, %d, %t, true)",
							waiting, errno, tokenMatches, statePreserved, wantWait, wantErr, wantToken)
					}
				})
			}
		}
	}
}
