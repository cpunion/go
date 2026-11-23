import json
from pathlib import Path
from subprocess import check_output

root = Path(__file__).parent.resolve()
goroot = Path(check_output([str(root / "sdk/bin/go"), "env", "GOROOT"], text=True).strip())
runtime = goroot / "src/runtime"
main = "coro_stackless.go"
core = "proc.go"
sources = (runtime / main).read_text()
process = (runtime / core).read_text()

def replace(text, old, new):
    assert text.count(old) == 1, old
    return text.replace(old, new)

sources = replace(sources,
    "\tpendingManager      atomic.Pointer[stacklessCoroScheduler]\n\tmanagerTimer        *timeTimer",
    "\tmanagerQueued       atomic.Bool")
sources = replace(sources, """	// Launch the manager after the public driver has handed off. This lets
	// the owner become runnable without first waiting to wake the manager.
	s.startPendingReplacementManager()
""", "")
begin = sources.index("\t\t// Usually the public driver hands off as soon as this resume ends.")
end = sources.index("\n\t} else {", begin)
sources = sources[:begin] + "\t\ts.queueReplacementManager()" + sources[end:]
begin = sources.index("//go:noinline\nfunc (s *stacklessCoroScheduler) startPendingReplacementManager()")
end = sources.index("// awaitExecutorDemand", begin)
sources = sources[:begin] + """var stacklessCoroManagerEntry = funcval{fn: abi.FuncPCABIInternal(runStacklessCoroQueuedManager)}

func (s *stacklessCoroScheduler) queueReplacementManager() {
	gp := getg()
	pc := sys.GetCallerPC()
	systemstack(func() {
		manager := newproc1(&stacklessCoroManagerEntry, gp, pc, false, waitReasonZero)
		manager.param = unsafe.Pointer(s)
		s.replacements.Load().managerQueued.Store(true)
		coroQueuedManagers.Add(1)
		runqput(getg().m.p.ptr(), manager, false)
		armStacklessCoroManagerWake()
	})
}

func runStacklessCoroQueuedManager() {
	gp := getg()
	s := (*stacklessCoroScheduler)(gp.param)
	gp.param = nil
	s.replacements.Load().managerQueued.Store(false)
	if coroQueuedManagers.Add(-1) < 0 {
		throw("runtime: invalid queued stackless coroutine manager count")
	}
	s.manageReplacementExecutors()
}

func armStacklessCoroManagerWake() {
	const delay = 100_000
	deadline := nanotime() + delay
	for previous := coroManagerWakeDeadline.Load(); previous == 0 || deadline < previous; previous = coroManagerWakeDeadline.Load() {
		if coroManagerWakeDeadline.CompareAndSwap(previous, deadline) {
			return
		}
	}
}

""" + sources[end:]
sources = replace(sources, """		if replacements.pendingManager.Load() != nil {
			s.startPendingReplacementManager()
		}""", """		if replacements.managerQueued.Load() {
			wakeStacklessCoroQueuedManager()
		}""")
sources = replace(sources, "\ts.stopExecutorRuns()\n\ts.startPendingReplacementManager()", "\ts.stopExecutorRuns()")
sources += """
//go:noinline
func wakeStacklessCoroQueuedManager() {
	wakep()
}
"""
process = replace(process, "// forcegcperiod is the maximum time", """// Coroutine manager launches publish runnable work before arming this deadline.
// The system monitor supplies a wake if the public driver continues running.
var coroManagerWakeDeadline atomic.Int64
var coroQueuedManagers atomic.Int32

// forcegcperiod is the maximum time""")
process = replace(process, """		now := nanotime()
		if debug.schedtrace""", """		now := nanotime()
		if goexperiment.Coro {
			if deadline := coroManagerWakeDeadline.Load(); deadline != 0 &&
				deadline <= now && coroManagerWakeDeadline.CompareAndSwap(deadline, 0) &&
				coroQueuedManagers.Load() != 0 {
				wakep()
			}
		}
		if debug.schedtrace""")

for mode, placed in (
    ("tail", "runqput(getg().m.p.ptr(), manager, false)"),
    ("next", "runqput(getg().m.p.ptr(), manager, true)"),
    ("global", "lock(&sched.lock)\n\t\tglobrunqput(manager)\n\t\tunlock(&sched.lock)"),
    ("eager", "runqput(getg().m.p.ptr(), manager, false)\n\t\twakep()"),
):
    mapping = {}
    candidate = replace(sources, "runqput(getg().m.p.ptr(), manager, false)", placed)
    for file, payload in ((main, candidate), (core, process)):
        dest = root / "overlays" / f"{mode}-{file}"
        dest.write_text(payload)
        mapping[str(runtime / file)] = str(dest)
    (root / "overlays" / f"{mode}.json").write_text(json.dumps({"Replace": mapping}))
