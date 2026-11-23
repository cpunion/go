import json
from pathlib import Path

root = Path(__file__).resolve().parent
runtime = root / "sdk/src/runtime"


def replace(source, old, new, count=1):
    assert source.count(old) == count, old
    return source.replace(old, new)


def save(mode, files):
    mapping = {}
    for name, source in files.items():
        target = root / "overlays" / f"{mode}-{name}"
        target.write_text(source)
        mapping[str(runtime / name)] = str(target)
    (root / "overlays" / f"{mode}.json").write_text(json.dumps({"Replace": mapping}))


sched = "coro_stackless.go"
spawn = "coro_stackless_spawn_batch.go"
fixed = "coro_stackless_native.go"
stub = "coro_stackless_native_stub.go"
files = {name: (runtime / name).read_text() for name in (sched, spawn, fixed, stub)}

fixed_body = files[fixed] + """
func (d *stacklessCoroNativeDriver) prefetch() {
	d.context = acquireStacklessCoroNativeContext()
}
"""
stub_body = files[stub] + "\nfunc (d *stacklessCoroNativeDriver) prefetch() {}\n"
scheduler = replace(files[sched], "\trootDone chan struct{}\n",
                    "\trootDone chan struct{}\n\tpreparedOwner chan bool // written and delivered by the public root\n")
scheduler = replace(scheduler, "go s.runOwner()", "s.startPreparedOwner()", count=2)
scheduler = replace(scheduler, """		if !s.drained() {
			s.startPreparedOwner()
		}
""", """		if !s.drained() {
			s.startPreparedOwner()
		} else {
			s.cancelPreparedOwner()
		}
""")
scheduler = replace(scheduler, """	s.stopReplacementExecutors()
	cacheStacklessCoroDrainedTasks(s)
	return finishStacklessCoroRootScheduler(s)
""", """	s.cancelPreparedOwner()
	s.stopReplacementExecutors()
	cacheStacklessCoroDrainedTasks(s)
	return finishStacklessCoroRootScheduler(s)
""")

owner_call = """func (s *stacklessCoroScheduler) runOwner() {
"""
scheduler = replace(scheduler, owner_call, """func (s *stacklessCoroScheduler) runOwner() {
	s.runOwnerDriver(stacklessCoroNativeDriver{})
}

func (s *stacklessCoroScheduler) prepareOwner() {
	deliver := make(chan bool, 1)
	s.preparedOwner = deliver
	go s.runPreparedOwner(deliver)
}

func (s *stacklessCoroScheduler) runPreparedOwner(deliver <-chan bool) {
	var native stacklessCoroNativeDriver
	if <-deliver {
		s.runOwnerDriver(native)
	} else {
		// The caller can pool its drained scheduler. Releasing inactive
		// context storage must not dereference the caller's scheduler.
		native.close(nil, false)
	}
}

func (s *stacklessCoroScheduler) startPreparedOwner() {
	deliver := s.preparedOwner
	if deliver == nil {
		go s.runOwner()
		return
	}
	s.preparedOwner = nil
	deliver <- true
}

func (s *stacklessCoroScheduler) cancelPreparedOwner() {
	if deliver := s.preparedOwner; deliver != nil {
		s.preparedOwner = nil
		close(deliver)
	}
}

func (s *stacklessCoroScheduler) runOwnerDriver(native stacklessCoroNativeDriver) {
""")
scheduler = replace(scheduler, """	var native stacklessCoroNativeDriver
	for !s.drained() {
		next := native.run(s, false, false)
""", """	for !s.drained() {
		next := native.run(s, false, false)
""")

spawn_source = files[spawn]
scalar, typed = spawn_source.split("// coroSpawnFrameBatch", 1)
before = """	s := context.scheduler
	lock(&s.lock)
"""
enabled = replace(scalar, before, """	s := context.scheduler
	// While the root is still private, start building an ordinary owner
	// independently of its initial batch's frame and registry work.
	if !raceenabled && gomaxprocs > 1 && parent == &s.root && s.lifetime.Load() == 0 &&
		getg().stackIsFixed() && remaining >= 8 && remaining <= 64 {
		s.prepareOwner()
	}
	lock(&s.lock)
""") + "// coroSpawnFrameBatch" + typed
save("preparenone", {sched: scheduler, spawn: spawn_source})
save("prepareg", {sched: scheduler, spawn: enabled})
prefetch = replace(scheduler,
    """	var native stacklessCoroNativeDriver
	if <-deliver {""",
    """	var native stacklessCoroNativeDriver
	native.prefetch()
	if <-deliver {""")
save("preparecontext", {sched: prefetch, spawn: enabled, fixed: fixed_body, stub: stub_body})
full_prefetch = replace(fixed_body, """func (d *stacklessCoroNativeDriver) prefetch() {
	d.context = acquireStacklessCoroNativeContext()
}""", """func (d *stacklessCoroNativeDriver) prefetch() {
	d.context = acquireStacklessCoroNativeContext()
	if d.context.schedulerG == nil {
		d.context.schedulerG = acquireStacklessCoroNativeSchedulerStack()
	}
}""")
save("preparestack", {sched: prefetch, spawn: enabled, fixed: full_prefetch, stub: stub_body})
