import json
import sys
from pathlib import Path


root = Path(__file__).resolve().parent
sdk = Path(sys.argv[1]).resolve()
overlay = Path(sys.argv[2]).resolve()
overlay.mkdir(parents=True, exist_ok=True)
updates = {}
runtime = sdk / "src/runtime"


def patch(name, replacements, trailer=None):
    path = runtime / name
    source = path.read_text()
    for old, new, count in replacements:
        assert source.count(old) == count, (name, old, source.count(old))
        source = source.replace(old, new)
    if trailer is not None:
        source += trailer
    target = overlay / name
    target.write_text(source)
    updates[str(path)] = str(target)


patch("coro_stackless.go", (
    ("\ts := acquireStacklessCoroRootScheduler(frame, resume, frameSize)",
     "\tstart := nanotime()\n\ts := acquireStacklessCoroRootScheduler(frame, resume, frameSize)\n\ts.coldTraceAt(0, nil, 0, start)\n\ts.coldTrace(1, nil, 0)", 1),
    ("\ts.registerPublicGoroutine(sys.GetCallerPC(), unsafe.Pointer(sys.GetCallerSP()))",
     "\ts.registerPublicGoroutine(sys.GetCallerPC(), unsafe.Pointer(sys.GetCallerSP()))\n\ts.coldTrace(12, &s.root, 0)", 1),
    ("func (s *stacklessCoroScheduler) runOwner() {",
     "func (s *stacklessCoroScheduler) runOwner() {\n\ts.coldTrace(4, nil, 0)", 1),
    ("func (s *stacklessCoroScheduler) manageReplacementExecutors() {",
     "func (s *stacklessCoroScheduler) manageReplacementExecutors() {\n\ts.coldTrace(5, nil, 0)", 1),
    ("func (s *stacklessCoroScheduler) runReplacementExecutor(start, assist bool) {",
     "func (s *stacklessCoroScheduler) runReplacementExecutor(start, assist bool) {\n\ts.coldTrace(6, nil, 0)", 1),
    ("resumeOwned:\n\tvar action uint8",
     "resumeOwned:\n\ts.coldTrace(8, task, 0)\n\tvar action uint8", 1),
    ("\tstate := getg().stacklessCoro\n\t// Each chunk switch",
     "\ts.coldTrace(9, task, uint32(action))\n\tstate := getg().stacklessCoro\n\t// Each chunk switch", 1),
    ("\t\tr.workProbe.Store(stacklessCoroWorkParallel)",
     "\t\tr.workProbe.Store(stacklessCoroWorkParallel)\n\t\ts.coldTrace(10, task, uint32(elapsed))", 1),
    ("\t\tclose(s.rootDone)",
     "\t\ts.coldTrace(11, &s.root, 0)\n\t\tclose(s.rootDone)", 1),
    ("\ts.executorState.Store(stacklessCoroExecutorStateRunning)",
     "\ts.executorState.Store(stacklessCoroExecutorStateRunning)\n\ts.coldTrace(3, nil, s.executorCount.Load())", 1),
), """
type stacklessCoroColdEvent struct {
	at int64
	scheduler, task uintptr
	value uint32
	kind uint8
	root bool
}

var stacklessCoroColdTraces [16]struct {
	count atomic.Uint32
	events [4096]stacklessCoroColdEvent
	pad [64]byte
}

func (s *stacklessCoroScheduler) coldTrace(kind uint8, task *stacklessCoroTask, value uint32) {
	s.coldTraceAt(kind, task, value, nanotime())
}

func (s *stacklessCoroScheduler) coldTraceAt(kind uint8, task *stacklessCoroTask, value uint32, at int64) {
	mp := acquirem()
	pp := mp.p.ptr()
	if pp != nil && uint32(pp.id) < uint32(len(stacklessCoroColdTraces)) {
		trace := &stacklessCoroColdTraces[pp.id]
		next := trace.count.Load()
		if next < uint32(len(trace.events)) {
			trace.events[next] = stacklessCoroColdEvent{at, uintptr(unsafe.Pointer(s)),
				uintptr(unsafe.Pointer(task)), value, kind, task == &s.root}
			trace.count.Store(next + 1)
		}
	}
	releasem(mp)
}

//go:linkname dumpStacklessCoroColdTrace
func dumpStacklessCoroColdTrace() {
	for i := range stacklessCoroColdTraces {
		trace := &stacklessCoroColdTraces[i]
		n := trace.count.Load()
		for _, event := range trace.events[:n] {
			println("coro-cold", i, event.kind, event.scheduler, event.task, event.root, event.value, event.at)
		}
	}
}
""")

patch("coro_stackless_spawn_batch.go", (
    ("\ts.registerGoroutinesLocked(tasks[:count], parent, callerpc, callersp)",
     "\ts.coldTrace(16, parent, uint32(count))\n\ts.registerGoroutinesLocked(tasks[:count], parent, callerpc, callersp)\n\ts.coldTrace(17, parent, uint32(count))", 2),
    ("\tunlock(&s.lock)\n\ts.prepareReplacementExecutors()",
     "\tunlock(&s.lock)\n\ts.coldTrace(2, parent, uint32(count))\n\ts.prepareReplacementExecutors()", 2),
))

patch("coro_stackless_native.go", (
    ("\tgp.param = unsafe.Pointer(ctx)\n\tmcall(coroNativeStart)",
     """	gp.param = unsafe.Pointer(ctx)
	var episode uint32
	if initial {
		episode |= 1
	}
	if public {
		episode |= 2
	}
	s.coldTrace(13, nil, episode)
	mcall(coroNativeStart)""", 1),
    ("\tscheduler := ctx.scheduler\n\tctx.scheduler = nil",
     "\tscheduler := ctx.scheduler\n\tscheduler.coldTrace(15, nil, episode)\n\tctx.scheduler = nil", 1),
    ("\tctx.scheduler.runTasks(true, false, initial, ctx.public, ctx.assist)",
     "\tctx.scheduler.coldTrace(14, nil, 0)\n\tctx.scheduler.runTasks(true, false, initial, ctx.public, ctx.assist)", 1),
))

(overlay / "trace.json").write_text(json.dumps({"Replace": updates}))
