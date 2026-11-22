from pathlib import Path
import json
import subprocess
import sys


sdk = Path(sys.argv[1]).resolve()
root = Path(sys.argv[2]).resolve()
mode = sys.argv[3]
candidate = sys.argv[4] if len(sys.argv) == 5 else None
source_dir = Path(__file__).resolve().parent
runtime = sdk / "src" / "runtime"
root.mkdir(parents=True, exist_ok=True)

sources = {}
if mode == "close":
    name = "coro_stackless_close.go"
    sources[name] = subprocess.check_output([
        "patch", "-s", "-o", "-", str(runtime / name), str(source_dir / "close.patch")
    ], text=True)
if mode in ("limit", "wide_limit"):
    name = "coro_stackless.go"
    source = ((runtime / name).read_text() if mode == "limit" else
              subprocess.check_output(["git", "-C", sdk, "show", f"{candidate}:src/runtime/{name}"], text=True))
    before = "func (s *stacklessCoroScheduler) warmExecutorLimit() uint32 {\n"
    after = before + """	if !raceenabled && !stacklessCoroIsPullComparison(s) && s.blockingExecutors.Load() == 0 {
		return min(stacklessCoroInitialExecutorCount, uint32(gomaxprocs))
	}
"""
    assert source.count(before) == 1
    sources[name] = source.replace(before, after)

if mode in ("wide", "wide_limit"):
    files = subprocess.check_output(["git", "-C", sdk, "diff", "--name-only",
                                     candidate + "^", candidate, "--", "src/runtime"], text=True)
    for path in files.splitlines():
        if path.endswith("_test.go") or Path(path).name in sources:
            continue
        sources[Path(path).name] = subprocess.check_output([
            "git", "-C", sdk, "show", f"{candidate}:{path}",
        ], text=True)

replacements = {}
for name, data in sources.items():
    target = root / name
    target.write_text(data)
    replacements[str(runtime / name)] = str(target)
assert replacements
print(json.dumps({"Replace": replacements}, indent=2))
