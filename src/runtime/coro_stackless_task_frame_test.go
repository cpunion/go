// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime_test

import (
	"bytes"
	"internal/race"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
	"unsafe"
)

func TestStacklessCoroTypedTaskFrames(t *testing.T) {
	if !runtime.CheckStacklessCoroTypedTaskFrameTypesForTest() {
		t.Fatal("combined task/frame pointer map or rejected-layout contract failed")
	}
	for _, tc := range []struct {
		name                                        string
		varyPrefix, cancelReservations, phaseSwitch bool
	}{
		{name: "matching-prefix"},
		{name: "mixed-prefix", varyPrefix: true},
		{name: "panic-cancels-reservations", cancelReservations: true},
		{name: "typed-to-scalar-phase", phaseSwitch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testStacklessCoroTypedTaskFrames(t, tc.varyPrefix, tc.cancelReservations, tc.phaseSwitch)
		})
	}
}

func testStacklessCoroTypedTaskFrames(t *testing.T, varyPrefix, cancelReservations, phaseSwitch bool) {
	t.Helper()
	const count = 513
	valid, combined, finalizerToken := runtime.CheckStacklessCoroTypedTaskFramesForTest(count, varyPrefix, cancelReservations, phaseSwitch)
	t.Logf("combined task/frame allocations = %d of %d", combined, count)
	if !valid {
		t.Fatal("combined task/frame payload, completion or clearing contract failed")
	}
	if unsafe.Sizeof(uintptr(0)) == 8 && !race.Enabled && !varyPrefix && combined == 0 {
		t.Fatal("no actual combined allocation beyond the cache prefix")
	}
	if (unsafe.Sizeof(uintptr(0)) != 8 || race.Enabled || varyPrefix) && combined != 0 {
		t.Fatal("combined allocation used on an excluded target/mode")
	}
	deadline := time.Now().Add(10 * time.Second)
	for runtime.StacklessCoroTypedTaskFrameFinalizedForTest(finalizerToken) != count && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
	if got := runtime.StacklessCoroTypedTaskFrameFinalizedForTest(finalizerToken); got != count {
		t.Fatalf("finalized payloads = %d, want %d", got, count)
	}
}

func TestStacklessCoroTypedTaskFrameInvalidType(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 || race.Enabled {
		t.Skip("combined allocation is excluded on this target/mode")
	}
	if os.Getenv("GOTEST_STACKLESS_CORO_BAD_TASK_FRAME") == "1" {
		runtime.StacklessCoroInvalidTaskFrameTypeForTest()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStacklessCoroTypedTaskFrameInvalidType$", "-test.count=1")
	cmd.Env = append(os.Environ(), "GOTEST_STACKLESS_CORO_BAD_TASK_FRAME=1")
	out, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 || !bytes.Contains(out, []byte("runtime: invalid stackless coroutine task-frame allocation type")) {
		t.Fatalf("invalid allocation type: error=%v, output=%s", err, out)
	}
}
