import json
from pathlib import Path
from subprocess import check_output

root = Path(__file__).parent.resolve()
goroot = Path(check_output([str(root / "sdk/bin/go"), "env", "GOROOT"], text=True).strip())
source = goroot / "src/runtime/coro_stackless.go"
base = source.read_text()

def replace(text, old, new):
    assert text.count(old) == 1, old
    return text.replace(old, new)

reuse = replace(base, """		replacements.pendingManager.Store(s)
		const delay = 100_000 // nanoseconds
		replacements.managerTimer = newTimer(nanotime()+delay, 0, startStacklessCoroTimedManager, replacements, nil)""", """		t := new(timeTimer)
		t.timer.init(nil, nil)
		t.isFake = gp.bubble != nil
		t.self = t
		replacements.managerTimer = t
		replacements.pendingManager.Store(s)
		const delay = 100_000 // nanoseconds
		t.modify(nanotime()+delay, 0, startStacklessCoroTimedManager, replacements, 0)""")
reuse = replace(reuse, "\t\tstopTimer(r.managerTimer)\n\t\tgo s.manageReplacementExecutors()",
    "\t\tgo s.manageReplacementExecutors()")
reuse = replace(reuse, """	t := newTimer(started+interval, interval, func(arg any, _ uintptr, _ int64) {
		select {
		case arg.(chan struct{}) <- struct{}{}:
		default:
		}
	}, wake, nil)
	defer stopTimer(t)""", """	ring := func(arg any, _ uintptr, _ int64) {
		select {
		case arg.(chan struct{}) <- struct{}{}:
		default:
		}
	}
	t := replacements.managerTimer
	if t == nil {
		t = newTimer(started+interval, interval, ring, wake, nil)
	} else {
		t.modify(started+interval, interval, ring, wake, 0)
	}
	defer stopTimer(t)""")

def skip_drained(text, stop_pending):
    body = "	if s.drained() {\n"
    if stop_pending:
        body += """		if timer := s.replacements.Load().managerTimer; timer != nil {
			stopTimer(timer)
		}
"""
    return replace(text, "func (s *stacklessCoroScheduler) awaitExecutorDemand() bool {\n",
                   "func (s *stacklessCoroScheduler) awaitExecutorDemand() bool {\n" +
                   body + "		return false\n	}\n")

for name, body in (("timerreuse", reuse), ("timerreuseempty", skip_drained(reuse, True)),
                   ("timerempty", skip_drained(base, False))):
    target = root / "overlays" / f"{name}-{source.name}"
    target.write_text(body)
    (root / "overlays" / (name + ".json")).write_text(json.dumps({"Replace": {str(source): str(target)}}))
