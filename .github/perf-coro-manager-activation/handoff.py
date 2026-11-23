import json
from pathlib import Path
from subprocess import check_output

root = Path(__file__).parent.resolve()
goroot = Path(check_output([str(root / "sdk/bin/go"), "env", "GOROOT"], text=True).strip())
runtime = goroot / "src/runtime"
core_path = runtime / "coro_stackless.go"
native_path = runtime / "coro_stackless_native.go"
stub_path = runtime / "coro_stackless_native_stub.go"

def swap(text, before, after):
    assert text.count(before) == 1, before
    return text.replace(before, after)

core = core_path.read_text()
core = swap(core,
    "\ts = runStacklessCoroRoot(s)\n\tlifetime := s.lifetime.Load()",
    """	var driver stacklessCoroNativeDriver
	driver.allowTransfer = !raceenabled && !stacklessCoroIsPullComparison(s)
	s = runStacklessCoroRootDriver(s, &driver)
	if driver.transferred {
		s.startPendingReplacementManager()
		s.waitPublicRoot()
		s.finish()
		return false
	}
	lifetime := s.lifetime.Load()""")
core = swap(core,
    """func runStacklessCoroRoot(s *stacklessCoroScheduler) *stacklessCoroScheduler {
	var native stacklessCoroNativeDriver
	initial := true""",
    """func runStacklessCoroRoot(s *stacklessCoroScheduler) *stacklessCoroScheduler {
	var native stacklessCoroNativeDriver
	return runStacklessCoroRootDriver(s, &native)
}

func runStacklessCoroRootDriver(s *stacklessCoroScheduler, native *stacklessCoroNativeDriver) *stacklessCoroScheduler {
	initial := true""")
core = swap(core, "func (s *stacklessCoroScheduler) runOwner() {",
    """func (s *stacklessCoroScheduler) runOwner() {
	s.runOwnerWithDriver(stacklessCoroNativeDriver{})
}

func (s *stacklessCoroScheduler) runOwnerWithDriver(native stacklessCoroNativeDriver) {""")
core = swap(core, "\tvar native stacklessCoroNativeDriver\n\tfor !s.drained() {", "\tfor !s.drained() {")

native = native_path.read_text()
native = swap(native, "\tdeferredYield       bool\n",  "\tdeferredYield       bool\n\tallowTransfer       bool\n\ttransferredOwner    bool\n")
native = swap(native, "\tassist  bool\n}", "\tassist  bool\n\tallowTransfer bool\n\ttransferred bool\n}")
native = swap(native, "\tctx.assist = d.assist\n", "\tctx.assist = d.assist\n\tctx.allowTransfer = d.allowTransfer\n")
native = swap(native, "\tmcall(coroNativeStart)\n\tmp := gp.m",
    """	mcall(coroNativeStart)
	if d.allowTransfer && gp.param != nil && gp.param != unsafe.Pointer(ctx) {
		scheduler := (*stacklessCoroScheduler)(gp.param)
		gp.param = nil
		d.transferred = true
		d.context = nil
		return scheduler
	}
	mp := gp.m""")
start = """	ctx.scheduler.runTasks(true, false, initial, ctx.public, ctx.assist)
	mcall(coroNativeFinish)"""
body = """	ctx.scheduler.runTasks(true, false, initial, ctx.public, ctx.assist)
	if ctx.canTransferOwner(gp) {
		mcall(coroNativeTransferOwner)
		ctx.gState.channelHandoffs = 0
		ctx.scheduler.runTasks(true, false, false, false, false)
	}
	mcall(coroNativeFinish)"""
native = swap(native, start, body)
native = swap(native, "\tgogo(&caller.sched)\n}", """	if ctx.transferredOwner {
		acquirem()
	}
	gogo(&caller.sched)
}""")
native += """
func (ctx *stacklessCoroNativeContext) canTransferOwner(executor *g) bool {
	if !ctx.allowTransfer || !ctx.public || gomaxprocs <= 1 ||
		ctx.deferredYield || ctx.deferredOperation != nil || ctx.deferredSleepWhen != 0 ||
		ctx.lockedG != 0 || ctx.lockedInt != 0 || executor.m.lockedExt != 0 ||
		executor.m.lockedInt != 1 || ctx.caller.lockedm != 0 || ctx.caller.bubble != nil ||
		!ctx.scheduler.spawned() || ctx.scheduler.drained() {
		return false
	}
	return ctx.scheduler.lifetime.Load()&stacklessCoroLifetimePublicHost == 0
}

var stacklessCoroTransferredOwnerEntry funcval

func init() {
	stacklessCoroTransferredOwnerEntry.fn = abi.FuncPCABIInternal(runStacklessCoroTransferredOwner)
}

// The new owner stays parked until this native activation ends. The public
// caller can resume on another M and wait for its own logical root.
func coroNativeTransferOwner(executor *g) {
	ctx := stacklessCoroNativeContextFor(executor)
	caller := ctx.caller
	owner := newproc1(&stacklessCoroTransferredOwnerEntry, caller, sys.GetCallerPC(), true, waitReasonCoroutine)
	owner.param = unsafe.Pointer(ctx)

	ctx.public = false
	ctx.allowTransfer = false
	ctx.transferredOwner = true
	ctx.caller = owner
	caller.param = unsafe.Pointer(ctx.scheduler)
	ready(caller, 1, true)
	gogo(&executor.sched)
}

func runStacklessCoroTransferredOwner() {
	gp := getg()
	ctx := (*stacklessCoroNativeContext)(gp.param)
	mp := gp.m
	if ctx == nil || ctx.caller != gp || mp.curg != gp ||
		mp.g0 != ctx.schedulerG || ctx.schedulerG.m != mp || ctx.nativeG0 == nil {
		println("transferred owner", ctx != nil, mp.curg == gp)
		if ctx != nil {
			println("context", ctx.caller == gp, mp.g0 == ctx.schedulerG, ctx.schedulerG.m == mp, ctx.nativeG0 != nil)
		}
		throw("runtime: invalid transferred stackless coroutine owner")
	}
	mp.g0 = ctx.nativeG0
	mp.g0StackAccurate = ctx.g0Accurate
	ctx.schedulerG.m = nil
	ctx.nativeG0 = nil
	ctx.g0Accurate = false
	ctx.transferredOwner = false
	gp.param = nil
	s := ctx.scheduler
	ctx.scheduler = nil
	ctx.caller = nil
	releasem(mp)

	native := stacklessCoroNativeDriver{context: ctx}
	if !s.drained() && (ctx.deferredYield || ctx.deferredSleepWhen != 0 || s.runnableState.Load() == 0) {
		native.waitForWork(s)
	}
	s.runOwnerWithDriver(native)
}
"""
stub = stub_path.read_text().replace("type stacklessCoroNativeDriver struct{}", "type stacklessCoroNativeDriver struct { allowTransfer, transferred bool }")
for mode, native_file in (("handoff", native), ("handoffcontrol", swap(native, body, start))):
    mapping = {}
    for file, data in ((core_path, core), (native_path, native_file), (stub_path, stub)):
        dest = root / "overlays" / f"{mode}-{file.name}"
        dest.write_text(data)
        mapping[str(file)] = str(dest)
    (root / "overlays" / f"{mode}.json").write_text(json.dumps({"Replace": mapping}))
