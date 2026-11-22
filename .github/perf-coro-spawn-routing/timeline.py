import json
import os
from random import Random
from statistics import median
import subprocess

def measure():
    random = Random(7391)
    for case in ("Work16", "Light64"):
        for procs in (2, 4):
            for repetition in range(32):
                modes = ["stock", "base", "waveall", "prefix1", "prefix3"]
                random.shuffle(modes)
                for mode in modes:
                    env = dict(os.environ, GOMAXPROCS=str(procs))
                    output = subprocess.check_output(
                        [f"./timeline-{mode}.test", "-test.run", "^$", "-test.bench",
                         f"^BenchmarkTimeline{case}$", "-test.benchtime", "1x"],
                        env=env, text=True, timeout=45,
                    )
                    rows = [l.split()[1:] for l in output.splitlines() if l.startswith("spawn-timeline ")]
                    assert len(rows) == 1, output
                    publish, first, done = map(int, rows[0])
                    assert publish > 0 and first > 0 and done >= first, output
                    print(json.dumps([case, procs, repetition, mode, publish, first, done]), flush=True)

if __name__ == "__main__":
    measure()
