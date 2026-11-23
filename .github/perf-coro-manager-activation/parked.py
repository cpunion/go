import json
from pathlib import Path
from subprocess import check_output

root = Path(__file__).parent.resolve()
goroot = Path(check_output([str(root / "sdk/bin/go"), "env", "GOROOT"], text=True).strip())
runtime = goroot / "src/runtime"
sources = (runtime / "coro_stackless.go").read_text()
process = (runtime / "proc.go").read_text()
stub = (runtime / "coro_stackless_g_stub.go").read_text()

def replace(text, old, new):
    assert text.count(old) == 1, old
    return text.replace(old, new)

sources = replace(sources,
    "\tpendingManager      atomic.Pointer[stacklessCoroScheduler]\n\tmanagerTimer        *timeTimer",
    """	managerParked atomic.Bool
	managerG, managerPrev, managerNext guintptr // protected by sched.lock
	managerDeadline int64                      // protected by sched.lock""")
begin = sources.index("\t\treplacements.pendingManager.Store(s)")
end = sources.index("\n\t} else {", begin)
sources = sources[:begin] + "\t\ts.parkReplacementManager(replacements)" + sources[end:]
before = sources.index("//go:noinline\nfunc (s *stacklessCoroScheduler) startPendingReplacementManager()")
after = sources.index("// awaitExecutorDemand", before)
sources = sources[:before] + """var stacklessCoroParkedManagerEntry funcval

func init() {
	stacklessCoroParkedManagerEntry.fn = abi.FuncPCABIInternal(runStacklessCoroParkedManager)
}

func (s *stacklessCoroScheduler) parkReplacementManager(r *stacklessCoroReplacementState) {
	gp := getg()
	pc := sys.GetCallerPC()
	systemstack(func() {
		manager := newproc1(&stacklessCoroParkedManagerEntry, gp, pc, true, waitReasonCoroutine)
		manager.param = unsafe.Pointer(s)
		lock(&sched.lock)
		r.managerG.set(manager)
		r.managerDeadline = nanotime() + 100_000
		r.managerPrev = coroParkedManagerTail
		if tail := coroParkedManagerTail.ptr(); tail != nil {
			stacklessCoroParkedManagerState(tail).managerNext.set(manager)
		} else {
			coroParkedManagerHead.set(manager)
			coroParkedManagerDeadline.Store(r.managerDeadline)
		}
		coroParkedManagerTail.set(manager)
		r.managerParked.Store(true)
		unlock(&sched.lock)
	})
}

func stacklessCoroParkedManagerState(gp *g) *stacklessCoroReplacementState {
	return (*stacklessCoroScheduler)(gp.param).replacements.Load()
}

// removeStacklessCoroParkedManager unlinks a still-waiting ordinary G.
// The caller holds sched.lock and owns the next runnable transition.
func removeStacklessCoroParkedManager(r *stacklessCoroReplacementState) *g {
	gp := r.managerG.ptr()
	if gp == nil {
		return nil
	}
	if prev := r.managerPrev.ptr(); prev != nil {
		stacklessCoroParkedManagerState(prev).managerNext = r.managerNext
	} else {
		coroParkedManagerHead = r.managerNext
		if head := coroParkedManagerHead.ptr(); head != nil {
			coroParkedManagerDeadline.Store(stacklessCoroParkedManagerState(head).managerDeadline)
		} else {
			coroParkedManagerDeadline.Store(0)
		}
	}
	if next := r.managerNext.ptr(); next != nil {
		stacklessCoroParkedManagerState(next).managerPrev = r.managerPrev
	} else {
		coroParkedManagerTail = r.managerPrev
	}
	r.managerG, r.managerPrev, r.managerNext = 0, 0, 0
	r.managerParked.Store(false)
	return gp
}

//go:noinline
func (s *stacklessCoroScheduler) startPendingReplacementManager() {
	if r := s.replacements.Load(); r != nil && r.managerParked.Load() {
		lock(&sched.lock)
		gp := removeStacklessCoroParkedManager(r)
		unlock(&sched.lock)
		if gp != nil {
			goready(gp, 0)
		}
	}
}

// The system monitor can inject already constructed goroutines without a P.
// No Go pointer is stored into either the intrusive list or the run list.
func wakeStacklessCoroParkedManagers(now int64) {
	var ready gList
	lock(&sched.lock)
	for gp := coroParkedManagerHead.ptr(); gp != nil; gp = coroParkedManagerHead.ptr() {
		r := stacklessCoroParkedManagerState(gp)
		if r.managerDeadline > now {
			break
		}
		ready.push(removeStacklessCoroParkedManager(r))
	}
	unlock(&sched.lock)
	if !ready.empty() {
		injectglist(&ready)
	}
}

func runStacklessCoroParkedManager() {
	gp := getg()
	s := (*stacklessCoroScheduler)(gp.param)
	gp.param = nil
	s.manageReplacementExecutors()
}

""" + sources[after:]
sources = replace(sources, "if replacements.pendingManager.Load() != nil {",
    "if replacements.managerParked.Load() {")
process = replace(process, "// forcegcperiod is the maximum time", """// Parked native coroutine managers are indexed under sched.lock. Their
// ordinary Gs remain in allg while waiting to be made runnable.
var coroParkedManagerHead, coroParkedManagerTail guintptr
var coroParkedManagerDeadline atomic.Int64

// forcegcperiod is the maximum time""")
process = replace(process, """		now := nanotime()
		if debug.schedtrace""", """		now := nanotime()
		if goexperiment.Coro {
			if deadline := coroParkedManagerDeadline.Load(); deadline != 0 && deadline <= now {
				wakeStacklessCoroParkedManagers(now)
			}
		}
		if debug.schedtrace""")
process = replace(process, """				next := timeSleepUntil()
				if next > now""", """				next := timeSleepUntil()
				if goexperiment.Coro {
					if deadline := coroParkedManagerDeadline.Load(); deadline != 0 && deadline < next {
						next = deadline
					}
				}
				if next > now""")
sources = replace(sources, """func wakeStacklessCoroParkedManagers(now int64) {
	var ready gList""", """func wakeStacklessCoroParkedManagers(now int64) {
	// A no-P sysmon must count as running from removal through injection
	// so a simultaneous idle transition cannot misdiagnose deadlock.
	incidlelocked(-1)
	var ready gList""")
sources = replace(sources, """	if !ready.empty() {
		injectglist(&ready)
	}
}""", """	if !ready.empty() {
		injectglist(&ready)
	}
	incidlelocked(1)
}""")
process = replace(process, """	// There are no goroutines running, so we can look at the P's.""", """	if goexperiment.Coro && coroParkedManagerDeadline.Load() != 0 {
		return
	}

	// There are no goroutines running, so we can look at the P's.""")
stub += "\nfunc wakeStacklessCoroParkedManagers(int64) {}\n"
for mode, contents, flavor in (
    ("parked", sources, process),
    ("parkedfast", sources.replace("r.managerDeadline = nanotime() + 100_000",
                                   "r.managerDeadline = nanotime() + 25_000"), process),
    ("parkedslow", sources.replace("r.managerDeadline = nanotime() + 100_000",
                                   "r.managerDeadline = nanotime() + 400_000"), process),
):
    mapping = {}
    for file, data in (("coro_stackless.go", contents), ("proc.go", flavor), ("coro_stackless_g_stub.go", stub)):
        dest = root / "overlays" / f"{mode}-{file}"
        dest.write_text(data)
        mapping[str(runtime / file)] = str(dest)
    (root / "overlays" / f"{mode}.json").write_text(json.dumps({"Replace": mapping}))
