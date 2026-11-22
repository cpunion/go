import json
from pathlib import Path
from subprocess import check_output
import sys

root = Path(__file__).parent.resolve()
goroot = Path(check_output([str(root / "sdk/bin/go"), "env", "GOROOT"], text=True).strip())
runtime = goroot / "src/runtime"
base = (runtime / "coro_stackless.go").read_text()
native = (runtime / "coro_stackless_native.go").read_text()


def insert(source, old, new, count=1):
    assert source.count(old) == count, old
    return source.replace(old, new)


directory = root / "overlays"
directory.mkdir(exist_ok=True)


def save(name, source, native_source=None):
    changes = {"coro_stackless.go": source}
    if native_source is not None:
        changes["coro_stackless_native.go"] = native_source
    mapping = {}
    for file, contents in changes.items():
        target = directory / f"{name}-{file}"
        target.write_text(contents)
        mapping[str(runtime / file)] = str(target)
    (directory / f"{name}.json").write_text(json.dumps({"Replace": mapping}))


launch = """			for range count - 1 {
				go s.replacementExecutor(true)
			}"""

batch = """
func runMeasuredCoroReplacement() {
	gp := getg()
	s := (*stacklessCoroScheduler)(gp.param)
	gp.param = nil
	s.replacementExecutor(true)
}

func (s *stacklessCoroScheduler) startMeasuredReplacements(count uint32) {
	callerg := getg()
	callerpc := sys.GetCallerPC()
	entry := &funcval{fn: abi.FuncPCABIInternal(runMeasuredCoroReplacement)}
	systemstack(func() {
		var list gList
		for range count {
			gp := newproc1(entry, callerg, callerpc, true, waitReasonCoroutine)
			gp.param = unsafe.Pointer(s)
			list.push(gp)
		}
		injectglist(&list)
	})
}
"""
count3 = "count := uint32(min(gomaxprocs, 3))"
save("allp", insert(base, count3, "count := uint32(min(gomaxprocs, 4))"))
b = insert(base, launch, "			s.startMeasuredReplacements(count - 1)") + batch
save("batchwake", b)
save("batchall", insert(b, count3, "count := uint32(min(gomaxprocs, 4))"))

managed = """
func (s *stacklessCoroScheduler) runMeasuredReplacement() {
	s.run(false, false)
	s.executorDone <- false
}
"""
save("managed", insert(base, launch, launch.replace("s.replacementExecutor(true)", "s.runMeasuredReplacement()")) + managed)

owner = """
func runMeasuredCoroOwner() {
	gp := getg()
	s := (*stacklessCoroScheduler)(gp.param)
	gp.param = nil
	s.runOwner()
}

func (s *stacklessCoroScheduler) scheduleCoroOwner() {
	callerg := getg()
	callerpc := sys.GetCallerPC()
	entry := &funcval{fn: abi.FuncPCABIInternal(runMeasuredCoroOwner)}
	systemstack(func() {
		var list gList
		gp := newproc1(entry, callerg, callerpc, true, waitReasonCoroutine)
		gp.param = unsafe.Pointer(s)
		list.push(gp)
		injectglist(&list)
	})
}
"""
save("ownerwake", insert(base, "\t\tgo s.runOwner()",
    "\t\ts.scheduleCoroOwner()", count=2) + owner)

if "phase" not in sys.argv:
    exit()

phase = base
for old, new, occurrences in [
    ("\ts.registerPublicGoroutine(sys.GetCallerPC(), unsafe.Pointer(sys.GetCallerSP()))", '\tcoroColdRecord("root-entry", s)\n\ts.registerPublicGoroutine(sys.GetCallerPC(), unsafe.Pointer(sys.GetCallerSP()))', 1),
    ("\ts = runStacklessCoroRoot(s)", '\ts = runStacklessCoroRoot(s)\n\tcoroColdRecord("root-initial-done", s)', 1),
    ("\t\tgo s.runOwner()", '\t\tcoroColdRecord("owner-created", s)\n\t\tgo s.runOwner()', 2),
    ("func (s *stacklessCoroScheduler) runOwner() {", 'func (s *stacklessCoroScheduler) runOwner() {\n\tcoroColdRecord("owner-running", s)', 1),
    ("func (s *stacklessCoroScheduler) startReplacementExecutors(deferWorkers bool) {", 'func (s *stacklessCoroScheduler) startReplacementExecutors(deferWorkers bool) {\n\tcoroColdRecord("replacement-setup", s)', 1),
    ("func (s *stacklessCoroScheduler) manageReplacementExecutors() {", 'func (s *stacklessCoroScheduler) manageReplacementExecutors() {\n\tcoroColdRecord("manager-running", s)', 1),
    ("\t\tr.workProbe.Store(stacklessCoroWorkParallel)", '\t\tcoroColdRecord("measured-work", s)\n\t\tr.workProbe.Store(stacklessCoroWorkParallel)', 1),
    (launch, '\t\t\tcoroColdRecord("helper-create-start", s)\n' + launch + '\n\t\t\tcoroColdRecord("helper-create-done", s)', 1),
    ("func (s *stacklessCoroScheduler) replacementExecutor(start bool) {", 'func (s *stacklessCoroScheduler) replacementExecutor(start bool) {\n\tcoroColdRecord("replacement-running", s)', 1),
    ("resumeOwned:\n", 'resumeOwned:\n\tif task != &s.root { coroColdRecord("child-resume", s) }\n', 1),
    ("\t\tclose(s.rootDone)", '\t\tcoroColdRecord("root-complete", s)\n\t\tclose(s.rootDone)', 1),
]:
    phase = insert(phase, old, new, occurrences)

phase_native = insert(native, "\tif d.context == nil {", '\tcoroColdRecord("driver-entry", s)\n\tif d.context == nil {')
phase_native = insert(phase_native, "\t\td.context = acquireStacklessCoroNativeContext()", '\t\td.context = acquireStacklessCoroNativeContext()\n\t\tcoroColdRecord("context-ready", s)')
phase_native = insert(phase_native, "\tgp.param = unsafe.Pointer(ctx)\n\tmcall(coroNativeStart)", '\tcoroColdRecord("driver-before-switch", s)\n\tgp.param = unsafe.Pointer(ctx)\n\tmcall(coroNativeStart)')

phase += """
var coroColdEvents struct {
	next atomic.Uint32
	rows [1024]struct {
		at atomic.Int64
		label string
		scheduler uintptr
		thread int64
	}
}

func coroColdRecord(label string, s *stacklessCoroScheduler) {
	n := coroColdEvents.next.Add(1)
	if n > uint32(len(coroColdEvents.rows)) {
		return
	}
	row := &coroColdEvents.rows[n-1]
	row.label = label
	row.scheduler = uintptr(unsafe.Pointer(s))
	row.thread = getg().m.id
	row.at.Store(nanotime())
}

//go:linkname coroColdDump
func coroColdDump() {
	for i := range min(coroColdEvents.next.Load(), uint32(len(coroColdEvents.rows))) {
		row := &coroColdEvents.rows[i]
		if at := row.at.Load(); at != 0 {
			println("coro-cold-phase", at, row.scheduler, row.thread, row.label)
		}
	}
}
"""
save("phase", phase, phase_native)
