import json
from pathlib import Path

root = Path(__file__).resolve().parent
runtime = root / "sdk/src/runtime"
name = "coro_stackless.go"
base = (runtime / name).read_text()


def replace(source, old, new):
    assert source.count(old) == 1, old
    return source.replace(old, new)


def save(mode, source):
    target = root / "overlays" / f"{mode}-{name}"
    target.write_text(source)
    config = {"Replace": {str(runtime / name): str(target)}}
    (root / "overlays" / f"{mode}.json").write_text(json.dumps(config))


owner_entry = """	// Launch the manager after the public driver has handed off. This lets
	// the owner become runnable without first waiting to wake the manager.
	s.startPendingReplacementManager()
"""
defer_owner = replace(base, owner_entry, """	// The watchdog or a blocking foreign call still starts the manager
	// when a child needs progress from another executor.
""")
save("deferowner", defer_owner)

defer_join = replace(defer_owner, """	s.stopExecutorRuns()
	s.startPendingReplacementManager()
	// Replacements may finish before the manager stops growing the set. Count
	// their notifications, but do not take the final count until the manager
	// has published its last possible increment through the same channel.
	var completed uint32
	for {
		if <-s.executorDone {
			break
		}
		completed++
	}
""", """	s.stopExecutorRuns()
	// A drained scheduler no longer needs growth. Claim an unstarted
	// manager using the watchdog's existing arbitration word.
	var completed uint32
	r := s.replacements.Load()
	if r.pendingManager.CompareAndSwap(s, nil) {
		stopTimer(r.managerTimer)
	} else {
		// A different caller claimed the manager. Its final message
		// publishes the last possible increment of executorCount.
		for {
			if <-s.executorDone {
				break
			}
			completed++
		}
	}
""")
save("deferjoin", defer_join)

demand = replace(defer_join, """		select {
		case r.wake <- struct{}{}:
""", """		s.startPendingReplacementManager()
		select {
		case r.wake <- struct{}{}:
""")
save("deferdemand", demand)

save("defer500us", replace(defer_join, "const delay = 100_000", "const delay = 500_000"))

before = """	var native stacklessCoroNativeDriver
	for !s.drained() {
		next := native.run(s, false, false)
"""
managed = replace(defer_join, before, """	if gomaxprocs > 1 && s.canAssist() &&
		s.runnableState.Load()&^stacklessCoroForeignReturnerBit <= 64 {
		s.run(false, false)
		s.stopReplacementExecutors()
		cacheStacklessCoroDrainedTasks(s)
		return
	}
""" + before)
save("managedowner", managed)

# Also try combining the managed owner with the existing locked FIFO
# successor selection. It still uses Go's ordinary stack and profiling.
# A bounded number of consecutive fast yields is followed by Gosched.
dispatch = replace(managed, """	var probe stacklessCoroWorkProbe
	if native && !public && !assist && gomaxprocs >= 2 &&
""", """	var probe stacklessCoroWorkProbe
	managedDispatch := !native && !public && !assist && !raceenabled &&
		gomaxprocs >= 2 && s.canAssist() && currentStacklessCoroHost() == nil &&
		!stacklessCoroIsPullComparison(s)
	if (native || managedDispatch) && !public && !assist && gomaxprocs >= 2 &&
""")
dispatch = replace(dispatch, """		if r := s.replacements.Load(); r.workProbe.Load() == stacklessCoroWorkUnmeasured {""",
    """		if r := s.replacements.Load(); r != nil && r.workProbe.Load() == stacklessCoroWorkUnmeasured {""")
dispatch = replace(dispatch, """	if native && !public {
		hostYieldCheck = 0
	}
""", """	if (native || managedDispatch) && !public {
		hostYieldCheck = 0
	}
""")
dispatch = replace(dispatch, """		dispatch := native && !public && action == stacklessCoroActionYield &&""",
    """		dispatch := (native || managedDispatch) && !public && action == stacklessCoroActionYield &&""")
dispatch = replace(dispatch, """		if !native {
			Gosched()
			break
		}
""", """		if !native {
			hostYieldCheck = 0
			Gosched()
			break
		}
""")
save("manageddispatch", dispatch)
