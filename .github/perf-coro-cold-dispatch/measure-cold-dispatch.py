import json
import os
import subprocess
import sys
from collections import defaultdict
from pathlib import Path
from statistics import median


binary = Path(sys.argv[1]).resolve()
directory = Path(sys.argv[2]).resolve()
directory.mkdir(parents=True, exist_ok=True)
trials = int(sys.argv[3])
env = dict(os.environ)
for name in ("GOEXPERIMENT", "GOFLAGS", "GODEBUG"):
    env.pop(name, None)


def collect(name, procs, tasks):
    result = subprocess.run(
        [str(binary), "-test.run=^$", f"-test.bench=^Benchmark{name}$",
         "-test.benchtime=1x", "-test.benchmem"],
        env={**env, "GOMAXPROCS": str(procs)}, text=True,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=True, timeout=30,
    ).stdout
    by_scheduler = defaultdict(list)
    elapsed = None
    for line in result.splitlines():
        if line.startswith("coro-cold "):
            _, p, kind, scheduler, task, root, value, at = line.split()
            by_scheduler[scheduler].append((int(at), int(kind), task, root == "true", int(p), int(value)))
        if line.startswith(f"Benchmark{name}"):
            elapsed = float(line.split()[2])
    assert elapsed is not None, result
    found = []
    for source in by_scheduler.values():
        events = sorted(source)
        resumes = [entry for entry in events if entry[1] == 8 and not entry[3]]
        if len({entry[2] for entry in resumes}) == tasks:
            found.append(events)
    assert len(found) == 1, (name, procs, len(found), result[:2000])
    events = found[0]

    def select(kind, where=lambda _: True):
        return [entry for entry in events if entry[1] == kind and where(entry)]

    def ts(kind, where=lambda _: True):
        matching = select(kind, where)
        return matching[0][0] if matching else None

    start = ts(0)
    begin = ts(8, lambda e: e[3])
    owner = ts(4)
    root_done = ts(11)
    first_child = ts(8, lambda e: not e[3])
    first_publish = ts(2)
    last_publish = select(2)[-1][0]
    assert start <= ts(1) <= ts(12) <= begin <= first_publish <= first_child <= root_done
    assert first_publish <= owner <= root_done and first_publish <= last_publish <= root_done
    registrations = list(zip(select(16), select(17), strict=True))
    assert all(a[5] == b[5] and a[0] <= b[0] for a, b in registrations)
    assert sum(a[5] for a, b in registrations) == tasks
    times = {
        "elapsed": elapsed,
        "root_scheduler": ts(1) - start,
        "root_registration": ts(12) - ts(1),
        "initial_native_entry": ts(14) - ts(13),
        "root_to_publish": first_publish - begin,
        "spawn_registration": sum(b[0] - a[0] for a, b in registrations),
        "first_publish_to_owner": owner - first_publish,
        "first_publish_to_last_publish": last_publish - first_publish,
        "owner_to_first_child": first_child - owner,
        "first_child_to_root_done": root_done - first_child,
        "publish_to_first_child": first_child - first_publish,
        "root_resumes": len(select(8, lambda event: event[3])),
        "child_resumes": len(select(8, lambda event: not event[3])),
    }
    for kind, label in ((5, "manager"), (6, "replacement"), (10, "work_probe")):
        at = ts(kind)
        if at is not None:
            times[f"entry_to_{label}"] = at - start
    processors = {}
    for event in select(8, lambda event: not event[3]):
        processors.setdefault(event[4], event[0])
    for i, at in enumerate(sorted(processors.values()), 1):
        times[f"entry_to_child_on_P{i}"] = at - start
    return times, events


samples = {}
with (directory / "cold-dispatch.jsonl").open("w") as output:
    for name, tasks in (("ParallelYieldWork16", 16), ("ParallelYieldLight64", 64)):
        for processors in (2, 4):
            group = []
            for i in range(trials):
                phases, events = collect(name, processors, tasks)
                group.append(phases)
                output.write(json.dumps([name, processors, i, phases, events]) + "\n")
            samples[f"{name}/P{processors}"] = {
                key: {"median": median(sample[key] for sample in group if key in sample),
                      "samples": sum(key in sample for sample in group)}
                for key in sorted(set().union(*group))
            }
(directory / "cold-dispatch-summary.json").write_text(json.dumps(samples, indent=2) + "\n")
