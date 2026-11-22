import json
from pathlib import Path
from subprocess import check_output

root = Path(__file__).parent.resolve()
goroot = Path(check_output([str(root / "sdk/bin/go"), "env", "GOROOT"], text=True).strip())
runtime = goroot / "src/runtime"

def insert(source, marker, new, count=1):
    assert source.count(marker) == count, marker
    return source.replace(marker, new)

base = (runtime / "coro_stackless.go").read_text()
native = (runtime / "coro_stackless_native.go").read_text()
proc = (runtime / "proc.go").read_text()

for before, after, count in (
    ("\ts.registerPublicGoroutine(sys.GetCallerPC(), unsafe.Pointer(sys.GetCallerSP()))",
     "\tcoroRoutePhase(1, s)\n\ts.registerPublicGoroutine(sys.GetCallerPC(), unsafe.Pointer(sys.GetCallerSP()))\n\tcoroRoutePhase(2, s)", 1),
    ("\ts = runStacklessCoroRoot(s)", "\ts = runStacklessCoroRoot(s)\n\tcoroRoutePhase(7, s)", 1),
    ("\t\tgo s.runOwner()", "\t\tcoroRoutePhase(8, s)\n\t\tgo s.runOwner()\n\t\tcoroRoutePhase(9, s)", 2),
    ("func (s *stacklessCoroScheduler) runOwner() {",
     "func (s *stacklessCoroScheduler) runOwner() {\n\tcoroRoutePhase(10, s)", 1),
    ("\t\treplacements.managerTimer = newTimer(nanotime()+delay, 0, startStacklessCoroTimedManager, replacements, nil)",
     "\t\tcoroRoutePhase(4, s)\n\t\treplacements.managerTimer = newTimer(nanotime()+delay, 0, startStacklessCoroTimedManager, replacements, nil)\n\t\tcoroRoutePhase(5, s)", 1),
    ("\t\tstopTimer(r.managerTimer)\n\t\tgo s.manageReplacementExecutors()",
     "\t\tstopTimer(r.managerTimer)\n\t\tcoroRoutePhase(11, s)\n\t\tgo s.manageReplacementExecutors()\n\t\tcoroRoutePhase(12, s)", 1),
    ("\t\tr.workProbe.Store(stacklessCoroWorkParallel)", "\t\tcoroRoutePhase(14, s)\n\t\tr.workProbe.Store(stacklessCoroWorkParallel)", 1),
    ("resumeOwned:\n", "resumeOwned:\n\tif task != &s.root { coroRoutePhase(13, s) }\n", 1),
    ("\t\tclose(s.rootDone)", "\t\tcoroRoutePhase(15, s)\n\t\tclose(s.rootDone)", 1),
    ("\ts.executorState.Store(stacklessCoroExecutorStateRunning)",
     "\ts.executorState.Store(stacklessCoroExecutorStateRunning)\n\tcoroRoutePhase(6, s)", 1),
):
    base = insert(base, before, after, count)
native = insert(native, "\tif d.context == nil {", "\tcoroRoutePhase(3, s)\n\tif d.context == nil {")
proc = insert(proc, "func newm(fn func(), pp *p, id int64) {",
    "func newm(fn func(), pp *p, id int64) {\n\tcoroRoutePhase(16, nil)")
proc = insert(proc, "\tnewm1(mp)\n\treleasem(getg().m)",
    "\tnewm1(mp)\n\tcoroRoutePhase(17, nil)\n\treleasem(getg().m)")
proc = insert(proc, "\t\treleasem(getg().m)\n\t\treturn\n\t}\n\tnewm1(mp)",
    "\t\tcoroRoutePhase(18, nil)\n\t\treleasem(getg().m)\n\t\treturn\n\t}\n\tnewm1(mp)")

base += """
var coroRoutePhases struct {
	next atomic.Uint32
	rows [2048]struct {
		at atomic.Int64
		scheduler uintptr
		thread int64
		kind uint8
	}
}

func coroRoutePhase(kind uint8, s *stacklessCoroScheduler) {
	n := coroRoutePhases.next.Add(1)
	if n > uint32(len(coroRoutePhases.rows)) {
		return
	}
	row := &coroRoutePhases.rows[n-1]
	row.scheduler = uintptr(unsafe.Pointer(s))
	if gp := getg(); gp != nil && gp.m != nil {
		row.thread = gp.m.id
	} else {
		row.thread = -1
	}
	row.kind = kind
	row.at.Store(nanotime())
}

//go:linkname coroColdDump
func coroColdDump() {
	for i := range min(coroRoutePhases.next.Load(), uint32(len(coroRoutePhases.rows))) {
		row := &coroRoutePhases.rows[i]
		if at := row.at.Load(); at != 0 {
			println("coro-routing-phase", at, row.scheduler, row.thread, row.kind)
		}
	}
}
"""
mapping = {}
for name, source in (("coro_stackless.go", base), ("coro_stackless_native.go", native), ("proc.go", proc)):
    dest = root / "overlays" / f"phase-{name}"
    dest.write_text(source)
    mapping[str(runtime / name)] = str(dest)
(root / "overlays/phase.json").write_text(json.dumps({"Replace": mapping}))
