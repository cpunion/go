from collections import defaultdict
import gzip
import json
from pathlib import Path
from statistics import median
import sys

def events(path):
    source = gzip.open(path, "rt") if path.suffix == ".gz" else path.open()
    with source as stream:
        rows = []
        for line in stream:
            fields = line.split()
            if fields and fields[0] == "coro-routing-phase":
                rows.append(tuple(map(int, fields[1:])))
    return rows

def summarize(directory):
    cases = defaultdict(list)
    for path in sorted(Path(directory).glob("*.log*")):
        case = path.name.split(".")[0].rsplit("-", 1)[0]
        timestamps = defaultdict(list)
        rows = events(path)
        key = next(scheduler for _, scheduler, _, kind in rows if kind == 1)
        for at, scheduler, thread, kind in rows:
            if scheduler == key:
                timestamps[kind].append((at, thread))
        row = {}
        for label, start_id, finish_id in (
            ("registration", 1, 2), ("to_watchdog", 2, 4),
            ("arm_watchdog", 4, 5), ("post_arm_to_owner_launch", 5, 8),
            ("owner_newproc", 8, 9), ("owner_ready_after_launch", 8, 10),
            ("manager_newproc", 11, 12), ("first_child", 1, 13), ("root_complete", 1, 15),
        ):
            if timestamps[start_id] and timestamps[finish_id]:
                start, thread = timestamps[start_id][0]
                finish, _ = timestamps[finish_id][0]
                row[label] = {"ns": finish - start, "same_thread_newm": any(
                    kind == 16 and thread == thread_id and start <= at <= finish
                    for at, _, thread_id, kind in rows
                )}
        cases[case].append(row)
    results = {}
    for case, groups in cases.items():
        assert len(groups) == 30, (case, len(groups))
        results[case] = {}
        for label in sorted(set().union(*groups)):
            samples = [row[label] for row in groups if label in row]
            with_newm = [s["ns"] for s in samples if s["same_thread_newm"]]
            without_newm = [s["ns"] for s in samples if not s["same_thread_newm"]]
            results[case][label] = {
                "runs": len(samples),
                "median_ns": median(s["ns"] for s in samples),
                "newm_runs": len(with_newm),
                "newm_median_ns": median(with_newm) if with_newm else None,
                "no_newm_median_ns": median(without_newm) if without_newm else None,
            }
    return results

if __name__ == "__main__":
    print(json.dumps(summarize(sys.argv[1]), indent=2, sort_keys=True))
