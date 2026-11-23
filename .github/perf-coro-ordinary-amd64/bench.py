from collections import defaultdict
from random import Random
from statistics import median
import json, os, resource, subprocess, sys

def measure(repeats, cases, modes):
    env = {key: value for key, value in os.environ.items()
           if key not in ("GOEXPERIMENT", "GOFLAGS", "GODEBUG")}
    env.update(GOGC="100", GOMEMLIMIT="off")
    random = Random(14617)
    for case in cases.split(","):
        name, p, n = case.split(":")
        env["GOMAXPROCS"] = p
        for pair in range(repeats):
            if pair % len(modes) == 0:
                order = list(modes)
                random.shuffle(order)
                shifts = list(range(len(modes)))
                random.shuffle(shifts)
            shift = shifts[pair % len(modes)]
            row = order[shift:] + order[:shift]
            for mode in row:
                before = resource.getrusage(resource.RUSAGE_CHILDREN)
                result = subprocess.check_output(["./" + mode + ".test", "-test.run=^$",
                    "-test.bench=^Benchmark" + name + "$", "-test.benchtime=" + n + "x",
                    "-test.benchmem"], env=env, text=True, timeout=100)
                after = resource.getrusage(resource.RUSAGE_CHILDREN)
                fields = next(line.split() for line in result.splitlines() if line.startswith("Benchmark" + name))
                assert int(fields[1]) == int(n) and fields[3] == "ns/op", result
                stats = dict(zip(fields[3::2], map(float, fields[2::2])))
                stats["CPU"] = ((after.ru_utime - before.ru_utime) + (after.ru_stime - before.ru_stime)) * 1e9 / int(n)
                print(json.dumps([name, int(p), pair, mode, stats]), flush=True)

def summarize(filename):
    groups = defaultdict(lambda: defaultdict(dict))
    with open(filename) as data:
        for line in data:
            name, p, pair, mode, stats = json.loads(line)
            groups[name,p][mode][pair] = stats
    for workload, modes in groups.items():
        print(*workload)
        for mode, values in modes.items():
            print(" ", mode, len(values), *(round(median(v[key] for v in values.values()), 2)
                                         for key in ("ns/op","CPU","B/op","allocs/op")), end="")
            for reference in ("stock","base"):
                if reference == mode or reference not in modes:
                    continue
                paired = values.keys() & modes[reference].keys()
                if paired:
                    print(" " + reference + ":", *(round(median(values[i][k]/modes[reference][i][k]
                        for i in paired), 3) for k in ("ns/op","CPU")),end="")
            print()

if sys.argv[1] == "run":
    measure(int(sys.argv[2]), sys.argv[3], sys.argv[4:])
else:
    summarize(sys.argv[2])
