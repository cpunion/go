import json
from pathlib import Path
from subprocess import check_output

root = Path(__file__).parent.resolve()
goroot = Path(check_output([str(root / "sdk/bin/go"), "env", "GOROOT"], text=True).strip())
source = goroot / "src/runtime/coro_stackless.go"
base = source.read_text()

def swap(text, old, new):
    assert text.count(old) == 1, old
    return text.replace(old, new)

initializer = """func initializeStacklessCoroSignal(c *hchan, size int) chan struct{} {
	c.buf = c.raceaddr()
	c.elemtype = abi.TypeOf(struct{}{})
	c.dataqsiz = uint(size)
	c.bubble = getg().bubble
	lockInit(&c.lock, lockRankHchan)
	return *(*chan struct{})(unsafe.Pointer(&c))
}

"""
for name, completion, rootdone in (
    ("signalgroup", False, False),
    ("completedgroup", True, False),
    ("rootgroup", True, True),
):
    extra_fields = ""
    extra_init = ""
    source_code = base
    if completion:
        extra_fields += "		done hchan\n		doneBuffer [stacklessCoroWarmExecutorCount]bool\n"
        extra_init += """	c := &storage.done
	c.buf = unsafe.Pointer(&storage.doneBuffer[0])
	c.elemsize = 1
	c.elemtype = abi.TypeOf(false)
	c.dataqsiz = stacklessCoroWarmExecutorCount
	c.bubble = getg().bubble
	lockInit(&c.lock, lockRankHchan)
	s.executorDone = *(*chan bool)(unsafe.Pointer(&c))
"""
    else:
        extra_init += "	s.executorDone = make(chan bool, stacklessCoroWarmExecutorCount)\n"
    if rootdone:
        extra_fields += "		rootDone hchan\n"
        extra_init += """	if s.rootDone == nil {
		s.rootDone = initializeStacklessCoroSignal(&storage.rootDone, 1)
	}
"""
        source_code = swap(source_code, """	s.rootDone = make(chan struct{}, 1)
	if !raceenabled && !stacklessCoroIsPullComparison(s) && gomaxprocs == 1 {
		s.executorState.Store""", """	if !raceenabled && !stacklessCoroIsPullComparison(s) && gomaxprocs == 1 {
		s.rootDone = make(chan struct{}, 1)
		s.executorState.Store""")
    source_code = swap(source_code, """func (s *stacklessCoroScheduler) startReplacementExecutors(deferWorkers bool) {
	replacements := &stacklessCoroReplacementState{
		wake: make(chan struct{}, stacklessCoroWarmExecutorCount-1),
	}
	s.replacements.Store(replacements)
	// The initial replacements and manager can all report completion without
	// parking. Growth beyond the warm set retains the ordinary join fallback.
	s.executorDone = make(chan bool, stacklessCoroWarmExecutorCount)
	s.executorGrow = make(chan struct{}, 1)
	s.executorStop = make(chan struct{})
	count := uint32""", initializer + """func (s *stacklessCoroScheduler) startReplacementExecutors(deferWorkers bool) {
	storage := new(struct {
		replacements stacklessCoroReplacementState
		wake, grow, stop hchan
""" + extra_fields + """	})
	replacements := &storage.replacements
	replacements.wake = initializeStacklessCoroSignal(&storage.wake, stacklessCoroWarmExecutorCount-1)
	s.replacements.Store(replacements)
	s.executorGrow = initializeStacklessCoroSignal(&storage.grow, 1)
	s.executorStop = initializeStacklessCoroSignal(&storage.stop, 0)
""" + extra_init + "	count := uint32")
    path = root / "overlays" / f"{name}-{source.name}"
    path.write_text(source_code)
    (root / "overlays" / f"{name}.json").write_text(json.dumps({"Replace": {str(source): str(path)}}))
