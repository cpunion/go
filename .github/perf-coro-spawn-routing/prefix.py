import json
from pathlib import Path
from subprocess import check_output

root = Path(__file__).parent.resolve()
goroot = Path(check_output([str(root / "sdk/bin/go"), "env", "GOROOT"], text=True).strip())
source = goroot / "src/runtime/coro_stackless_spawn_batch.go"
base = source.read_text()
eager = (root / "overlays/waveall-coro_stackless_spawn_batch.go").read_text()
modified = "	limit := min(remaining, len(tasks), abi.CoroSpawnBatchFrameBytes/int(size))\n"
for name, width, payload in (
    ("prefix1", 1, eager), ("prefix3", 3, eager),
    ("prefix5", 5, eager), ("prefix3owner", 3, base),
):
    before = f"""	limit := min(remaining, len(tasks), abi.CoroSpawnBatchFrameBytes/int(size))
	if gomaxprocs >= 2 && remaining >= 2*int(gomaxprocs) &&
		remaining <= 4*stacklessCoroSpawnBatchSize && !s.spawned() {{
		limit = min(limit, {width})
	}}
"""
    assert payload.count(modified) == 2
    target = root / "overlays" / f"{name}-{source.name}"
    target.write_text(payload.replace(modified, before))
    config = root / "overlays" / f"{name}.json"
    config.write_text(json.dumps({"Replace": {str(source): str(target)}}))
