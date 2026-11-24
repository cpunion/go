#!/usr/bin/env python3
# Copyright 2026 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

"""Stage-2 coroutine performance benchmark, path-audit, and attribution harness.

Supports all three required comparison groups:
  1. Same-commit coro vs nocoro (--coro vs --nocoro)
  2. Exact upstream merge-base (--coro / --nocoro vs --upstream)
  3. Clean parent vs single optimization candidate (--parent vs --candidate)

Every benchmark is audited BOTH statically (-d=coro=4 diagnostics, nm, and
.coro.func* vs public entry objdump) AND dynamically at runtime outside timing
(hardware frame-pointer stack probe at every P on every coro-enabled SDK) into:
  - native-coro         (runtime observed runtime.coroNativeMain + ops verified)
  - managed-coro        (runtime observed stacklessCoroScheduler without native + ops verified)
  - ordinary-fallback   (skipped or runtime observed ordinary stack -> segregated)
  - compiler-eliminated (lowered but required target op disappeared -> segregated)
  - mixed-coro / unverified (backend inconsistent or unprobed -> invalidated)

In Mode 3 (--parent vs --candidate), BOTH parent and candidate are audited at
every P. Any unexpected fallback, compiler-eliminated target op, or backend
mismatch between parent and candidate invalidates the comparison (exit code 2).
"""

from __future__ import annotations

import argparse
import dataclasses
import datetime as dt
import hashlib
import json
import os
import platform
import random
import re
import statistics
import subprocess
import sys
from pathlib import Path
from typing import Any

HEX40_RE = re.compile(r"^[0-9a-f]{40}$")
CORO_VALID_PATHS = frozenset({"native-coro", "managed-coro"})


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
        name="MassivePark30k",
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


def parse_runtime_path_output(text: str) -> dict[tuple[str, int], dict[str, Any]]:
    """Parse SUITEV2_RUNTIME_PATH JSON lines emitted by TestSuiteV2RuntimePath."""
    out: dict[tuple[str, int], dict[str, Any]] = {}
    marker = "SUITEV2_RUNTIME_PATH "
    for line in text.splitlines():
        idx = line.find(marker)
        if idx == -1:
            continue
        payload = json.loads(line[idx + len(marker) :].strip())
        out[(payload["benchmark"], int(payload["cpu"]))] = payload
    return out


def classify_runtime_observation(static_status: str, runtime_rep: dict[str, Any] | None) -> str:
    """Combine static lowering/op audit with observed runtime execution path."""
    if static_status == "compiler-eliminated":
        return "compiler-eliminated"
    if static_status == "ordinary-fallback":
        return "ordinary-fallback"
    if runtime_rep is None:
        return "unverified"
    obs = set(runtime_rep.get("observed_paths") or [])
    if not obs:
        return "unverified"
    if "ordinary-fallback" in obs:
        return "ordinary-fallback"
    if obs == {"native-coro"}:
        return "native-coro"
    if obs == {"managed-coro"}:
        return "managed-coro"
    return "mixed-coro"


def classify_benchmark_audit(
    spec: BenchmarkSpec,
    lowering_log: str,
    nm_symbols: set[str],
    objdump_coro_by_func: dict[str, str],
    objdump_entry_by_func: dict[str, str],
    runtime_by_cpu: dict[int, dict[str, Any]] | None = None,
    target_cpus: list[int] | None = None,
) -> dict[str, Any]:
    """Audit one benchmark statically and across every tested P at runtime."""
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
        static_status = "ordinary-fallback"
    elif missing_ops:
        static_status = "compiler-eliminated"
    else:
        static_status = "coro-lowered"

    cpus_checked = list(target_cpus) if target_cpus is not None else list(spec.cpus)
    runtime_by_cpu = runtime_by_cpu or {}
    per_cpu: dict[str, dict[str, Any]] = {}
    observed_classes: set[str] = set()
    for cpu in cpus_checked:
        rt = runtime_by_cpu.get(cpu)
        cls = classify_runtime_observation(static_status, rt)
        observed_classes.add(cls)
        per_cpu[str(cpu)] = {
            "classification": cls,
            "runtime_observed_paths": rt.get("observed_paths", []) if rt else [],
            "root_paths": rt.get("root_paths", []) if rt else [],
            "worker_paths": rt.get("worker_paths", []) if rt else [],
            "root_samples": rt.get("root_samples", 0) if rt else 0,
            "worker_samples": rt.get("worker_samples", 0) if rt else 0,
            "sample_frames": rt.get("sample_frames", {}) if rt else {},
        }

    if len(observed_classes) == 1:
        overall = next(iter(observed_classes))
    elif "compiler-eliminated" in observed_classes:
        overall = "compiler-eliminated"
    elif "ordinary-fallback" in observed_classes:
        overall = "ordinary-fallback"
    elif "unverified" in observed_classes:
        overall = "unverified"
    else:
        overall = "mixed-coro"

    return {
        "benchmark": spec.name,
        "category": spec.category,
        "static_status": static_status,
        "classification": overall,
        "expected_bucket": spec.expected_bucket,
        "per_cpu": per_cpu,
        "skipped": skipped,
        "missing_coro_symbols": missing_coro_sym,
        "forbidden_hits": forbidden_hits,
        "missing_required_ops": missing_ops,
    }


def validate_comparison_audits(
    audits: dict[str, list[dict[str, Any]]],
    selected_specs: list[BenchmarkSpec],
) -> dict[str, Any]:
    """Validate execution paths across all coro SDKs and every P.

    Invalidates the run if any core benchmark unexpectedly falls back, loses a
    required target op, has mixed/unverified runtime paths, or changes backend
    between parent and candidate.
    """
    failures: list[dict[str, Any]] = []
    invalid_keys: set[tuple[str, int]] = set()
    coro_modes = list(audits.keys())
    audit_by_mode = {m: {a["benchmark"]: a for a in alist} for m, alist in audits.items()}

    for spec in selected_specs:
        for mode in coro_modes:
            a = audit_by_mode[mode].get(spec.name)
            if a is None:
                failures.append({"benchmark": spec.name, "cpu": None, "mode": mode, "reason": "missing audit record"})
                continue
            for cpu_s, info in sorted(a["per_cpu"].items(), key=lambda kv: int(kv[0])):
                cpu = int(cpu_s)
                cls = info["classification"]
                if spec.expected_bucket == "coro":
                    if cls not in CORO_VALID_PATHS:
                        failures.append({
                            "benchmark": spec.name,
                            "cpu": cpu,
                            "mode": mode,
                            "classification": cls,
                            "reason": f"core benchmark executed as {cls!r} instead of verified coroutine backend",
                        })
                        invalid_keys.add((spec.name, cpu))
                elif spec.expected_bucket == "ordinary-fallback":
                    if cls != "ordinary-fallback":
                        failures.append({
                            "benchmark": spec.name,
                            "cpu": cpu,
                            "mode": mode,
                            "classification": cls,
                            "reason": f"fallback control executed as {cls!r} instead of 'ordinary-fallback'",
                        })
                        invalid_keys.add((spec.name, cpu))

        if "parent" in audit_by_mode and "candidate" in audit_by_mode:
            pa = audit_by_mode["parent"].get(spec.name)
            ca = audit_by_mode["candidate"].get(spec.name)
            if pa and ca:
                all_cpus = sorted({int(c) for c in pa["per_cpu"].keys()} | {int(c) for c in ca["per_cpu"].keys()})
                for cpu in all_cpus:
                    p_cls = pa["per_cpu"].get(str(cpu), {}).get("classification", "missing")
                    c_cls = ca["per_cpu"].get(str(cpu), {}).get("classification", "missing")
                    if p_cls != c_cls:
                        failures.append({
                            "benchmark": spec.name,
                            "cpu": cpu,
                            "mode": "parent->candidate",
                            "parent_path": p_cls,
                            "candidate_path": c_cls,
                            "reason": f"execution path changed between parent ({p_cls}) and candidate ({c_cls})",
                        })
                        invalid_keys.add((spec.name, cpu))

    return {
        "valid": len(failures) == 0,
        "failure_count": len(failures),
        "invalid_pairs": sorted([[b, c] for b, c in invalid_keys]),
        "failures": failures,
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


def compute_baseline_stability(summary: dict[str, Any]) -> dict[str, Any] | None:
    """Compute exact nocoro/upstream dispersion and 95% CI exclusion counts."""
    total = 0
    outside_band: list[dict[str, Any]] = []
    ci_excludes_one: list[dict[str, Any]] = []
    for bname, cpu_map in summary.items():
        for cpu_s, entry in sorted(cpu_map.items(), key=lambda kv: int(kv[0])):
            r = entry.get("ratios", {}).get("nocoro/upstream")
            if not r or r.get("median_ratio") is None:
                continue
            total += 1
            med = r["median_ratio"]
            lo, hi = r["ci95"]
            rec = {"benchmark": bname, "cpu": int(cpu_s), "median_ratio": med, "ci95": [lo, hi]}
            if med < 0.95 or med > 1.05:
                outside_band.append(rec)
            if hi < 1.0 or lo > 1.0:
                ci_excludes_one.append(rec)
    if total == 0:
        return None
    return {
        "total": total,
        "outside_095_105_count": len(outside_band),
        "ci95_excludes_1_count": len(ci_excludes_one),
        "outside_095_105": outside_band,
        "ci95_excludes_1": ci_excludes_one,
    }


def run_cmd(cmd: list[str], cwd: Path | None = None, env: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
    return subprocess.run(cmd, cwd=cwd, env=env, text=True, capture_output=True)


def clean_env(goexperiment: str) -> dict[str, str]:
    env = os.environ.copy()
    for k in ("GOROOT", "GOOS", "GOARCH", "GOAMD64", "GOARM64", "GOFLAGS"):
        env.pop(k, None)
    env["GOENV"] = "off"
    env["GOTOOLCHAIN"] = "local"
    env["GO111MODULE"] = "on"
    env["GOEXPERIMENT"] = goexperiment
    env["CGO_ENABLED"] = "1"
    env["GOGC"] = "100"
    env["GOMEMLIMIT"] = "off"
    env["GODEBUG"] = ""
    return env


def git_rev(path: Path) -> str | None:
    p = run_cmd(["git", "-C", str(path), "rev-parse", "HEAD"])
    rev = p.stdout.strip()
    return rev if p.returncode == 0 and HEX40_RE.match(rev) else None


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
    compile_bin = goroot / "pkg" / "tool" / f"{goos}_{goarch}" / "compile"
    if not compile_bin.is_file():
        raise SystemExit(f"[{label}] missing compile binary: {compile_bin}")

    go_bin_sha = sha256_file(go_bin)
    compile_sha = sha256_file(compile_bin)

    rev: str | None = None
    dirty = False
    identity_source = ""

    if (goroot / ".git").exists():
        rev = git_rev(goroot)
        if not rev:
            raise SystemExit(f"[{label}] failed to read full 40-char git rev from {goroot}")
        dirty = not git_clean(goroot)
        identity_source = "git-worktree"
    else:
        manifest_path = goroot / "CORO_BUILD_MANIFEST.json"
        if not manifest_path.is_file():
            raise SystemExit(
                f"[{label}] SDK {goroot} has neither .git nor CORO_BUILD_MANIFEST.json; "
                "refusing unverified version identity"
            )
        try:
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        except Exception as exc:
            raise SystemExit(f"[{label}] invalid manifest {manifest_path}: {exc}")
        rev = str(manifest.get("git_rev", "")).strip()
        if not HEX40_RE.match(rev):
            raise SystemExit(f"[{label}] manifest git_rev is not a full 40-char SHA: {rev!r}")
        dirty = bool(manifest.get("git_dirty", False))
        if manifest.get("go_bin_sha256") != go_bin_sha:
            raise SystemExit(f"[{label}] manifest go_bin_sha256 mismatch for {go_bin}")
        if manifest.get("compile_sha256") != compile_sha:
            raise SystemExit(f"[{label}] manifest compile_sha256 mismatch for {compile_bin}")
        if str(manifest.get("goexperiment", "")) != goexperiment:
            raise SystemExit(
                f"[{label}] manifest goexperiment={manifest.get('goexperiment')!r} != expected {goexperiment!r}"
            )
        identity_source = "build-manifest"

    if dirty and not allow_dirty:
        raise SystemExit(f"[{label}] SDK source tree is dirty ({identity_source}): {goroot}")
    if rev[:10] not in version:
        raise SystemExit(f"[{label}] binary version {version!r} does not contain verified SHA prefix {rev[:10]}")
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
        "git_dirty": dirty,
        "identity_source": identity_source,
        "go_bin_sha256": go_bin_sha,
        "compile_sha256": compile_sha,
    }


def resolve_git_repo(sdks: dict[str, dict[str, Any]], explicit_repo: str | None, suite_dir: Path) -> Path:
    candidates: list[Path] = []
    if explicit_repo:
        candidates.append(Path(explicit_repo).resolve())
    for sdk in sdks.values():
        p = Path(sdk["goroot"])
        if (p / ".git").exists():
            candidates.append(p)
    candidates.append(suite_dir)
    for c in candidates:
        p = run_cmd(["git", "-C", str(c), "rev-parse", "--show-toplevel"])
        if p.returncode == 0:
            return Path(p.stdout.strip())
    raise SystemExit("unable to locate a git repository to verify SDK commit ancestry; pass --git-repo")


def verify_sdk_identities(
    sdks: dict[str, dict[str, Any]],
    git_repo: Path,
    upstream_ref: str,
    allow_same_commit: bool,
) -> dict[str, Any]:
    """Verify full-SHA relationships across all configured SDK comparison modes."""
    provenance: dict[str, Any] = {"git_repo": str(git_repo)}

    # Ensure every SDK's full commit object exists in the verification git repository.
    for label, sdk in sdks.items():
        rev = sdk["git_rev"]
        chk = run_cmd(["git", "-C", str(git_repo), "cat-file", "-e", f"{rev}^{{commit}}"])
        if chk.returncode != 0:
            raise SystemExit(f"[{label}] commit {rev} not found in git repo {git_repo}")

    if "coro" in sdks and "nocoro" in sdks:
        c_rev = sdks["coro"]["git_rev"]
        n_rev = sdks["nocoro"]["git_rev"]
        if c_rev != n_rev:
            raise SystemExit(f"same-commit check failed: coro={c_rev} != nocoro={n_rev}")
        provenance["coro_nocoro_same_commit"] = c_rev

    if "upstream" in sdks and ("coro" in sdks or "nocoro" in sdks):
        up_rev = sdks["upstream"]["git_rev"]
        target_label = "coro" if "coro" in sdks else "nocoro"
        target_rev = sdks[target_label]["git_rev"]
        anc = run_cmd(["git", "-C", str(git_repo), "merge-base", "--is-ancestor", up_rev, target_rev])
        if anc.returncode != 0:
            raise SystemExit(f"upstream SDK {up_rev} is not an ancestor of {target_label} {target_rev}")
        mb_p = run_cmd(["git", "-C", str(git_repo), "merge-base", upstream_ref, target_rev])
        if mb_p.returncode != 0:
            raise SystemExit(
                f"failed to compute git merge-base {upstream_ref!r} {target_rev} in {git_repo}: {mb_p.stderr.strip()}"
            )
        exact_mb = mb_p.stdout.strip()
        if exact_mb != up_rev:
            raise SystemExit(f"upstream SDK {up_rev} != exact merge-base({upstream_ref}, {target_rev}) {exact_mb}")
        provenance["upstream_merge_base"] = exact_mb
        provenance["upstream_ref"] = upstream_ref

    if "parent" in sdks and "candidate" in sdks:
        p_rev = sdks["parent"]["git_rev"]
        c_rev = sdks["candidate"]["git_rev"]
        if p_rev == c_rev and not allow_same_commit:
            raise SystemExit(
                f"Mode-3 parent and candidate have identical SHA {p_rev}; pass --allow-same-commit for smoke runs"
            )
        anc = run_cmd(["git", "-C", str(git_repo), "merge-base", "--is-ancestor", p_rev, c_rev])
        if anc.returncode != 0:
            raise SystemExit(f"Mode-3 ancestry failed: parent {p_rev} is not an ancestor of candidate {c_rev}")
        cnt_p = run_cmd(["git", "-C", str(git_repo), "rev-list", "--count", f"{p_rev}..{c_rev}"])
        dist = int(cnt_p.stdout.strip()) if cnt_p.returncode == 0 else -1
        provenance["mode3_parent_rev"] = p_rev
        provenance["mode3_candidate_rev"] = c_rev
        provenance["mode3_commit_distance"] = dist

    return provenance


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


def cpus_for_spec(spec: BenchmarkSpec, requested_cpus: list[int]) -> list[int]:
    if spec.fresh_process_1x:
        return [1]
    return [c for c in requested_cpus if c in spec.cpus]


def audit_coro_mode(
    sdk: dict[str, Any],
    build_info: dict[str, Any],
    selected_specs: list[BenchmarkSpec],
    requested_cpus: list[int],
    out_dir: Path,
) -> list[dict[str, Any]]:
    binary = Path(build_info["binary"])
    lowering_log = Path(build_info["build_log"]).read_text(encoding="utf-8")
    env = clean_env(sdk["goexperiment"])
    nm_p = run_cmd([sdk["go_bin"], "tool", "nm", str(binary)], env=env)
    if nm_p.returncode != 0:
        raise SystemExit(f"[{sdk['label']}] go tool nm failed: {nm_p.stderr}")
    nm_symbols: set[str] = set()
    for line in nm_p.stdout.splitlines():
        parts = line.split()
        if len(parts) >= 3 and parts[1] in ("T", "t"):
            nm_symbols.add(parts[2])

    all_funcs: set[str] = set()
    for spec in selected_specs:
        all_funcs.update(spec.entry_funcs)
        all_funcs.update(spec.required_ops.keys())
        all_funcs.update(spec.required_entry_ops.keys())
        all_funcs.update(spec.forbidden_ops.keys())

    objdump_coro_by_func: dict[str, str] = {}
    objdump_entry_by_func: dict[str, str] = {}
    for fn in sorted(all_funcs):
        esc = re.escape(fn)
        od_coro = run_cmd([sdk["go_bin"], "tool", "objdump", "-s", rf"\.{esc}\.coro\.func[0-9]+$", str(binary)], env=env)
        od_entry = run_cmd([sdk["go_bin"], "tool", "objdump", "-s", rf"\.{esc}$", str(binary)], env=env)
        objdump_coro_by_func[fn] = od_coro.stdout
        objdump_entry_by_func[fn] = od_entry.stdout

    # Runtime execution path probe outside timing, per CPU level.
    cpu_to_benches: dict[int, list[str]] = {}
    for spec in selected_specs:
        for cpu in cpus_for_spec(spec, requested_cpus):
            cpu_to_benches.setdefault(cpu, []).append(spec.name)

    runtime_results: dict[tuple[str, int], dict[str, Any]] = {}
    for cpu, bnames in sorted(cpu_to_benches.items()):
        probe_cmd = [
            str(binary),
            "-test.v",
            "-test.run",
            "^TestSuiteV2RuntimePath$",
            "-test.count=1",
            f"-test.cpu={cpu}",
            "-test.timeout=2m",
            f"-suitev2.probe-bench={','.join(bnames)}",
        ]
        pr = run_cmd(probe_cmd, env=env)
        (out_dir / f"runtime_path.{sdk['label']}.p{cpu}.log").write_text(
            pr.stdout + ("\n--- STDERR ---\n" + pr.stderr if pr.stderr else ""), encoding="utf-8"
        )
        if pr.returncode != 0:
            raise SystemExit(
                f"[{sdk['label']}] runtime path probe failed at P={cpu}:\n{pr.stdout[-1500:]}\n{pr.stderr[-1500:]}"
            )
        runtime_results.update(parse_runtime_path_output(pr.stdout))

    audits: list[dict[str, Any]] = []
    for spec in selected_specs:
        t_cpus = cpus_for_spec(spec, requested_cpus)
        rt_map = {cpu: runtime_results[(spec.name, cpu)] for cpu in t_cpus if (spec.name, cpu) in runtime_results}
        audits.append(
            classify_benchmark_audit(
                spec,
                lowering_log,
                nm_symbols,
                objdump_coro_by_func,
                objdump_entry_by_func,
                runtime_by_cpu=rt_map,
                target_cpus=t_cpus,
            )
        )
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
    prov = receipt.get("provenance", {})
    validation = receipt.get("validation", {"valid": True, "failures": [], "invalid_pairs": []})
    invalid_set = {(b, int(c)) for b, c in validation.get("invalid_pairs", [])}

    lines.append("# Stage-2 Coroutine Performance Baseline & Attribution Report")
    lines.append("")
    if not validation.get("valid", True):
        lines.append("> [!CAUTION]")
        lines.append(
            f"> **COMPARISON INVALIDATED ({validation.get('failure_count', len(validation.get('failures', [])))} path audit failure(s))**: "
            "One or more scored benchmarks suffered an unexpected fallback, target-operation elimination, "
            "or backend change between parent and candidate. Do NOT accept speedup conclusions from invalidated pairs."
        )
        lines.append("")

    lines.append(f"- **Generated**: {meta['generated_at']}")
    lines.append(f"- **Platform**: `{meta['goos']}/{meta['goarch']}` (`{meta['uname']}`)")
    lines.append(
        f"- **Settings**: samples={meta['samples']}, warmups={meta['warmups']}, "
        f"park_samples={meta['park_samples']}, benchtime=`{meta['benchtime']}`, cpus=`{meta['cpus']}`"
    )
    if prov.get("upstream_merge_base"):
        lines.append(
            f"- **Upstream merge-base verified**: `{prov['upstream_merge_base']}` (ref `{prov.get('upstream_ref', 'master')}`)"
        )
    if prov.get("coro_nocoro_same_commit"):
        lines.append(f"- **Same-commit `coro`/`nocoro` verified**: `{prov['coro_nocoro_same_commit']}`")
    if prov.get("mode3_parent_rev"):
        lines.append(
            f"- **Mode-3 ancestry verified**: `{prov['mode3_parent_rev'][:10]}` -> `{prov['mode3_candidate_rev'][:10]}` "
            f"(distance={prov.get('mode3_commit_distance')} commit(s))"
        )
    lines.append(f"- **Path Validation Status**: **{'PASS' if validation.get('valid', True) else 'INVALID'}**")
    lines.append("")

    lines.append("## 1. SDK Provenance (Full Verified Identity)")
    lines.append("")
    lines.append("| Mode | Full Git SHA | Source | `GOEXPERIMENT` | Version | Build `gcflags` |")
    lines.append("|---|---|---|---|---|---|")
    for label, sdk in receipt["sdks"].items():
        b = receipt["builds"][label]
        lines.append(
            f"| `{label}` | `{sdk['git_rev']}` | `{sdk.get('identity_source', '-')}` | "
            f"`{sdk['goexperiment'] or '(default)'}` | `{sdk['version']}` | `{b['gcflags']}` |"
        )
    lines.append("")

    primary_coro_mode = receipt["primary_coro_mode"]
    audits_all = receipt["audits"]
    audits = audits_all.get(primary_coro_mode, [])
    audit_map = {a["benchmark"]: a for a in audits}
    parent_audit_map = {a["benchmark"]: a for a in audits_all.get("parent", [])}

    lines.append("## 2. Execution Path Audit (Static + Per-P Runtime Verified)")
    lines.append("")
    if parent_audit_map and primary_coro_mode == "candidate":
        lines.append("| Benchmark | Category | Parent Path (per P) | Candidate Path (per P) | Match | Details |")
        lines.append("|---|---|---|---|---|---|")
        for a in audits:
            pa = parent_audit_map.get(a["benchmark"], {})
            p_str = ", ".join(f"P{c}:{v['classification']}" for c, v in sorted(pa.get("per_cpu", {}).items(), key=lambda kv: int(kv[0])))
            c_str = ", ".join(f"P{c}:{v['classification']}" for c, v in sorted(a["per_cpu"].items(), key=lambda kv: int(kv[0])))
            match_ok = all(
                pa.get("per_cpu", {}).get(c, {}).get("classification") == v["classification"]
                for c, v in a["per_cpu"].items()
            )
            details = _format_audit_details(a)
            lines.append(
                f"| `{a['benchmark']}` | `{a['category']}` | `{p_str}` | `{c_str}` | "
                f"**{'OK' if match_ok else 'MISMATCH'}** | {details} |"
            )
    else:
        lines.append(f"| Benchmark | Category | Verified Path (`{primary_coro_mode}`, per P) | Details |")
        lines.append("|---|---|---|---|")
        for a in audits:
            c_str = ", ".join(f"P{c}:{v['classification']}" for c, v in sorted(a["per_cpu"].items(), key=lambda kv: int(kv[0])))
            details = _format_audit_details(a)
            lines.append(f"| `{a['benchmark']}` | `{a['category']}` | **`{c_str}`** | {details} |")
    lines.append("")

    if not validation.get("valid", True):
        lines.append("### Invalidated Comparisons")
        lines.append("")
        lines.append("| Benchmark | P | Mode | Reason |")
        lines.append("|---|---|---|---|")
        for f in validation.get("failures", []):
            lines.append(f"| `{f['benchmark']}` | `{f.get('cpu', '-')}` | `{f.get('mode', '-')}` | {f['reason']} |")
        lines.append("")

    stability = receipt.get("baseline_stability")
    if stability:
        lines.append("## 2b. Baseline Stability Check (`nocoro/upstream`)")
        lines.append("")
        lines.append(
            f"- Median outside `[0.95x, 1.05x]`: **{stability['outside_095_105_count']}/{stability['total']}**"
        )
        lines.append(
            f"- 95% paired bootstrap CI excludes `1.0x`: **{stability['ci95_excludes_1_count']}/{stability['total']}**"
        )
        if stability["ci95_excludes_1"]:
            excl = ", ".join(
                f"`{x['benchmark']}/P{x['cpu']}` ({x['median_ratio']:.3f}x [{x['ci95'][0]:.3f}, {x['ci95'][1]:.3f}])"
                for x in stability["ci95_excludes_1"]
            )
            lines.append(f"- CI-excluded entries: {excl}")
        lines.append("")

    summary = receipt["summary"]
    ratio_keys = receipt["ratio_pairs"]
    selected_specs = [s for s in BENCHMARKS if s.name in audit_map or s.name in summary]

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
            a = audit_map.get(bname, {})
            cpus_for_b = sorted(int(c) for c in summary.get(bname, {}).keys())
            for cpu in cpus_for_b:
                entry = summary[bname][str(cpu)]
                cm = entry["medians"].get(primary_coro_mode, {})
                path_cls = a.get("per_cpu", {}).get(str(cpu), {}).get("classification", a.get("classification", "-"))
                is_inv = (bname, cpu) in invalid_set
                path_disp = f"**INVALID ({path_cls})**" if is_inv else f"`{path_cls}`"
                lat_str = (
                    f"{fmt_num(cm.get('p50-ns'), 0)} / {fmt_num(cm.get('p95-ns'), 0)} / {fmt_num(cm.get('p99-ns'), 0)}"
                    if "p50-ns" in cm
                    else "-"
                )
                rss_str = f"{fmt_bytes(cm.get('rss-live-B'))} ({fmt_bytes(cm.get('rss-peak-B'))})"
                row = [
                    f"`{bname}`",
                    str(cpu),
                    path_disp,
                    fmt_num(cm.get("ns/op")),
                    fmt_num(cm.get("ops/s")),
                    lat_str,
                    fmt_num(cm.get("B/op"), 0),
                    fmt_num(cm.get("allocs/op"), 0),
                    rss_str,
                ]
                for rk in ratio_keys:
                    r_txt = fmt_ratio(entry["ratios"].get(rk))
                    row.append(f"INVALID ({r_txt})" if is_inv else r_txt)
                lines.append("| " + " | ".join(row) + " |")
        lines.append("")

    real_io_names = [s.name for s in selected_specs if s.category == "real_io" and audit_map.get(s.name, {}).get("classification") in CORO_VALID_PATHS]
    fanout_names = [s.name for s in selected_specs if s.category == "fanout" and audit_map.get(s.name, {}).get("classification") in CORO_VALID_PATHS]
    park_names = [s.name for s in selected_specs if s.category == "massive_park" and audit_map.get(s.name, {}).get("classification") in CORO_VALID_PATHS]
    phase_names = [s.name for s in selected_specs if s.category == "phase_attrib" and audit_map.get(s.name, {}).get("classification") in CORO_VALID_PATHS]
    fallback_names = [s.name for s in selected_specs if audit_map.get(s.name, {}).get("classification") == "ordinary-fallback"]
    eliminated_names = [s.name for s in selected_specs if audit_map.get(s.name, {}).get("classification") == "compiler-eliminated"]

    render_workload_table("3. Core Dashboard — Real I/O (Runtime-Verified Coroutine Paths)", real_io_names)
    render_workload_table("4. Core Dashboard — Parameterized & Capturing Fan-out/Fan-in (Runtime-Verified Coroutine Paths)", fanout_names)

    lines.append("## 5. Core Dashboard — Massive Parking (RSS, Wake Tail Latency & GC Reclamation)")
    lines.append("")
    if park_names:
        for bname in park_names:
            is_inv = (bname, 1) in invalid_set
            cls = audit_map[bname]["per_cpu"].get("1", {}).get("classification", audit_map[bname]["classification"])
            tag = f"INVALID ({cls})" if is_inv else cls
            lines.append(f"### `{bname}` (`{tag}`)")
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
                rparts = [
                    f"`{rk}`: {'INVALID (' + fmt_ratio(entry['ratios'].get(rk)) + ')' if is_inv else fmt_ratio(entry['ratios'].get(rk))}"
                    for rk in ratio_keys
                    if rk in entry["ratios"]
                ]
                lines.append("- **Wall-time paired ratios (`ns/op`)**: " + " | ".join(rparts))
                lines.append("")
    else:
        lines.append("_None._\n")

    render_workload_table("6. Phase Attribution — Raw Measurements (Launch / Register / Wake / Execute / Recycle)", phase_names)

    lines.append("### Derived Per-Task Cost Breakdown")
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


def _format_audit_details(a: dict[str, Any]) -> str:
    details: list[str] = []
    if a["skipped"]:
        details.append("skip: " + "; ".join(a["skipped"]))
    if a["missing_coro_symbols"]:
        details.append("no .coro: " + ", ".join(a["missing_coro_symbols"]))
    if a["forbidden_hits"]:
        details.append("forbidden: " + "; ".join(a["forbidden_hits"]))
    if a["missing_required_ops"]:
        details.append("missing ops: " + "; ".join(a["missing_required_ops"]))
    if details:
        return "; ".join(details)
    return "static ops + per-P runtime hardware frames verified"


def compute_phase_attribution(summary: dict[str, Any], mode_order: list[str], cpus: list[int]) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for cpu in cpus:
        cs = str(cpu)

        def get_ns(bench: str, mode: str) -> float | None:
            return summary.get(bench, {}).get(cs, {}).get("medians", {}).get(mode, {}).get("ns/op")

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
    ap.add_argument("--git-repo", help="Git repository used to verify full commit SHAs and ancestry")
    ap.add_argument("--upstream-ref", default="master", help="Git ref used to verify upstream merge-base")
    ap.add_argument("--filter", help="Optional regex selecting a subset of benchmarks to run")
    ap.add_argument("--samples", type=int, default=6, help="Measured interleaved rounds for regular benchmarks")
    ap.add_argument("--warmups", type=int, default=2, help="Warmup rounds discarded before recording")
    ap.add_argument("--park-samples", type=int, default=4, help="Measured fresh-process samples for MassivePark*")
    ap.add_argument("--benchtime", default="200ms", help="go test -benchtime for regular benchmarks")
    ap.add_argument("--cpus", default="1,2,4", help="Comma-separated GOMAXPROCS values")
    ap.add_argument("--allow-dirty", action="store_true", help="Allow dirty worktree during local testing")
    ap.add_argument("--allow-same-commit", action="store_true", help="Allow parent==candidate SHA in Mode-3 smoke tests")
    args = ap.parse_args(argv)

    suite_dir = Path(__file__).resolve().parent
    out_dir = Path(args.output).resolve()
    if out_dir.exists():
        raise SystemExit(f"output directory already exists: {out_dir}")
    out_dir.mkdir(parents=True)
    raw_dir = out_dir / "raw"
    raw_dir.mkdir()

    cpus = [int(x.strip()) for x in args.cpus.split(",") if x.strip()]

    if args.filter:
        flt = re.compile(args.filter)
        selected_specs = [s for s in BENCHMARKS if flt.search(s.name) or s.expected_bucket == "ordinary-fallback"]
        if not any(s.expected_bucket == "coro" for s in selected_specs):
            raise SystemExit(f"--filter {args.filter!r} matched no core benchmarks")
    else:
        selected_specs = list(BENCHMARKS)

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

    git_repo = resolve_git_repo(sdks, args.git_repo, suite_dir)
    provenance = verify_sdk_identities(sdks, git_repo, args.upstream_ref, args.allow_same_commit)

    # Step 1: Build & correctness check all SDKs.
    builds: dict[str, dict[str, Any]] = {}
    for label in mode_order:
        builds[label] = compile_and_check(sdks[label], suite_dir, out_dir)

    # Step 2: Mandatory static + per-P runtime execution path audit on every coro-enabled SDK.
    audits: dict[str, list[dict[str, Any]]] = {}
    for label in mode_order:
        if sdks[label]["goexperiment"] == "coro":
            audits[label] = audit_coro_mode(sdks[label], builds[label], selected_specs, cpus, out_dir)

    validation = validate_comparison_audits(audits, selected_specs)

    primary_coro_mode = (
        "candidate" if "candidate" in sdks else ("coro" if "coro" in sdks else mode_order[-1])
    )

    # Step 3: Collect interleaved regular benchmarks.
    raw_samples: dict[str, dict[int, dict[str, list[dict[str, float]]]]] = {}

    regular_specs = [s for s in selected_specs if not s.fresh_process_1x]
    park_specs = [s for s in selected_specs if s.fresh_process_1x]

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
    baseline_stability = compute_baseline_stability(summary)

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
            "filter": args.filter,
            "upstream_merge_base": provenance.get("upstream_merge_base"),
        },
        "provenance": provenance,
        "validation": validation,
        "baseline_stability": baseline_stability,
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
    if not validation["valid"]:
        print(
            f"suitev2 comparison INVALIDATED ({validation['failure_count']} failure(s)); see {out_dir}/report.md",
            file=sys.stderr,
        )
        return 2
    print(f"suitev2 benchmark complete: {out_dir}/report.md")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
