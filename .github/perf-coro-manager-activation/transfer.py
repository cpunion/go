import json
from pathlib import Path
from subprocess import check_output

root = Path(__file__).parent.resolve()
goroot = Path(check_output([str(root / "sdk/bin/go"), "env", "GOROOT"], text=True).strip())
runtime = goroot / "src/runtime"
core = "coro_stackless.go"
native = "coro_stackless_native.go"
stub = "coro_stackless_native_stub.go"

def replace(text, old, new, count=1):
    assert text.count(old) == count, old
    return text.replace(old, new)

base = (runtime / core).read_text()
base = replace(base,
    "\ts = runStacklessCoroRoot(s)\n\tlifetime := s.lifetime.Load()",
    """	var driver stacklessCoroNativeDriver
	s = runStacklessCoroRootDriver(s, &driver)
	lifetime := s.lifetime.Load()
	if lifetime&(stacklessCoroLifetimePublicHost|stacklessCoroLifetimeSpawned) != 0 &&
		lifetime&stacklessCoroLifetimeDrained == 0 {
		go s.runOwner(driver.forOwner(s))
		s.waitPublicRoot()
		s.finish()
		return false
	}
	driver.close(s, lifetime&stacklessCoroLifetimeDrained != 0)""")
start = base.index("\tif lifetime&stacklessCoroLifetimePublicHost != 0 {")
end = base.index("\ts.stopReplacementExecutors()", start)
base = base[:start] + """	if lifetime&stacklessCoroLifetimePublicHost != 0 {
		s.waitPublicRoot()
		s.finish()
		return false
	}
""" + base[end:]
base = replace(base,
    """func runStacklessCoroRoot(s *stacklessCoroScheduler) *stacklessCoroScheduler {
	var native stacklessCoroNativeDriver
	initial := true""",
    """func runStacklessCoroRoot(s *stacklessCoroScheduler) *stacklessCoroScheduler {
	var driver stacklessCoroNativeDriver
	s = runStacklessCoroRootDriver(s, &driver)
	driver.close(s, s.drained())
	return s
}

func runStacklessCoroRootDriver(s *stacklessCoroScheduler, native *stacklessCoroNativeDriver) *stacklessCoroScheduler {
	initial := true""")
base = replace(base, """		if s.publicDriverDone() || !native.waitForWork(s) {
			native.close(s, s.drained())
			return s
		}""", """		if s.publicDriverDone() || !native.waitForWork(s) {
			return s
		}""")
base = replace(base, "func (s *stacklessCoroScheduler) runOwner() {",
    "func (s *stacklessCoroScheduler) runOwner(native stacklessCoroNativeDriver) {")
base = replace(base,
    "\tvar native stacklessCoroNativeDriver\n\tfor !s.drained()",
    "\tfor !s.drained()")
extension = """
// forOwner transfers only an idle native context. The active executor and the
// public caller have already restored their own M state in run's return path.
func (d *stacklessCoroNativeDriver) forOwner(s *stacklessCoroScheduler) stacklessCoroNativeDriver {
	ctx := d.context
	if ctx != nil && (ctx.nativeG0 != nil || ctx.caller != nil ||
		ctx.schedulerG != nil && ctx.schedulerG.m != nil ||
		ctx.deferredYield || ctx.deferredOperation != nil || ctx.deferredSleepWhen != 0) {
		throw("runtime: transferring active stackless coroutine context")
	}
	d.context = nil
	return stacklessCoroNativeDriver{context: ctx}
}
"""
fallback = """
func (*stacklessCoroNativeDriver) forOwner(*stacklessCoroScheduler) stacklessCoroNativeDriver {
	return stacklessCoroNativeDriver{}
}
"""
old_native = (runtime / native).read_text()
old_stub = (runtime / stub).read_text()
for mode, addition in (
    ("closecontrol", """
func (d *stacklessCoroNativeDriver) forOwner(s *stacklessCoroScheduler) stacklessCoroNativeDriver {
	d.close(s, false)
	return stacklessCoroNativeDriver{}
}
"""),
    ("transfer", extension),
    ("transferreset", replace(extension, "\td.context = nil\n\treturn stacklessCoroNativeDriver{context: ctx}",
        """	if ctx != nil {
		ctx.selfFrameMissedRoot = false
		ctx.selfFrameCacheUsed = false
	}
	d.context = nil
	return stacklessCoroNativeDriver{context: ctx}""")),
):
    mapping = {}
    for name, source in ((core, base), (native, old_native + addition), (stub, old_stub + fallback)):
        dest = root / "overlays" / f"{mode}-{name}"
        dest.write_text(source)
        mapping[str(runtime / name)] = str(dest)
    (root / "overlays" / f"{mode}.json").write_text(json.dumps({"Replace": mapping}))
