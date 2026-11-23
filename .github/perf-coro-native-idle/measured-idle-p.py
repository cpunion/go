import json
import sys
from pathlib import Path


sdk = Path(sys.argv[1]).resolve()
directory = Path(sys.argv[2]).resolve()
directory.mkdir(parents=True, exist_ok=True)
file = sdk / "src/runtime/coro_stackless.go"
original = file.read_text()

old = """\t\tif count >= 2 && s.lifetime.Load()&stacklessCoroLifetimeDeferredExecutors != 0 &&
			s.executorCount.CompareAndSwap(1, count) {
			for range count - 1 {
				go s.replacementExecutor(true)
			}
		}"""
new = """\t\tif count >= 2 && s.lifetime.Load()&stacklessCoroLifetimeDeferredExecutors != 0 &&
			s.executorCount.CompareAndSwap(1, count) {
			if gomaxprocs >= 4 && elapsed >= 2*sustainedWork {
				s.startObservedCoroutineWorkers(count - 1)
			} else {
				for range count - 1 {
					go s.replacementExecutor(true)
				}
			}
		}"""
assert original.count(old) == 1
template = original.replace(old, new) + """
var stacklessCoroObservedWorkerEntry funcval

func init() {
	stacklessCoroObservedWorkerEntry.fn = abi.FuncPCABIInternal(runStacklessCoroObservedWorker)
}

func runStacklessCoroObservedWorker() {
	gp := getg()
	s := (*stacklessCoroScheduler)(gp.param)
	gp.param = nil
	s.replacementExecutor(true)
}

func (s *stacklessCoroScheduler) startObservedCoroutineWorkers(count uint32) {
	caller := getg()
	pc := sys.GetCallerPC()
	systemstack(func() {
		local := getg().m.p.ptr()
		queued := false
		for i := uint32(0); i < count; i++ {
			gp := newproc1(&stacklessCoroObservedWorkerEntry, caller, pc, false, waitReasonZero)
			gp.param = unsafe.Pointer(s)
			PLACE
			runqput(local, gp, true)
			queued = true
		}
		if queued && mainStarted {
			wakep()
		}
	})
}
"""
place = """if i == 0 {
				lock(&sched.lock)
				var idle *p
				if !sched.gcwaiting.Load() && WANT_IDLE {
					idle, _ = pidleget(0)
				}
				if idle != nil {
					if !runqempty(idle) {
						throw("observed coroutine worker reserved a busy P")
					}
					runqput(idle, gp, true)
					NOTICE
					startm(idle, false, true)
				}
				unlock(&sched.lock)
				if idle != nil {
					continue
				}
			}"""
for mode in ("localwake", "reusem", "startm", "proofreusem", "proofstartm"):
    text = ""
    if mode != "localwake":
        want = "sched.nmidle > 0" if mode.endswith("reusem") else "true"
        notice = ('println("coro-observed-idle", sched.nmidle > 0)' if mode.startswith("proof") else "")
        text = place.replace("WANT_IDLE", want).replace("NOTICE", notice)
    target = directory / (mode + "-coro_stackless.go")
    target.write_text(template.replace("PLACE", text))
    (directory / (mode + ".json")).write_text(json.dumps({"Replace": {str(file): str(target)}}))
