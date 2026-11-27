// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime_test

import (
	"runtime"
	"runtime/metrics"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

func TestCoroV2RootReuseAndZeroRetention(t *testing.T) {
	type rootFrame struct {
		hdr   runtime.CoroV2Header
		state uint32
		in    uint64
		out   uint64
	}
	typ := runtime.CoroV2TypeOfForTest(rootFrame{})
	resume := func(ctx unsafe.Pointer) uint8 {
		f := (*rootFrame)(ctx)
		if f.state == 0 {
			f.state = 1
			return runtime.CoroV2ActionYieldForTest()
		}
		f.out = f.in + 42
		return runtime.CoroV2ActionCompleteForTest()
	}
	for i := uint64(0); i < 100; i++ {
		ptr := runtime.CoroV2AllocForTest(typ)
		f := (*rootFrame)(ptr)
		f.in = i
		runtime.CoroV2RunFrameForTest(ptr, resume)
		if f.out != i+42 {
			t.Fatalf("round %d: got %d want %d", i, f.out, i+42)
		}
		runtime.CoroV2ReleaseFrameForTest(ptr)
	}
	// Two idle GCs must expire 100% of cached frames back to 0 B.
	runtime.GC()
	runtime.GC()
	if b := runtime.CoroV2CacheBytesForTest(); b != 0 {
		t.Fatalf("cacheBytes after 2 GCs = %d, want 0", b)
	}
}

func TestCoroV2SpawnFanoutMultiP(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	type workerFrame struct {
		hdr   runtime.CoroV2Header
		state uint32
		id    int
		ch    chan int
	}
	type rootFrame struct {
		hdr   runtime.CoroV2Header
		state uint32
		n     int
		i     int
		sum   int
		val   int
		ch    chan int
	}
	wTyp := runtime.CoroV2TypeOfForTest(workerFrame{})
	rTyp := runtime.CoroV2TypeOfForTest(rootFrame{})

	wResume := func(ctx unsafe.Pointer) uint8 {
		f := (*workerFrame)(ctx)
		switch f.state {
		case 0:
			f.state = 1
			return runtime.CoroV2ActionYieldForTest()
		case 1:
			f.state = 2
			v := f.id + 1
			if runtime.CoroV2ChanSendForTest(ctx, f.ch, unsafe.Pointer(&v)) {
				return runtime.CoroV2ActionWaitForTest()
			}
			return runtime.CoroV2ActionCompleteForTest()
		default:
			return runtime.CoroV2ActionCompleteForTest()
		}
	}

	rResume := func(ctx unsafe.Pointer) uint8 {
		f := (*rootFrame)(ctx)
		if f.state == 0 {
			for i := 0; i < f.n; i++ {
				wp := runtime.CoroV2AllocForTest(wTyp)
				wf := (*workerFrame)(wp)
				wf.id = i
				wf.ch = f.ch
				runtime.CoroV2SpawnFrameForTest(ctx, wp, wResume)
			}
			f.state = 1
		}
		for f.i < f.n {
			if f.state == 2 {
				f.sum += f.val
				f.i++
				f.state = 1
				continue
			}
			if runtime.CoroV2ChanRecvForTest(ctx, f.ch, unsafe.Pointer(&f.val), nil) {
				f.state = 2
				return runtime.CoroV2ActionWaitForTest()
			}
			f.sum += f.val
			f.i++
		}
		return runtime.CoroV2ActionCompleteForTest()
	}

	const N = 1000
	rp := runtime.CoroV2AllocForTest(rTyp)
	rf := (*rootFrame)(rp)
	rf.n = N
	rf.ch = make(chan int, 16)
	runtime.CoroV2RunFrameForTest(rp, rResume)
	got := rf.sum
	runtime.CoroV2ReleaseFrameForTest(rp)

	want := N * (N + 1) / 2
	if got != want {
		t.Fatalf("sum = %d, want %d", got, want)
	}
	// Wait briefly for last unhosted worker retirement.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.CoroV2UnhostedForTest() != 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if u := runtime.CoroV2UnhostedForTest(); u != 0 {
		t.Fatalf("unhosted = %d after drain, want 0", u)
	}
}

func TestCoroV2ChannelPingPongAndClose(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	type pingFrame struct {
		hdr   runtime.CoroV2Header
		state uint32
		i     int
		n     int
		v     int
		ping  chan int
		pong  chan int
	}
	typ := runtime.CoroV2TypeOfForTest(pingFrame{})
	worker := func(ctx unsafe.Pointer) uint8 {
		f := (*pingFrame)(ctx)
		for f.i < f.n {
			switch f.state {
			case 0:
				if runtime.CoroV2ChanRecvForTest(ctx, f.ping, unsafe.Pointer(&f.v), nil) {
					f.state = 1
					return runtime.CoroV2ActionWaitForTest()
				}
				f.state = 1
			case 1:
				f.v++
				f.state = 2
				if runtime.CoroV2ChanSendForTest(ctx, f.pong, unsafe.Pointer(&f.v)) {
					return runtime.CoroV2ActionWaitForTest()
				}
			case 2:
				f.i++
				f.state = 0
			}
		}
		return runtime.CoroV2ActionCompleteForTest()
	}

	// Ordinary G ping-ponging with stackless coroutine.
	ping := make(chan int)
	pong := make(chan int)
	const iters = 500
	wp := runtime.CoroV2AllocForTest(typ)
	wf := (*pingFrame)(wp)
	wf.n = iters
	wf.ping = ping
	wf.pong = pong
	runtime.CoroV2SpawnFrameForTest(nil, wp, worker)
	for i := 0; i < iters; i++ {
		ping <- i
		if got := <-pong; got != i+1 {
			t.Fatalf("iter %d: got %d want %d", i, got, i+1)
		}
	}
}

func TestCoroV2SelectAndSleep(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	type selFrame struct {
		hdr    runtime.CoroV2Header
		state  uint32
		c1     chan int
		c2     chan int
		v1     int
		v2     int
		chosen int
		ok     bool
	}
	typ := runtime.CoroV2TypeOfForTest(selFrame{})
	c1 := make(chan int)
	c2 := make(chan int)

	go func() {
		time.Sleep(5 * time.Millisecond)
		c2 <- 99
	}()

	fp := runtime.CoroV2AllocForTest(typ)
	f := (*selFrame)(fp)
	f.c1 = c1
	f.c2 = c2
	runtime.CoroV2RunFrameForTest(fp, func(ctx unsafe.Pointer) uint8 {
		sf := (*selFrame)(ctx)
		switch sf.state {
		case 0:
			sf.state = 1
			cases := []runtime.CoroV2SelectCaseForTest{
				{Ch: sf.c1, Elem: unsafe.Pointer(&sf.v1)},
				{Ch: sf.c2, Elem: unsafe.Pointer(&sf.v2)},
			}
			if runtime.CoroV2SelectForTest(ctx, cases, 0, 2, true, &sf.chosen, &sf.ok) {
				return runtime.CoroV2ActionWaitForTest()
			}
			return runtime.CoroV2ActionYieldForTest()
		case 1:
			sf.state = 2
			if runtime.CoroV2SleepForTest(ctx, int64(2*time.Millisecond)) {
				return runtime.CoroV2ActionWaitForTest()
			}
			return runtime.CoroV2ActionCompleteForTest()
		default:
			return runtime.CoroV2ActionCompleteForTest()
		}
	})
	if f.chosen != 1 || !f.ok || f.v2 != 99 {
		t.Fatalf("select result: chosen=%d ok=%v v2=%d, want 1, true, 99", f.chosen, f.ok, f.v2)
	}
	runtime.CoroV2ReleaseFrameForTest(fp)
}

func TestCoroV2DeferPanicRecoverAndGoexit(t *testing.T) {
	type childFrame struct {
		hdr    runtime.CoroV2Header
		events *[]string
		mode   string
	}
	type deferFrame struct {
		hdr       runtime.CoroV2Header
		events    *[]string
		recovered *any
	}
	type rootFrame struct {
		hdr    runtime.CoroV2Header
		state  uint32
		events *[]string
		mode   string
	}
	cTyp := runtime.CoroV2TypeOfForTest(childFrame{})
	dTyp := runtime.CoroV2TypeOfForTest(deferFrame{})
	rTyp := runtime.CoroV2TypeOfForTest(rootFrame{})

	// 1. Panic in awaited child -> recovered by deferred coroutine on root -> normal return!
	var events []string
	var rec any
	rp := runtime.CoroV2AllocForTest(rTyp)
	rf := (*rootFrame)(rp)
	rf.events = &events
	rf.mode = "recover"
	runtime.CoroV2RunFrameForTest(rp, func(ctx unsafe.Pointer) uint8 {
		f := (*rootFrame)(ctx)
		if f.state == 0 {
			f.state = 1
			dp := runtime.CoroV2AllocForTest(dTyp)
			df := (*deferFrame)(dp)
			df.events = f.events
			df.recovered = &rec
			runtime.CoroV2DeferCoroForTest(ctx, dp, func(dctx unsafe.Pointer) uint8 {
				ddf := (*deferFrame)(dctx)
				*ddf.events = append(*ddf.events, "defer-coro")
				*ddf.recovered = runtime.CoroV2RecoverForTest(dctx)
				return runtime.CoroV2ActionCompleteForTest()
			})
			runtime.CoroV2DeferForTest(ctx, func() {
				*f.events = append(*f.events, "defer-plain")
			})
			cp := runtime.CoroV2AllocForTest(cTyp)
			cf := (*childFrame)(cp)
			cf.events = f.events
			return runtime.CoroV2AwaitFrameForTest(ctx, cp, func(cctx unsafe.Pointer) uint8 {
				ccf := (*childFrame)(cctx)
				*ccf.events = append(*ccf.events, "child-panic")
				runtime.CoroV2PanicForTest(cctx, "boom")
				return runtime.CoroV2ActionPanicForTest()
			})
		}
		return runtime.CoroV2ActionCompleteForTest()
	})
	runtime.CoroV2ReleaseFrameForTest(rp)
	if rec != "boom" {
		t.Fatalf("recovered = %v, want boom", rec)
	}
	wantEvents := []string{"child-panic", "defer-plain", "defer-coro"}
	if len(events) != len(wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	for i := range wantEvents {
		if events[i] != wantEvents[i] {
			t.Fatalf("events[%d] = %q, want %q (all=%v)", i, events[i], wantEvents[i], events)
		}
	}

	// 2. Unrecovered panic propagates onto caller Go stack.
	var caught any
	func() {
		defer func() { caught = recover() }()
		rp2 := runtime.CoroV2AllocForTest(rTyp)
		runtime.CoroV2RunFrameForTest(rp2, func(ctx unsafe.Pointer) uint8 {
			runtime.CoroV2PanicForTest(ctx, "unhandled")
			return runtime.CoroV2ActionPanicForTest()
		})
	}()
	if caught != "unhandled" {
		t.Fatalf("propagated panic = %v, want unhandled", caught)
	}

	// 3. runtime.Goexit inside spawned coroutine runs defers and exits cleanly without crashing.
	done := make(chan string, 1)
	cp := runtime.CoroV2AllocForTest(cTyp)
	runtime.CoroV2SpawnFrameForTest(nil, cp, func(ctx unsafe.Pointer) uint8 {
		runtime.CoroV2DeferForTest(ctx, func() {
			done <- "goexit-defer-ran"
		})
		runtime.CoroV2GoexitForTest(ctx)
		return runtime.CoroV2ActionGoexitForTest()
	})
	if got := <-done; got != "goexit-defer-ran" {
		t.Fatalf("goexit defer = %q", got)
	}
}

func TestCoroV2MassivePark10kAndMetrics(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	runtime.GC()
	runtime.GC()

	type parkFrame struct {
		hdr   runtime.CoroV2Header
		state uint32
		gate  chan struct{}
		ready *atomic.Int64
		done  *atomic.Int64
	}
	type rootFrame struct {
		hdr   runtime.CoroV2Header
		state uint32
	}
	pTyp := runtime.CoroV2TypeOfForTest(parkFrame{})
	rTyp := runtime.CoroV2TypeOfForTest(rootFrame{})

	const N = 10000
	gate := make(chan struct{})
	var ready, done atomic.Int64
	var before, parked runtime.MemStats
	runtime.ReadMemStats(&before)
	beforeG := runtime.NumGoroutine()

	worker := func(ctx unsafe.Pointer) uint8 {
		f := (*parkFrame)(ctx)
		if f.state == 0 {
			f.state = 1
			f.ready.Add(1)
			if runtime.CoroV2ChanRecvForTest(ctx, f.gate, nil, nil) {
				return runtime.CoroV2ActionWaitForTest()
			}
		}
		f.done.Add(1)
		return runtime.CoroV2ActionCompleteForTest()
	}

	var parkedG int
	var parkedUnhosted uint64
	var stackDelta int64

	rp := runtime.CoroV2AllocForTest(rTyp)
	runtime.CoroV2RunFrameForTest(rp, func(ctx unsafe.Pointer) uint8 {
		f := (*rootFrame)(ctx)
		switch f.state {
		case 0:
			for i := 0; i < N; i++ {
				wp := runtime.CoroV2AllocForTest(pTyp)
				wf := (*parkFrame)(wp)
				wf.gate = gate
				wf.ready = &ready
				wf.done = &done
				runtime.CoroV2SpawnFrameForTest(ctx, wp, worker)
			}
			f.state = 1
			return runtime.CoroV2ActionYieldForTest()
		case 1:
			if ready.Load() < N {
				return runtime.CoroV2ActionYieldForTest()
			}
			runtime.ReadMemStats(&parked)
			stackDelta = int64(parked.StackInuse) - int64(before.StackInuse)
			parkedG = runtime.NumGoroutine() - beforeG
			var sample [1]metrics.Sample
			sample[0].Name = "/sched/coro/goroutines/unhosted:goroutines"
			metrics.Read(sample[:])
			parkedUnhosted = sample[0].Value.Uint64()
			close(gate)
			f.state = 2
			return runtime.CoroV2ActionYieldForTest()
		default:
			if done.Load() < N {
				return runtime.CoroV2ActionYieldForTest()
			}
			return runtime.CoroV2ActionCompleteForTest()
		}
	})
	runtime.CoroV2ReleaseFrameForTest(rp)

	if parkedG < N || parkedUnhosted != N {
		t.Fatalf("parkedG=%d parkedUnhosted=%d, want >=%d and ==%d", parkedG, parkedUnhosted, N, N)
	}
	if stackDelta > 64<<10 {
		t.Fatalf("Go stack grew by %d B for %d parked coroutines", stackDelta, N)
	}
	runtime.GC()
	runtime.GC()
	if b := runtime.CoroV2CacheBytesForTest(); b != 0 {
		t.Fatalf("cacheBytes after 2 GCs = %d, want 0", b)
	}
}

func TestCoroV2GOMAXPROCSShrinkDeterministic(t *testing.T) {
	prev := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(prev)

	const N = 64
	queued, nonZeroP := runtime.CoroV2SeedMultiPRingsForTest(N)
	if queued != N || nonZeroP != N {
		t.Fatalf("seeded queued=%d nonZeroP=%d, want %d", queued, nonZeroP, N)
	}
	// Shrink 4 -> 1 while all N tasks sit on P1..P3 rings; coroDrainP must hand them all off!
	runtime.GOMAXPROCS(1)
	done, hits := runtime.CoroV2DrainMultiPRingsForTest()
	if done != int64(N) || hits != int64(N) {
		t.Fatalf("after 4->1 shrink: done=%d hits=%d, want %d", done, hits, N)
	}
}
