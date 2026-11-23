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

start = """		select {
		case s.executorGrow <- struct{}{}:
		default:
		}
	}
	const rateMillis = 1_000_000"""
body = """		select {
		case s.executorGrow <- struct{}{}:
		default:
		}
		count := s.executorCount.Load()
		if count >= 2 && gomaxprocs >= 3 && s.canAssist() &&
			s.replacements.Load().workProbe.Load() == stacklessCoroWorkParallel &&
			s.needsWarmExecutor(count) {
			// The existing manager already has a P. Reserve this executor
			// before handing growth and blocking signals to its successor.
			s.executorCount.Add(1)
			go s.manageReplacementGrowth()
			s.replacementExecutor(true)
			return
		}
	}
	s.manageReplacementGrowth()
}

func (s *stacklessCoroScheduler) manageReplacementGrowth() {
	const rateMillis = 1_000_000"""
worker = replace(base, start, body)
no_worker = replace(base, start, """		select {
		case s.executorGrow <- struct{}{}:
		default:
		}
	}
	s.manageReplacementGrowth()
}

func (s *stacklessCoroScheduler) manageReplacementGrowth() {
	const rateMillis = 1_000_000""")
small_probe = replace(worker,
    "count := uint32(min(gomaxprocs, 3))",
    "count := uint32(min(gomaxprocs, 2))")
for name, contents in (("managercontrol", no_worker), ("managerworker", worker), ("managerone", small_probe)):
    target = root / "overlays" / f"{name}-{source.name}"
    target.write_text(contents)
    (root / "overlays" / f"{name}.json").write_text(json.dumps({"Replace": {str(source): str(target)}}))
