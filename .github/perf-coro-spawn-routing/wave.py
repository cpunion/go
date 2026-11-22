import json
from pathlib import Path
from subprocess import check_output

root = Path(__file__).parent.resolve()
goroot = Path(check_output([str(root / "sdk/bin/go"), "env", "GOROOT"], text=True).strip())
source = goroot / "src/runtime/coro_stackless_spawn_batch.go"
base = source.read_text()
marker = """	s.prepareReplacementExecutors()
	if host := currentStacklessCoroHost();"""
assert base.count(marker) == 2

for name, width in (("waveone", 2), ("waveall", 4), ("wavemanager", 0), ("wavecombined", 3)):
    # Only a bounded first compiler-emitted wave can be admitted here.
    # A final chunk of a large producer must not satisfy the gate.
    launch = f"""		count := uint32(min(gomaxprocs, {width}))
		if s.executorCount.CompareAndSwap(1, count) {{
			for range count - 1 {{
				go s.replacementExecutor(true)
			}}
		}}
""" if width else ""
    if name in {"wavemanager", "wavecombined"}:
        launch += "		s.startPendingReplacementManager()\n"
    after = """	s.prepareReplacementExecutors()
	if gomaxprocs >= 2 && ready == 0 && live == uint64(count) &&
		remaining >= 2*int(gomaxprocs) && remaining <= 4*stacklessCoroSpawnBatchSize &&
		s.lifetime.Load()&stacklessCoroLifetimeDeferredExecutors != 0 && s.canAssist() {
""" + launch + """	}
	if host := currentStacklessCoroHost();"""
    target = root / "overlays" / f"{name}-{source.name}"
    target.write_text(base.replace(marker, after))
    config = root / "overlays" / f"{name}.json"
    config.write_text(json.dumps({"Replace": {str(source): str(target)}}))
