import json
from pathlib import Path

root = Path(__file__).resolve().parent
runtime = root / "sdk/src/runtime"
scheduler = "coro_stackless.go"
assistance = "coro_stackless_assist.go"
base = (runtime / scheduler).read_text()
assist = (runtime / assistance).read_text()


def replace(source, old, new):
    assert source.count(old) == 1, old
    return source.replace(old, new)


def save(mode, files):
    mapping = {}
    for name, source in files.items():
        target = root / "overlays" / f"{mode}-{name}"
        target.write_text(source)
        mapping[str(runtime / name)] = str(target)
    (root / "overlays" / f"{mode}.json").write_text(json.dumps({"Replace": mapping}))


all_p = replace(base, "\t\tcount := uint32(min(gomaxprocs, 3))",
                "\t\tcount := uint32(min(gomaxprocs, stacklessCoroMaxWarmExecutors))")
save("probefull", {scheduler: all_p})

# A manager and the first measured executor can concurrently discover demand.
# Never replace already-published lanes, since ready work can be on them.
assist = replace(assist, """	dispatch := &stacklessCoroDispatchLanes{
		assistant: r.assistant(),
		lanes:     make([]stacklessCoroAssistLane, min(gomaxprocs, stacklessCoroMaxWarmExecutors)),
	}
	for i := range dispatch.lanes {
		lane := &dispatch.lanes[i]
		lockInit(&lane.lock, lockRankStacklessCoroCache)
		lane.wide = true
	}
	r.dispatch.Store(dispatch)
""", """	for {
		previous := r.dispatch.Load()
		if previous != nil && len(previous.lanes) != 0 {
			return
		}
		dispatch := &stacklessCoroDispatchLanes{
			lanes: make([]stacklessCoroAssistLane, min(gomaxprocs, stacklessCoroMaxWarmExecutors)),
		}
		if previous != nil {
			dispatch.assistant = previous.assistant
		}
		for i := range dispatch.lanes {
			lane := &dispatch.lanes[i]
			lockInit(&lane.lock, lockRankStacklessCoroCache)
			lane.wide = true
		}
		if r.dispatch.CompareAndSwap(previous, dispatch) {
			return
		}
	}
""")


def initialize_lanes(source, route_owner):
    source = replace(source, """			for range count - 1 {
				go s.replacementExecutor(true)
			}
		}
		select {
		case r.wake <- struct{}{}:
""", """			r.enableDispatchBatching()
			for range count - 1 {
				go s.replacementExecutor(true)
			}
		}
		select {
		case r.wake <- struct{}{}:
""")
    source = replace(source, """				replacements.dispatch.Store(&pilot.dispatch)
				s.executorCount.Add(1)
				go s.runReplacementExecutor(true, true)
""", """				if replacements.dispatch.CompareAndSwap(nil, &pilot.dispatch) {
					s.executorCount.Add(1)
					go s.runReplacementExecutor(true, true)
				} else {
					lane = nil
				}
""")
    if route_owner:
        source = replace(source, """	samples      int
}""", """	samples      int
	openLanes    bool
}""")
        source = replace(source, """			r.enableDispatchBatching()
			for range count - 1""", """			r.enableDispatchBatching()
			probe.openLanes = true
			for range count - 1""")
        source = replace(source, """		next, resumeDirectly, foreignReturner := s.yield(task, native, dispatch)""", """		if probe.openLanes {
			probe.openLanes = false
			lane = s.acquireDispatchLane()
			dispatch = lane == nil && dispatch
		}
		next, resumeDirectly, foreignReturner := s.yield(task, native, dispatch)""")
    return source


for mode, source, owner in (
    ("probelanes", base, False),
    ("probewide", all_p, True),
    ("probesmall", base, True),
    ("probeeager", all_p.replace("const sustainedWork = 1_200", "const sustainedWork = 600"), True),
):
    save(mode, {scheduler: initialize_lanes(source, owner), assistance: assist})
