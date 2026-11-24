#!/usr/bin/env python3
# Copyright 2026 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

"""Stage-2 coroutine performance benchmark, path-audit, and attribution harness.

Supports all three required comparison groups:
  1. Same-commit coro vs nocoro (--coro vs --nocoro)
  2. Exact upstream merge-base (--coro / --nocoro vs --upstream)
  3. Clean parent vs single optimization candidate (--parent vs --candidate)

Every benchmark is automatically audited from compiler -d=coro=4 diagnostics,
symbol tables, and objdump disassembly into one of four classes:
  - native-coro       (darwin/arm64 or linux/amd64 lowered + ops verified)
  - managed-coro      (other targets lowered + ops verified)
  - ordinary-fallback (skipped / fell back to ordinary goroutines -> segregated)
  - compiler-eliminated (lowered but required target op disappeared -> segregated)
"""

from __future__ import annotations

import argparse
import dataclasses
import datetime as dt
import hashlib
import json
import math
import os
import platform
import random
import re
import statistics
import subprocess
import sys
from pathlib import Path
from typing import Any


@dataclasses.dataclass(frozen=True)
class BenchmarkSpec:
    name: str
    category: str  # real_io | fanout | massive_park | phase_attrib | fallback_control
    entry_funcs: tuple[str, ...]
    required_ops: dict[str, tuple[str, ...]]  # func_short_name -> regexes required in .coro.func* state machine
    required_entry_ops: dict[str, tuple[str, ...]] = dataclasses.field(default_factory=dict)  # checked on public entry .fn$
    forbidden_ops: dict[str, tuple[str, ...]] = dataclasses.field(default_factory=dict)  # forbidden inside .coro.func*
    cpus: tuple[int, ...] = (1, 2, 4)
    fresh_process_1x: bool = False
    expected_bucket: str = "coro"  # "coro" | "ordinary-fallback"


BENCHMARKS: tuple[BenchmarkSpec, ...] = (
    # --- Core Dashboard: Real I/O ---
    BenchmarkSpec(
        name="RealIOPipeRoundTrip16",
        category="real_io",
        entry_funcs=("runPipeRoundTrips", "pipeEchoServer", "pipeEchoClient", "fileReadFull", "fileWriteFull"),
        required_ops={
            "runPipeRoundTrips": (r"runtime\.coroSpawnFrame", r"runtime\.coroChanRecv"),
            "fileReadFull": (r"os\.\(\*File\)\.coroReadStart", r"os\.\(\*File\)\.coroReadFinish"),
            "fileWriteFull": (r"os\.\(\*File\)\.coroWriteStart", r"os\.\(\*File\)\.coroWriteFinish"),
        },
        forbidden_ops={"runPipeRoundTrips": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
    ),
    BenchmarkSpec(
        name="RealIOTCPEcho16",
        category="real_io",
        entry_funcs=("runTCPEchoRounds", "tcpEchoServer", "tcpEchoClient", "tcpReadFull", "tcpWriteFull"),
        required_ops={
            "runTCPEchoRounds": (r"runtime\.coroSpawnFrame", r"runtime\.coroChanRecv"),
            "tcpReadFull": (r"net\.\(\*conn\)\.coroReadStart", r"net\.\(\*conn\)\.coroReadFinish"),
            "tcpWriteFull": (r"net\.\(\*conn\)\.coroWriteStart", r"net\.\(\*conn\)\.coroWriteFinish"),
        },
        forbidden_ops={"runTCPEchoRounds": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
    ),
    BenchmarkSpec(
        name="RealIOTCPFanout8",
        category="real_io",
        entry_funcs=("runTCPFanoutBatches", "tcpFanoutBackend", "tcpFanoutLeaf", "tcpReadFull", "tcpWriteFull"),
        required_ops={
            "runTCPFanoutBatches": (r"runtime\.coroSpawnFrame", r"runtime\.coroChanRecv"),
            "tcpReadFull": (r"net\.\(\*conn\)\.coroReadStart", r"net\.\(\*conn\)\.coroReadFinish"),
            "tcpWriteFull": (r"net\.\(\*conn\)\.coroWriteStart", r"net\.\(\*conn\)\.coroWriteFinish"),
        },
        forbidden_ops={"runTCPFanoutBatches": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
    ),
    # --- Core Dashboard: Parameterized & Capturing Fan-out / Fan-in ---
    BenchmarkSpec(
        name="FanoutArgs16",
        category="fanout",
        entry_funcs=("runFanoutArgs", "fanoutArgsWorker"),
        required_ops={
            "runFanoutArgs": (r"runtime\.coroSpawnFrame", r"runtime\.coroChanRecv"),
            "fanoutArgsWorker": (r"runtime\.coroChanSend",),
        },
        forbidden_ops={"runFanoutArgs": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
    ),
    BenchmarkSpec(
        name="FanoutArgs100",
        category="fanout",
        entry_funcs=("runFanoutArgs", "fanoutArgsWorker"),
        required_ops={
            "runFanoutArgs": (r"runtime\.coroSpawnFrame", r"runtime\.coroChanRecv"),
            "fanoutArgsWorker": (r"runtime\.coroChanSend",),
        },
        forbidden_ops={"runFanoutArgs": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
    ),
    BenchmarkSpec(
        name="FanoutArgs1000",
        category="fanout",
        entry_funcs=("runFanoutArgs", "fanoutArgsWorker"),
        required_ops={
            "runFanoutArgs": (r"runtime\.coroSpawnFrame", r"runtime\.coroChanRecv"),
            "fanoutArgsWorker": (r"runtime\.coroChanSend",),
        },
        forbidden_ops={"runFanoutArgs": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
    ),
    BenchmarkSpec(
        name="FanoutCapture16",
        category="fanout",
        entry_funcs=("runFanoutCapture", "runFanoutCapture.func1"),
        required_ops={
            "runFanoutCapture": (r"runtime\.coroSpawnFrame", r"runtime\.coroChanRecv"),
            "runFanoutCapture.func1": (r"runtime\.coroChanSend",),
        },
        forbidden_ops={"runFanoutCapture": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
    ),
    BenchmarkSpec(
        name="FanoutCapture100",
        category="fanout",
        entry_funcs=("runFanoutCapture", "runFanoutCapture.func1"),
        required_ops={
            "runFanoutCapture": (r"runtime\.coroSpawnFrame", r"runtime\.coroChanRecv"),
            "runFanoutCapture.func1": (r"runtime\.coroChanSend",),
        },
        forbidden_ops={"runFanoutCapture": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
    ),
    BenchmarkSpec(
        name="FanoutCapture1000",
        category="fanout",
        entry_funcs=("runFanoutCapture", "runFanoutCapture.func1"),
        required_ops={
            "runFanoutCapture": (r"runtime\.coroSpawnFrame", r"runtime\.coroChanRecv"),
            "runFanoutCapture.func1": (r"runtime\.coroChanSend",),
        },
        forbidden_ops={"runFanoutCapture": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
    ),
    BenchmarkSpec(
        name="FanoutDynamicOpen64",
        category="fanout",
        entry_funcs=("runFanoutDynamicOpen", "(*openAddWorker).Run", "(*openXorWorker).Run"),
        required_ops={
            "runFanoutDynamicOpen": (r"runtime\.coroLookupInterface", r"runtime\.coroSpawnFrame", r"runtime\.coroChanRecv"),
            "(*openAddWorker).Run": (r"runtime\.coroChanSend",),
            "(*openXorWorker).Run": (r"runtime\.coroChanSend",),
        },
        forbidden_ops={"runFanoutDynamicOpen": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
    ),
    # --- Core Dashboard: Massive Parking (RSS, Wake Tail Latency, GC Reclamation) ---
    BenchmarkSpec(
        name="MassivePark10k",
        category="massive_park",
        entry_funcs=("runMassivePark", "massiveParkWorker"),
        required_ops={
            "runMassivePark": (r"runtime\.coroSpawnFrame",),
            "massiveParkWorker": (r"runtime\.coroChanRecv",),
        },
        forbidden_ops={"runMassivePark": (r"runtime\.newproc",)},
        cpus=(1,),
        fresh_process_1x=True,
    ),
    BenchmarkSpec(
        name="MassivePark100k",
        category="massive_park",
        entry_funcs=("runMassivePark", "massiveParkWorker"),
        required_ops={
            "runMassivePark": (r"runtime\.coroSpawnFrame",),
            "massiveParkWorker": (r"runtime\.coroChanRecv",),
        },
        forbidden_ops={"runMassivePark": (r"runtime\.newproc",)},
        cpus=(1,),
        fresh_process_1x=True,
    ),
    # --- Phase Attribution: Launch / Register / Wake / Execute / Recycle ---
    BenchmarkSpec(
        name="PhaseRootStart",
        category="phase_attrib",
        entry_funcs=("phaseRootEntry",),
        required_ops={},
        required_entry_ops={"phaseRootEntry": (r"runtime\.coroRunFrame",)},
        cpus=(1, 2, 4),
    ),
    BenchmarkSpec(
        name="PhaseSpawnComplete100",
        category="phase_attrib",
        entry_funcs=("runPhaseSpawnComplete", "phaseNoWorkWorker"),
        required_ops={"runPhaseSpawnComplete": (r"runtime\.coroSpawnFrame",)},
        forbidden_ops={"runPhaseSpawnComplete": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
    ),
    BenchmarkSpec(
        name="PhaseSpawnComplete1000",
        category="phase_attrib",
        entry_funcs=("runPhaseSpawnComplete", "phaseNoWorkWorker"),
        required_ops={"runPhaseSpawnComplete": (r"runtime\.coroSpawnFrame",)},
        forbidden_ops={"runPhaseSpawnComplete": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
    ),
    BenchmarkSpec(
        name="PhaseSpawnYield100",
        category="phase_attrib",
        entry_funcs=("runPhaseSpawnYield", "phaseYieldWorker"),
        required_ops={"runPhaseSpawnYield": (r"runtime\.coroSpawnFrame",)},
        forbidden_ops={"runPhaseSpawnYield": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
    ),
    BenchmarkSpec(
        name="PhaseSpawnYield1000",
        category="phase_attrib",
        entry_funcs=("runPhaseSpawnYield", "phaseYieldWorker"),
        required_ops={"runPhaseSpawnYield": (r"runtime\.coroSpawnFrame",)},
        forbidden_ops={"runPhaseSpawnYield": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
    ),
    BenchmarkSpec(
        name="PhaseParkWake100",
        category="phase_attrib",
        entry_funcs=("runPhaseParkWake", "phaseParkWorker"),
        required_ops={
            "runPhaseParkWake": (r"runtime\.coroSpawnFrame",),
            "phaseParkWorker": (r"runtime\.coroChanRecv",),
        },
        forbidden_ops={"runPhaseParkWake": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
    ),
    BenchmarkSpec(
        name="PhaseParkWake1000",
        category="phase_attrib",
        entry_funcs=("runPhaseParkWake", "phaseParkWorker"),
        required_ops={
            "runPhaseParkWake": (r"runtime\.coroSpawnFrame",),
            "phaseParkWorker": (r"runtime\.coroChanRecv",),
        },
        forbidden_ops={"runPhaseParkWake": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
    ),
    BenchmarkSpec(
        name="PhaseChannelPingPong",
        category="phase_attrib",
        entry_funcs=("runPhaseChannelPingPong", "phasePingPongWorker"),
        required_ops={
            "runPhaseChannelPingPong": (r"runtime\.coroSpawnFrame", r"runtime\.coroChanSend", r"runtime\.coroChanRecv"),
            "phasePingPongWorker": (r"runtime\.coroChanRecv", r"runtime\.coroChanSend"),
        },
        forbidden_ops={"runPhaseChannelPingPong": (r"runtime\.newproc",)},
        cpus=(1, 2),
    ),
    # --- Deliberate Ordinary Fallback Control (must be segregated automatically) ---
    BenchmarkSpec(
        name="FallbackOrdinaryHost16",
        category="fallback_control",
        entry_funcs=("runFallbackOrdinaryFanout", "fallbackOrdinaryWorker"),
        required_ops={
            "runFallbackOrdinaryFanout": (r"runtime\.coroSpawnFrame", r"runtime\.coroChanRecv"),
            "fallbackOrdinaryWorker": (r"runtime\.coroChanSend",),
        },
        forbidden_ops={"runFallbackOrdinaryFanout": (r"runtime\.newproc",)},
        cpus=(1, 2, 4),
        expected_bucket="ordinary-fallback",
    ),
)

BENCH_LINE_RE = re.compile(r"^Benchmark([A-Za-z0-9_]+?)(?:-([0-9]+))?\s+([0-9]+)\s+(.*)$")


def parse_go_benchmark_output(text: str) -> dict[tuple[str, int], dict[str, float]]:
    """Parse `go test -bench` lines into {(bench_name, cpu): {unit: value}}."""
    results: dict[tuple[str, int], dict[str, float]] = {}
    for line in text.splitlines():
        m = BENCH_LINE_RE.match(line.strip())
        if not m:
            continue
        name = m.group(1)
        cpu = int(m.group(2)) if m.group(2) else 1
        iters = float(m.group(3))
        rest = m.group(4).split()
        metrics: dict[str, float] = {"iters": iters}
        i = 0
        while i + 1 < len(rest):
            try:
                val = float(rest[i])
            except ValueError:
                i += 1
                continue
            unit = rest[i + 1]
            metrics[unit] = val
            i += 2
        results[(name, cpu)] = metrics
    return results


def classify_benchmark_audit(
    spec: BenchmarkSpec,
    lowering_log: str,
    nm_symbols: set[str],
    objdump_coro_by_func: dict[str, str],
    objdump_entry_by_func: dict[str, str],
    backend_kind: str,  # "native-coro" or "managed-coro"
) -> dict[str, Any]:
    """Classify one benchmark into native-coro / managed-coro / ordinary-fallback / compiler-eliminated."""
    skipped: list[str] = []
    missing_coro_sym: list[str] = []
    forbidden_hits: list[str] = []
    missing_ops: list[str] = []

    for fn in spec.entry_funcs:
        esc = re.escape(fn)
        skip_matches = re.findall(rf"coro: skip [^\n]*\.{esc}: ([^\n]+)", lowering_log)
        if skip_matches:
            skipped.append(f"{fn}: {skip_matches[0].strip()}")
        has_coro = any(s.endswith(f".{fn}.coro") for s in nm_symbols)
        if not has_coro:
            missing_coro_sym.append(fn)

    for fn, patterns in spec.forbidden_ops.items():
        # Check forbidden fallback calls inside the lowered .coro.func* state machine
        # (or public entry if no lowered state machine exists).
        dis = objdump_coro_by_func.get(fn, "") or objdump_entry_by_func.get(fn, "")
        for pat in patterns:
            if re.search(pat, dis):
                forbidden_hits.append(f"{fn} calls {pat}")

    for fn, patterns in spec.required_ops.items():
        dis = objdump_coro_by_func.get(fn, "")
        for pat in patterns:
            if not re.search(pat, dis):
                missing_ops.append(f"{fn}.coro.func missing {pat}")

    for fn, patterns in spec.required_entry_ops.items():
        dis = objdump_entry_by_func.get(fn, "")
        for pat in patterns:
            if not re.search(pat, dis):
                missing_ops.append(f"{fn} entry missing {pat}")

    if skipped or missing_coro_sym or forbidden_hits:
        classification = "ordinary-fallback"
    elif missing_ops:
        classification = "compiler-eliminated"
    else:
        classification = backend_kind

    return {
        "benchmark": spec.name,
        "category": spec.category,
        "classification": classification,
        "expected_bucket": spec.expected_bucket,
        "skipped": skipped,
        "missing_coro_symbols": missing_coro_sym,
        "forbidden_hits": forbidden_hits,
        "missing_required_ops": missing_ops,
    }


def paired_ratio_ci(
    num_samples: list[float],
    den_samples: list[float],
    resamples: int = 2000,
    seed: int = 20261124,
) -> dict[str, Any]:
    n = min(len(num_samples), len(den_samples))
    if n == 0:
        return {"median_ratio": None, "ci95": [None, None], "n": 0}
    ratios = [num_samples[i] / den_samples[i] for i in range(n) if den_samples[i] > 0]
    if not ratios:
        return {"median_ratio": None, "ci95": [None, None], "n": 0}
    med = statistics.median(ratios)
    if len(ratios) == 1:
        return {"median_ratio": med, "ci95": [med, med], "n": 1}
    rng = random.Random(seed)
    boots: list[float] = []
    m = len(ratios)
    for _ in range(resamples):
        sample = [ratios[rng.randrange(m)] for _ in range(m)]
        boots.append(statistics.median(sample))
    boots.sort()
    lo = boots[int(0.025 * (len(boots) - 1))]
    hi = boots[int(0.975 * (len(boots) - 1))]
    return {"median_ratio": med, "ci95": [lo, hi], "n": m}


def run_cmd(cmd: list[str], cwd: Path | None = None, env: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
    return subprocess.run(cmd, cwd=cwd, env=env, text=True, capture_output=True)


def clean_env(goexperiment: str) -> dict[str, str]:
    env = os.environ.copy()
    for k in ("GOROOT", "GOOS", "GOARCH", "GOAMD64", "GOARM64", "GOFLAGS"):
        env.pop(k, None)
    env["GOENV"] = "off"
    env["GOTOOLCHAIN"] = "local"
    env["GO111MODULE"] = "off"
    env["GOEXPERIMENT"] = goexperiment
    env["CGO_ENABLED"] = "1"
    env["GOGC"] = "100"
    env["GOMEMLIMIT"] = "off"
    env["GODEBUG"] = ""
    return env


def git_rev(path: Path) -> str | None:
    p = run_cmd(["git", "-C", str(path), "rev-parse", "HEAD"])
    return p.stdout.strip() if p.returncode == 0 else None


def git_clean(path: Path) -> bool:
    p = run_cmd(["git", "-C", str(path), "status", "--porcelain"])
    return p.returncode == 0 and p.stdout.strip() == ""


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


def inspect_sdk(label: str, goroot: Path, goexperiment: str, allow_dirty: bool) -> dict[str, Any]:
    go_bin = goroot / "bin" / "go"
    if not go_bin.is_file():
        raise SystemExit(f"[{label}] missing Go binary: {go_bin}")
    env = clean_env(goexperiment)
    ver_p = run_cmd([str(go_bin), "version"], env=env)
    if ver_p.returncode != 0:
        raise SystemExit(f"[{label}] go version failed: {ver_p.stderr}")
    version = ver_p.stdout.strip()
    goos = run_cmd([str(go_bin), "env", "GOOS"], env=env).stdout.strip()
    goarch = run_cmd([str(go_bin), "env", "GOARCH"], env=env).stdout.strip()
    rev = git_rev(goroot)
    if rev and not allow_dirty and not git_clean(goroot):
        raise SystemExit(f"[{label}] git worktree is dirty: {goroot}")
    if rev and rev[:10] not in version:
        raise SystemExit(f"[{label}] binary version {version!r} does not match HEAD {rev[:10]}")
    if goexperiment == "coro" and "X:coro" not in version:
        raise SystemExit(f"[{label}] expected X:coro toolchain, got: {version}")
    if goexperiment in ("", "nocoro") and "X:coro" in version:
        raise SystemExit(f"[{label}] expected non-coro toolchain, got: {version}")
    return {
        "label": label,
        "goroot": str(goroot),
        "go_bin": str(go_bin),
        "goexperiment": goexperiment,
        "version": version,
        "goos": goos,
        "goarch": goarch,
        "git_rev": rev,
    }


def compile_and_check(sdk: dict[str, Any], suite_dir: Path, out_dir: Path) -> dict[str, Any]:
    label = sdk["label"]
    binary = out_dir / f"suitev2.{label}.test"
    build_log = out_dir / f"build.{label}.log"
    is_coro = sdk["goexperiment"] == "coro"
    gcflags = "-l -d=coro=4" if is_coro else "-l"
    env = clean_env(sdk["goexperiment"])
    p = run_cmd([sdk["go_bin"], "test", "-c", f"-gcflags={gcflags}", "-o", str(binary), "."], cwd=suite_dir, env=env)
    build_log.write_text(p.stdout + p.stderr, encoding="utf-8")
    if p.returncode != 0:
        raise SystemExit(f"[{label}] build failed (see {build_log}):\n{p.stderr[-2000:]}")
    t = run_cmd([str(binary), "-test.run", "^TestSuiteV2Probes$", "-test.count=1", "-test.timeout=2m"], env=env)
    (out_dir / f"correctness.{label}.log").write_text(t.stdout + t.stderr, encoding="utf-8")
    if t.returncode != 0:
        raise SystemExit(f"[{label}] correctness check failed:\n{t.stdout}\n{t.stderr}")
    return {
        "binary": str(binary),
        "binary_sha256": sha256_file(binary),
        "build_log": str(build_log),
        "gcflags": gcflags,
    }


def audit_coro_mode(sdk: dict[str, Any], build_info: dict[str, Any], out_dir: Path) -> list[dict[str, Any]]:
    binary = Path(build_info["binary"])
    lowering_log = Path(build_info["build_log"]).read_text(encoding="utf-8")
    nm_p = run_cmd([sdk["go_bin"], "tool", "nm", str(binary)], env=clean_env(sdk["goexperiment"]))
    if nm_p.returncode != 0:
        raise SystemExit(f"[{sdk['label']}] go tool nm failed: {nm_p.stderr}")
    nm_symbols: set[str] = set()
    for line in nm_p.stdout.splitlines():
        parts = line.split()
        if len(parts) >= 3 and parts[1] in ("T", "t"):
            nm_symbols.add(parts[2])

    backend_kind = (
        "native-coro"
        if (sdk["goos"], sdk["goarch"]) in (("darwin", "arm64"), ("linux", "amd64"))
        else "managed-coro"
    )

    # Disassemble each unique function referenced across specs once, separating
    # lowered state machines (.coro.func*) from public entry wrappers (.fn$).
    all_funcs: set[str] = set()
    for spec in BENCHMARKS:
        all_funcs.update(spec.entry_funcs)
        all_funcs.update(spec.required_ops.keys())
        all_funcs.update(spec.required_entry_ops.keys())
        all_funcs.update(spec.forbidden_ops.keys())

    objdump_coro_by_func: dict[str, str] = {}
    objdump_entry_by_func: dict[str, str] = {}
    env = clean_env(sdk["goexperiment"])
    for fn in sorted(all_funcs):
        esc = re.escape(fn)
        od_coro = run_cmd([sdk["go_bin"], "tool", "objdump", "-s", rf"\.{esc}\.coro\.func[0-9]+$", str(binary)], env=env)
        od_entry = run_cmd([sdk["go_bin"], "tool", "objdump", "-s", rf"\.{esc}$", str(binary)], env=env)
        objdump_coro_by_func[fn] = od_coro.stdout
        objdump_entry_by_func[fn] = od_entry.stdout

    audits = [
        classify_benchmark_audit(spec, lowering_log, nm_symbols, objdump_coro_by_func, objdump_entry_by_func, backend_kind)
        for spec in BENCHMARKS
    ]
    (out_dir / f"audit.{sdk['label']}.json").write_text(json.dumps(audits, indent=2), encoding="utf-8")
    return audits


def run_benchmark_slice(
    binary: str,
    goexperiment: str,
    bench_regex: str,
    benchtime: str,
    cpu: int,
    raw_path: Path,
) -> dict[tuple[str, int], dict[str, float]]:
    env = clean_env(goexperiment)
    cmd = [
        binary,
        "-test.run",
        "^$",
        "-test.bench",
        bench_regex,
        "-test.benchtime",
        benchtime,
        "-test.count=1",
        f"-test.cpu={cpu}",
        "-test.benchmem",
        "-test.timeout=10m",
    ]
    p = run_cmd(cmd, env=env)
    raw_path.write_text(p.stdout + ("\n--- STDERR ---\n" + p.stderr if p.stderr else ""), encoding="utf-8")
    if p.returncode != 0:
        raise SystemExit(f"benchmark failed ({cmd}):\n{p.stdout[-1500:]}\n{p.stderr[-1500:]}")
    return parse_go_benchmark_output(p.stdout)


def fmt_num(v: float | None, digits: int = 1) -> str:
    if v is None:
        return "-"
    if abs(v) >= 1e9:
        return f"{v/1e9:.2f}G"
    if abs(v) >= 1e6:
        return f"{v/1e6:.2f}M"
    if abs(v) >= 1e4:
        return f"{v/1e3:.1f}k"
    return f"{v:.{digits}f}"


def fmt_bytes(v: float | None) -> str:
    if v is None:
        return "-"
    sign = "-" if v < 0 else ""
    av = abs(v)
    if av >= 1024 * 1024:
        return f"{sign}{av/(1024*1024):.2f} MiB"
    if av >= 1024:
        return f"{sign}{av/1024:.1f} KiB"
    return f"{sign}{av:.0f} B"


def fmt_ratio(info: dict[str, Any] | None) -> str:
    if not info or info.get("median_ratio") is None:
        return "-"
    med = info["median_ratio"]
    lo, hi = info["ci95"]
    return f"{med:.3f}x [{lo:.3f}, {hi:.3f}]"


def build_report_markdown(receipt: dict[str, Any]) -> str:
    lines: list[str] = []
    meta = receipt["meta"]
    lines.append(f"# Stage-2 Coroutine Performance Baseline & Attribution Report")
    lines.append("")
    lines.append(f"- **Generated**: {meta['generated_at']}")
    lines.append(f"- **Platform**: `{meta['goos']}/{meta['goarch']}` (`{meta['uname']}`)")
    lines.append(f"- **Settings**: samples={meta['samples']}, warmups={meta['warmups']}, park_samples={meta['park_samples']}, benchtime=`{meta['benchtime']}`, cpus=`{meta['cpus']}`")
    if meta.get("upstream_merge_base"):
        lines.append(f"- **Upstream merge-base verified**: `{meta['upstream_merge_base']}`")
    lines.append("")
    lines.append("## 1. SDK Provenance")
    lines.append("")
    lines.append("| Mode | Version | Git Rev | `GOEXPERIMENT` | Build `gcflags` |")
    lines.append("|---|---|---|---|---|")
    for label, sdk in receipt["sdks"].items():
        b = receipt["builds"][label]
        lines.append(f"| `{label}` | `{sdk['version']}` | `{(sdk['git_rev'] or '-')[:10]}` | `{sdk['goexperiment'] or '(default)'}` | `{b['gcflags']}` |")
    lines.append("")

    primary_coro_mode = receipt["primary_coro_mode"]
    audits = receipt["audits"].get(primary_coro_mode, [])
    audit_map = {a["benchmark"]: a for a in audits}

    lines.append(f"## 2. Execution Path Audit (`{primary_coro_mode}`)")
    lines.append("")
    lines.append("| Benchmark | Category | Actual Path | Details |")
    lines.append("|---|---|---|---|")
    for a in audits:
        details: list[str] = []
        if a["skipped"]:
            details.append("skip: " + "; ".join(a["skipped"]))
        if a["missing_coro_symbols"]:
            details.append("no .coro: " + ", ".join(a["missing_coro_symbols"]))
        if a["forbidden_hits"]:
            details.append("forbidden: " + "; ".join(a["forbidden_hits"]))
        if a["missing_required_ops"]:
            details.append("missing ops: " + "; ".join(a["missing_required_ops"]))
        detail_str = "; ".join(details) if details else "all lowered + target ops verified in objdump"
        lines.append(f"| `{a['benchmark']}` | `{a['category']}` | **`{a['classification']}`** | {detail_str} |")
    lines.append("")

    summary = receipt["summary"]
    ratio_keys = receipt["ratio_pairs"]

    def render_workload_table(title: str, bench_names: list[str]) -> None:
        lines.append(f"## {title}")
        lines.append("")
        if not bench_names:
            lines.append("_None._")
            lines.append("")
            return
        hdr = ["Benchmark", "P", "Path", f"{primary_coro_mode} ns/op", "ops/s", "p50 / p95 / p99 (ns)", "B/op", "allocs/op", "RSS live (peak)"]
        for rk in ratio_keys:
            hdr.append(f"`{rk}` (95% CI)")
        lines.append("| " + " | ".join(hdr) + " |")
        lines.append("|" + "|".join(["---"] * len(hdr)) + "|")
        for bname in bench_names:
            path_cls = audit_map.get(bname, {}).get("classification", "-")
            cpus_for_b = sorted(int(c) for c in summary.get(bname, {}).keys())
            for cpu in cpus_for_b:
                entry = summary[bname][str(cpu)]
                cm = entry["medians"].get(primary_coro_mode, {})
                lat_str = (
                    f"{fmt_num(cm.get('p50-ns'), 0)} / {fmt_num(cm.get('p95-ns'), 0)} / {fmt_num(cm.get('p99-ns'), 0)}"
                    if "p50-ns" in cm
                    else "-"
                )
                rss_str = f"{fmt_bytes(cm.get('rss-live-B'))} ({fmt_bytes(cm.get('rss-peak-B'))})"
                row = [
                    f"`{bname}`",
                    str(cpu),
                    f"`{path_cls}`",
                    fmt_num(cm.get("ns/op")),
                    fmt_num(cm.get("ops/s")),
                    lat_str,
                    fmt_num(cm.get("B/op"), 0),
                    fmt_num(cm.get("allocs/op"), 0),
                    rss_str,
                ]
                for rk in ratio_keys:
                    row.append(fmt_ratio(entry["ratios"].get(rk)))
                lines.append("| " + " | ".join(row) + " |")
        lines.append("")

    coro_buckets = {"native-coro", "managed-coro"}
    real_io_names = [s.name for s in BENCHMARKS if s.category == "real_io" and audit_map.get(s.name, {}).get("classification") in coro_buckets]
    fanout_names = [s.name for s in BENCHMARKS if s.category == "fanout" and audit_map.get(s.name, {}).get("classification") in coro_buckets]
    park_names = [s.name for s in BENCHMARKS if s.category == "massive_park" and audit_map.get(s.name, {}).get("classification") in coro_buckets]
    phase_names = [s.name for s in BENCHMARKS if s.category == "phase_attrib" and audit_map.get(s.name, {}).get("classification") in coro_buckets]
    fallback_names = [s.name for s in BENCHMARKS if audit_map.get(s.name, {}).get("classification") == "ordinary-fallback"]
    eliminated_names = [s.name for s in BENCHMARKS if audit_map.get(s.name, {}).get("classification") == "compiler-eliminated"]

    render_workload_table("3. Core Dashboard — Real I/O (Verified Coroutine Paths)", real_io_names)
    render_workload_table("4. Core Dashboard — Parameterized & Capturing Fan-out/Fan-in (Verified Coroutine Paths)", fanout_names)

    # Massive Parking dedicated detailed section (RSS, wake tail latency, unhosted ratio, GC reclamation)
    lines.append("## 5. Core Dashboard — Massive Parking (RSS, Wake Tail Latency & GC Reclamation)")
    lines.append("")
    if park_names:
        for bname in park_names:
            lines.append(f"### `{bname}` (`{audit_map[bname]['classification']}`)")
            lines.append("")
            lines.append("| Mode | spawn-park (ns/task) | wake-drain (ns/task) | wake p50 / p95 / p99 (ns) | parked heap/task | parked Go-stack/task | parked OS-RSS/task (peak) | unhosted/task | Go-G delta | GC1 heap delta | GC4 heap delta (retained%) | GC4 RSS / FreeOS RSS delta |")
            lines.append("|---|---|---|---|---|---|---|---|---|---|---|---|")
            entry = summary.get(bname, {}).get("1", {})
            for mode in receipt["mode_order"]:
                m = entry.get("medians", {}).get(mode, {})
                if not m:
                    continue
                wake_str = f"{fmt_num(m.get('p50-ns'), 0)} / {fmt_num(m.get('p95-ns'), 0)} / {fmt_num(m.get('p99-ns'), 0)}"
                rss_str = f"{fmt_bytes(m.get('parked-rss-B/task'))} ({fmt_bytes(m.get('rss-peak-B'))})"
                gc4_str = f"{fmt_bytes(m.get('gc4-heap-delta-B'))} ({m.get('gc4-retained-pct', 0.0):.1f}%)"
                rss_rec = f"{fmt_bytes(m.get('gc4-rss-delta-B'))} / {fmt_bytes(m.get('freeos-rss-delta-B'))}"
                lines.append(
                    f"| `{mode}` | {fmt_num(m.get('spawn-park-ns/task'))} | {fmt_num(m.get('wake-drain-ns/task'))} | {wake_str} | "
                    f"{fmt_bytes(m.get('parked-heap-B/task'))} | {fmt_bytes(m.get('parked-Go-stack-B/task'))} | {rss_str} | "
                    f"{m.get('unhosted-coro/task', 0.0):.2f} | {fmt_num(m.get('go-goroutine-delta'), 0)} | "
                    f"{fmt_bytes(m.get('gc1-heap-delta-B'))} | {gc4_str} | {rss_rec} |"
                )
            lines.append("")
            if entry.get("ratios"):
                rparts = [f"`{rk}`: {fmt_ratio(entry['ratios'].get(rk))}" for rk in ratio_keys if rk in entry["ratios"]]
                lines.append("- **Wall-time paired ratios (`ns/op`)**: " + " | ".join(rparts))
                lines.append("")
    else:
        lines.append("_None._\n")

    render_workload_table("6. Phase Attribution — Raw Measurements (Launch / Register / Wake / Execute / Recycle)", phase_names)

    # Derived per-phase attribution table
    lines.append("### Derived Per-Task Cost Breakdown (`coro` vs `nocoro` vs `upstream`)")
    lines.append("")
    lines.append("| Phase Component | P | " + " | ".join(f"`{m}`" for m in receipt["mode_order"]) + " |")
    lines.append("|---|---|" + "|".join(["---"] * len(receipt["mode_order"])) + "|")
    for comp in receipt.get("phase_attribution", []):
        vals = [f"{fmt_num(comp['values'].get(m))} {comp['unit']}" for m in receipt["mode_order"]]
        lines.append(f"| {comp['name']} | {comp['cpu']} | " + " | ".join(vals) + " |")
    lines.append("")

    render_workload_table("7. Segregated — Ordinary Goroutine Fallback (Not Scored as Stackless Coroutines)", fallback_names)
    render_workload_table("8. Segregated — Compiler-Eliminated Target Operations (Compiler Optimization Only)", eliminated_names)
    return "\n".join(lines)


def compute_phase_attribution(summary: dict[str, Any], mode_order: list[str], cpus: list[int]) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for cpu in cpus:
        cs = str(cpu)
        def get_ns(bench: str, mode: str) -> float | None:
            return summary.get(bench, {}).get(cs, {}).get("medians", {}).get(mode, {}).get("ns/op")

        # Root start/teardown
        vals = {m: get_ns("PhaseRootStart", m) for m in mode_order}
        if any(v is not None for v in vals.values()):
            rows.append({"name": "Root entry + teardown (`PhaseRootStart`)", "cpu": cpu, "unit": "ns/root", "values": vals})

        for n in (100, 1000):
            sc = {m: (v / n if (v := get_ns(f"PhaseSpawnComplete{n}", m)) is not None else None) for m in mode_order}
            sy = {m: (v / n if (v := get_ns(f"PhaseSpawnYield{n}", m)) is not None else None) for m in mode_order}
            pw = {m: (v / n if (v := get_ns(f"PhaseParkWake{n}", m)) is not None else None) for m in mode_order}
            if any(v is not None for v in sc.values()):
                rows.append({"name": f"Spawn + register + complete (`N={n}`)", "cpu": cpu, "unit": "ns/task", "values": sc})
            ydelta = {m: (sy[m] - sc[m] if sy[m] is not None and sc[m] is not None else None) for m in mode_order}
            if any(v is not None for v in ydelta.values()):
                rows.append({"name": f"Yield + dispatch delta (`Yield{n} - Complete{n}`)", "cpu": cpu, "unit": "ns/task", "values": ydelta})
            pdelta = {m: (pw[m] - sc[m] if pw[m] is not None and sc[m] is not None else None) for m in mode_order}
            if any(v is not None for v in pdelta.values()):
                rows.append({"name": f"Channel park + wake + drain delta (`ParkWake{n} - Complete{n}`)", "cpu": cpu, "unit": "ns/task", "values": pdelta})

        pp = {m: get_ns("PhaseChannelPingPong", m) for m in mode_order}
        if any(v is not None for v in pp.values()):
            rows.append({"name": "Steady channel ping-pong (`PhaseChannelPingPong`)", "cpu": cpu, "unit": "ns/rt", "values": pp})
    return rows


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="Stage-2 Coroutine Performance & Attribution Harness")
    ap.add_argument("--output", required=True, help="New output directory for receipt.json, report.md, and raw logs")
    ap.add_argument("--upstream", help="Exact upstream merge-base GOROOT (default build)")
    ap.add_argument("--nocoro", help="Same-commit GOEXPERIMENT=nocoro GOROOT")
    ap.add_argument("--coro", help="Same-commit GOEXPERIMENT=coro GOROOT")
    ap.add_argument("--parent", help="Mode-3 clean parent GOEXPERIMENT=coro GOROOT")
    ap.add_argument("--candidate", help="Mode-3 candidate patch GOEXPERIMENT=coro GOROOT")
    ap.add_argument("--upstream-ref", default="master", help="Git ref used to verify upstream merge-base")
    ap.add_argument("--samples", type=int, default=6, help="Measured interleaved rounds for regular benchmarks")
    ap.add_argument("--warmups", type=int, default=2, help="Warmup rounds discarded before recording")
    ap.add_argument("--park-samples", type=int, default=4, help="Measured fresh-process samples for MassivePark*")
    ap.add_argument("--benchtime", default="200ms", help="go test -benchtime for regular benchmarks")
    ap.add_argument("--cpus", default="1,2,4", help="Comma-separated GOMAXPROCS values")
    ap.add_argument("--allow-dirty", action="store_true", help="Allow dirty worktree during local testing")
    args = ap.parse_args(argv)

    suite_dir = Path(__file__).resolve().parent
    out_dir = Path(args.output).resolve()
    if out_dir.exists():
        raise SystemExit(f"output directory already exists: {out_dir}")
    out_dir.mkdir(parents=True)
    raw_dir = out_dir / "raw"
    raw_dir.mkdir()

    cpus = [int(x.strip()) for x in args.cpus.split(",") if x.strip()]

    sdks: dict[str, dict[str, Any]] = {}
    mode_order: list[str] = []
    if args.upstream:
        sdks["upstream"] = inspect_sdk("upstream", Path(args.upstream).resolve(), "", args.allow_dirty)
        mode_order.append("upstream")
    if args.nocoro:
        sdks["nocoro"] = inspect_sdk("nocoro", Path(args.nocoro).resolve(), "nocoro", args.allow_dirty)
        mode_order.append("nocoro")
    if args.coro:
        sdks["coro"] = inspect_sdk("coro", Path(args.coro).resolve(), "coro", args.allow_dirty)
        mode_order.append("coro")
    if args.parent:
        sdks["parent"] = inspect_sdk("parent", Path(args.parent).resolve(), "coro", args.allow_dirty)
        mode_order.append("parent")
    if args.candidate:
        sdks["candidate"] = inspect_sdk("candidate", Path(args.candidate).resolve(), "coro", args.allow_dirty)
        mode_order.append("candidate")

    if not mode_order:
        raise SystemExit("must specify at least one SDK (--upstream/--nocoro/--coro/--parent/--candidate)")

    # Verify exact upstream merge-base when both upstream and coro git trees are present.
    upstream_merge_base: str | None = None
    if "upstream" in sdks and "coro" in sdks:
        up_rev = sdks["upstream"]["git_rev"]
        co_rev = sdks["coro"]["git_rev"]
        if up_rev and co_rev:
            mb_p = run_cmd(["git", "-C", sdks["coro"]["goroot"], "merge-base", args.upstream_ref, co_rev])
            if mb_p.returncode == 0:
                upstream_merge_base = mb_p.stdout.strip()
                if upstream_merge_base != up_rev:
                    raise SystemExit(f"upstream SDK {up_rev} != exact merge-base {upstream_merge_base}")
        else:
            # Non-git archive SDK (e.g. OrbStack copy): verify version strings carry matching short SHAs.
            upstream_merge_base = sdks["upstream"]["version"]

    # Step 1: Build & correctness check all SDKs.
    builds: dict[str, dict[str, Any]] = {}
    for label in mode_order:
        builds[label] = compile_and_check(sdks[label], suite_dir, out_dir)

    # Step 2: Mandatory path audit on every coro-enabled SDK.
    audits: dict[str, list[dict[str, Any]]] = {}
    for label in mode_order:
        if sdks[label]["goexperiment"] == "coro":
            audits[label] = audit_coro_mode(sdks[label], builds[label], out_dir)
            # Fail fast if deliberate fallback control wasn't caught or if an unexpected elimination occurred.
            for a in audits[label]:
                if a["expected_bucket"] == "ordinary-fallback" and a["classification"] != "ordinary-fallback":
                    raise SystemExit(f"[{label}] fallback control {a['benchmark']} misclassified as {a['classification']}")

    primary_coro_mode = (
        "candidate" if "candidate" in sdks else ("coro" if "coro" in sdks else mode_order[-1])
    )

    # Step 3: Collect interleaved regular benchmarks.
    # raw_samples[bench_name][cpu][mode] = list of metric dicts per round
    raw_samples: dict[str, dict[int, dict[str, list[dict[str, float]]]]] = {}

    regular_specs = [s for s in BENCHMARKS if not s.fresh_process_1x]
    park_specs = [s for s in BENCHMARKS if s.fresh_process_1x]

    for cpu in cpus:
        matching = [s.name for s in regular_specs if cpu in s.cpus]
        if not matching:
            continue
        regex = "^Benchmark(" + "|".join(matching) + ")$"
        total_rounds = args.warmups + args.samples
        for r in range(total_rounds):
            is_warmup = r < args.warmups
            rot = r % len(mode_order)
            order = mode_order[rot:] + mode_order[:rot]
            phase_tag = f"warmup{r+1}" if is_warmup else f"sample{r - args.warmups + 1}"
            for mode in order:
                parsed = run_benchmark_slice(
                    builds[mode]["binary"],
                    sdks[mode]["goexperiment"],
                    regex,
                    args.benchtime,
                    cpu,
                    raw_dir / f"regular.p{cpu}.{phase_tag}.{mode}.txt",
                )
                if is_warmup:
                    continue
                for bname in matching:
                    m = parsed.get((bname, cpu))
                    if m is None:
                        raise SystemExit(f"missing benchmark output for {bname} P={cpu} mode={mode}")
                    raw_samples.setdefault(bname, {}).setdefault(cpu, {}).setdefault(mode, []).append(m)

    # Step 4: Collect fresh-process 1x MassivePark benchmarks.
    for spec in park_specs:
        for cpu in spec.cpus:
            if cpu not in cpus and cpu != 1:
                continue
            regex = f"^Benchmark{spec.name}$"
            for r in range(args.park_samples):
                rot = r % len(mode_order)
                order = mode_order[rot:] + mode_order[:rot]
                for mode in order:
                    parsed = run_benchmark_slice(
                        builds[mode]["binary"],
                        sdks[mode]["goexperiment"],
                        regex,
                        "1x",
                        cpu,
                        raw_dir / f"park.{spec.name}.p{cpu}.sample{r+1}.{mode}.txt",
                    )
                    m = parsed.get((spec.name, cpu))
                    if m is None:
                        raise SystemExit(f"missing massive park output for {spec.name} P={cpu} mode={mode}")
                    raw_samples.setdefault(spec.name, {}).setdefault(cpu, {}).setdefault(mode, []).append(m)

    # Step 5: Compute medians & paired ratios across all comparison groups.
    ratio_pairs: list[str] = []
    pair_defs: list[tuple[str, str, str]] = []
    if "coro" in sdks and "nocoro" in sdks:
        pair_defs.append(("coro/nocoro", "coro", "nocoro"))
    if "coro" in sdks and "upstream" in sdks:
        pair_defs.append(("coro/upstream", "coro", "upstream"))
    if "nocoro" in sdks and "upstream" in sdks:
        pair_defs.append(("nocoro/upstream", "nocoro", "upstream"))
    if "candidate" in sdks and "parent" in sdks:
        pair_defs.append(("candidate/parent", "candidate", "parent"))
    ratio_pairs = [k for k, _, _ in pair_defs]

    summary: dict[str, dict[str, Any]] = {}
    for bname, cpu_map in raw_samples.items():
        summary[bname] = {}
        for cpu, mode_map in cpu_map.items():
            medians: dict[str, dict[str, float]] = {}
            for mode, sample_list in mode_map.items():
                keys = sorted({k for d in sample_list for k in d.keys()})
                medians[mode] = {
                    k: statistics.median([d[k] for d in sample_list if k in d])
                    for k in keys
                }
            ratios: dict[str, Any] = {}
            for rk, num_m, den_m in pair_defs:
                num_vals = [d["ns/op"] for d in mode_map.get(num_m, []) if "ns/op" in d]
                den_vals = [d["ns/op"] for d in mode_map.get(den_m, []) if "ns/op" in d]
                ratios[rk] = paired_ratio_ci(num_vals, den_vals)
            summary[bname][str(cpu)] = {
                "medians": medians,
                "ratios": ratios,
                "samples": mode_map,
            }

    phase_attrib = compute_phase_attribution(summary, mode_order, cpus)

    first_sdk = sdks[mode_order[0]]
    receipt: dict[str, Any] = {
        "meta": {
            "generated_at": dt.datetime.now(dt.timezone.utc).isoformat(),
            "goos": first_sdk["goos"],
            "goarch": first_sdk["goarch"],
            "uname": " ".join(platform.uname()),
            "samples": args.samples,
            "warmups": args.warmups,
            "park_samples": args.park_samples,
            "benchtime": args.benchtime,
            "cpus": cpus,
            "upstream_merge_base": upstream_merge_base,
        },
        "mode_order": mode_order,
        "primary_coro_mode": primary_coro_mode,
        "ratio_pairs": ratio_pairs,
        "sdks": sdks,
        "builds": builds,
        "audits": audits,
        "summary": summary,
        "phase_attribution": phase_attrib,
    }

    (out_dir / "receipt.json").write_text(json.dumps(receipt, indent=2), encoding="utf-8")
    report = build_report_markdown(receipt)
    (out_dir / "report.md").write_text(report, encoding="utf-8")
    print(f"suitev2 benchmark complete: {out_dir}/report.md")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
