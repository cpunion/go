import json
import sys
from pathlib import Path


sdk = Path(sys.argv[1]).resolve()
directory = Path(sys.argv[2]).resolve()
directory.mkdir(parents=True, exist_ok=True)
runtime = sdk / "src/runtime"
scheduler = runtime / "coro_stackless.go"
batch = runtime / "coro_stackless_spawn_batch.go"


def replace(source, old, new, count=1):
    assert source.count(old) == count, (old, source.count(old))
    return source.replace(old, new)


original = scheduler.read_text()
spawn = batch.read_text()
prepare = """
// Start bounded helpers before taking the queue lock. They can wait on an
// ordinary Go stack while the producer initializes the first ready prefix.
func (s *stacklessCoroScheduler) startInitialSpawnWorkers(parent *stacklessCoroTask,
	remaining, limit int) chan struct{} {
	if !stacklessCoroNativeStackAvailable || raceenabled || gomaxprocs < 3 ||
		parent != &s.root || remaining <= limit || remaining < 8 || remaining > 64 ||
		s.spawned() || !s.canAssist() || stacklessCoroIsPullComparison(s) || !getg().stackIsFixed() ||
		s.executorState.Load() != stacklessCoroExecutorStateOff {
		return nil
	}
	s.prepareReplacementExecutors()
	target := uint32(min(gomaxprocs, WORKERS))
	if !s.executorCount.CompareAndSwap(1, target) {
		return nil
	}
	published := make(chan struct{})
	for range target - 1 {
		go s.runInitialSpawnWorker(published)
	}
	return published
}

func (s *stacklessCoroScheduler) runInitialSpawnWorker(published <-chan struct{}) {
	<-published
	s.replacementExecutor(true)
}
"""

admission = """		if count >= 2 && s.lifetime.Load()&stacklessCoroLifetimeDeferredExecutors != 0 &&
			s.executorCount.CompareAndSwap(1, count) {
			for range count - 1 {
				go s.replacementExecutor(true)
			}
		}"""
reserve = """		if count >= 2 && s.lifetime.Load()&stacklessCoroLifetimeDeferredExecutors != 0 {
			for previous := s.executorCount.Load(); previous < count; previous = s.executorCount.Load() {
				if s.executorCount.CompareAndSwap(previous, count) {
					for range count - previous {
						go s.replacementExecutor(true)
					}
					break
				}
			}
		}"""


def batch_schedule(code, after_publish):
    code = replace(code, """	s := context.scheduler
	lock(&s.lock)""", """	s := context.scheduler
	size := typ.Size_
	limit := min(remaining, stacklessCoroSpawnBatchSize, abi.CoroSpawnBatchFrameBytes/int(size))
	START
	lock(&s.lock)""", 2)
    code = replace(code, """	size := typ.Size_
	limit := min(remaining, len(tasks), abi.CoroSpawnBatchFrameBytes/int(size))
""", "", 2)
    code = replace(code, """	unlock(&s.lock)
	s.prepareReplacementExecutors()""", """	unlock(&s.lock)
	FINISH
	s.prepareReplacementExecutors()""", 2)
    start = "published := s.startInitialSpawnWorkers(parent, remaining, limit)"
    finish = "if published != nil {\n\t\tclose(published)\n\t}"
    if after_publish:
        start, finish = "", start + "\n\t" + finish
    return code.replace("START", start).replace("FINISH", finish)


for mode, count, after in (("early2", 2, False), ("early3", 3, False),
                            ("early4", 4, False), ("after3", 3, True)):
    code = replace(original, admission, reserve) + prepare.replace("WORKERS", str(count))
    sources = {scheduler: code, batch: batch_schedule(spawn, after)}
    config = {}
    for path, body in sources.items():
        target = directory / f"{mode}-{path.name}"
        target.write_text(body)
        config[str(path)] = str(target)
    (directory / f"{mode}.json").write_text(json.dumps({"Replace": config}))
