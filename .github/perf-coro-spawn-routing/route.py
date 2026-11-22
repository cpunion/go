import json
from pathlib import Path
from subprocess import check_output

root = Path(__file__).parent.resolve()
goroot = Path(check_output([str(root / "sdk/bin/go"), "env", "GOROOT"], text=True).strip())
source = goroot / "src/runtime/coro_stackless.go"
base = source.read_text()

def change(text, old, new, count=1):
    assert text.count(old) == count, old
    return text.replace(old, new)

def save(mode, contents):
    target = root / "overlays" / f"{mode}-{source.name}"
    target.write_text(contents)
    config = root / "overlays" / f"{mode}.json"
    config.write_text(json.dumps({"Replace": {str(source): str(target)}}))

# The joined public driver below is diagnostic, and is only run on
# fixtures with no detached blocking or foreign calls.
joined = change(base,
    "stacklessCoroLifetimeRootComplete|stacklessCoroLifetimeSpawned|stacklessCoroLifetimePublicHost",
    "stacklessCoroLifetimeRootComplete|stacklessCoroLifetimePublicHost")
joined = change(joined, "\tvar lane *stacklessCoroAssistLane\n\tvar useWide bool",
    "\tvar lane *stacklessCoroAssistLane\n\tvar joined bool\n\tvar useWide bool")
joined = change(joined, "next:\n\t// A public driver",
    """next:
	if public && native && !joined && s.spawned() {
		joined = true
		s.startPendingReplacementManager()
		if r := s.replacements.Load(); r != nil &&
			gomaxprocs >= 2 && r.workProbe.Load() == stacklessCoroWorkUnmeasured {
			probe.replacements = r
		}
	}
	// A public driver""")
save("joined", joined)
wide = joined
for old in (
    "native && !public && action == stacklessCoroActionYield",
    "native && !public && !rotate && !raceenabled",
    "dispatch := native && !public && !raceenabled",
):
    wide = change(wide, old, old.replace("!public", "(!public || joined)"))
wide = change(wide, "\t\tjoined = true\n\t\ts.startPendingReplacementManager()",
    "\t\tjoined = true\n\t\tuseWide = true\n\t\ts.startPendingReplacementManager()")
save("joinedwide", wide)

original = "s = runStacklessCoroRoot(s)"
managed = change(base, original, "s.runInitial()")
save("managedfirst", managed)
deferred = change(managed, "s.startReplacementExecutors(getg().stackIsFixed())",
    "s.startReplacementExecutors(true)")
deferred = change(deferred, "if deferWorkers && count == 1 && stacklessCoroPublicCaller(gp) != gp {",
    "if deferWorkers && count == 1 {")
deferred = change(deferred, "\tgp := getg()\n\tif deferWorkers && count == 1 {",
    "\tif deferWorkers && count == 1 {")
save("manageddeferred", deferred)

marker = "\ts.startPendingReplacementManager()\n\t// A short detached episode"
for mode, width in [("earlytwo", 2), ("earlyall", 4)]:
    front = change(base, marker, f"""	if gomaxprocs >= 2 && s.canAssist() &&
		s.runnableState.Load()&^stacklessCoroForeignReturnerBit >= 8 &&
		s.lifetime.Load()&stacklessCoroLifetimeDeferredExecutors != 0 {{
		count := uint32(min(gomaxprocs, {width}))
		if s.executorCount.CompareAndSwap(1, count) {{
			for range count - 1 {{
				go s.replacementExecutor(true)
			}}
		}}
	}}
	s.startPendingReplacementManager()
	// A short detached episode""")
    save(mode, front)
