import json
from pathlib import Path
from subprocess import check_output

root = Path(__file__).parent.resolve()
goroot = Path(check_output([str(root / "sdk/bin/go"), "env", "GOROOT"], text=True).strip())
directory = goroot / "src/runtime"
runtime = "coro_stackless.go"
base = (directory / runtime).read_text()
timers = (directory / "time.go").read_text()
proc = (directory / "proc.go").read_text()

def swap(text, old, new, n=1):
    assert text.count(old) == n, old
    return text.replace(old, new)

def save(mode, files):
    mapping = {}
    for file, body in files.items():
        target = root / "overlays" / f"{mode}-{file}"
        target.write_text(body)
        mapping[str(directory / file)] = str(target)
    (root / "overlays" / f"{mode}.json").write_text(json.dumps({"Replace": mapping}))

marker = "\t\twake = wakeTime == 0 || when < wakeTime\n\t\tts.addHeap(t)"
quiet = swap(timers, marker, """		wake = wakeTime == 0 || when < wakeTime
		if _, watchdog := t.arg.(*stacklessCoroReplacementState); watchdog {
			wake = false
		}
		ts.addHeap(t)""")
quiet_net = swap(quiet, "if _, watchdog := t.arg.(*stacklessCoroReplacementState); watchdog {",
    "if _, watchdog := t.arg.(*stacklessCoroReplacementState); watchdog && sched.lastpoll.Load() != 0 {")
save("quietwatch", {"time.go": quiet})
save("quietnet", {"time.go": quiet_net})

clockless = swap(base, """		const delay = 100_000 // nanoseconds
		replacements.managerTimer = newTimer(nanotime()+delay, 0, startStacklessCoroTimedManager, replacements, nil)
""", "")
clockless = swap(clockless, "\t\tstopTimer(r.managerTimer)\n", "")
save("clockless", {runtime: clockless})

inline = swap(base, "\tmanagerTimer        *timeTimer\n", "\tmanagerTimer        *timer\n")
inline = swap(inline,
    """	replacements := &stacklessCoroReplacementState{
		wake: make(chan struct{}, stacklessCoroWarmExecutorCount-1),
	}
	s.replacements.Store(replacements)""",
    """	var replacements *stacklessCoroReplacementState
	if deferWorkers && !raceenabled && !stacklessCoroIsPullComparison(s) && stacklessCoroPublicCaller(getg()) != getg() {
		pair := new(struct {
			state stacklessCoroReplacementState
			timer timer
		})
		replacements = &pair.state
		replacements.managerTimer = &pair.timer
	} else {
		replacements = new(stacklessCoroReplacementState)
	}
	replacements.wake = make(chan struct{}, stacklessCoroWarmExecutorCount-1)
	s.replacements.Store(replacements)""")
inline = swap(inline,
    "\t\treplacements.managerTimer = newTimer(nanotime()+delay, 0, startStacklessCoroTimedManager, replacements, nil)",
    "\t\treplacements.managerTimer.init(startStacklessCoroTimedManager, replacements)\n"
    "\t\treplacements.managerTimer.reset(nanotime()+delay, 0)")
inline = swap(inline, "stopTimer(r.managerTimer)", "r.managerTimer.stop()")
save("inlinewatch", {runtime: inline})

# Diagnostic proof for the performance comparison. The printing variant is
# kept outside timing and checks whether wakeNetPoller would start a P.
proof = swap(timers, marker, """		wake = wakeTime == 0 || when < wakeTime
		if _, watchdog := t.arg.(*stacklessCoroReplacementState); watchdog {
			println("watchdog-arm", wake, sched.lastpoll.Load() == 0, sched.npidle.Load(), sched.nmspinning.Load())
		}
		ts.addHeap(t)""")
save("watchproof", {"time.go": proof})
