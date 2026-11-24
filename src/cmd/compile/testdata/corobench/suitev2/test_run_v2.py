#!/usr/bin/env python3
# Copyright 2026 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

"""Unit tests for the Stage-2 path-audit classifier and benchmark parser."""

import unittest

import run_v2


class TestSuiteV2Harness(unittest.TestCase):
    def test_parse_benchmark_output(self) -> None:
        sample = (
            "BenchmarkFanoutArgs16-2    100    12345 ns/op    1296000 ops/s    "
            "11000 p50-ns    14000 p95-ns    18000 p99-ns    4096 B/op    32 allocs/op\n"
            "BenchmarkMassivePark10k    1    5000000 ns/op    2000000 ops/s    320 parked-heap-B/task\n"
        )
        parsed = run_v2.parse_go_benchmark_output(sample)
        self.assertIn(("FanoutArgs16", 2), parsed)
        self.assertEqual(parsed[("FanoutArgs16", 2)]["ns/op"], 12345.0)
        self.assertEqual(parsed[("FanoutArgs16", 2)]["p99-ns"], 18000.0)
        self.assertIn(("MassivePark10k", 1), parsed)
        self.assertEqual(parsed[("MassivePark10k", 1)]["parked-heap-B/task"], 320.0)

    def test_classify_native_managed_fallback_and_eliminated(self) -> None:
        spec = run_v2.BenchmarkSpec(
            name="FanoutArgs16",
            category="fanout",
            entry_funcs=("runFanoutArgs", "fanoutArgsWorker"),
            required_ops={
                "runFanoutArgs": (r"runtime\.coroSpawnFrame", r"runtime\.coroChanRecv"),
                "fanoutArgsWorker": (r"runtime\.coroChanSend",),
            },
            forbidden_ops={"runFanoutArgs": (r"runtime\.newproc",)},
        )
        good_nm = {"pkg.runFanoutArgs.coro", "pkg.fanoutArgsWorker.coro"}
        good_obj = {
            "runFanoutArgs": "CALL runtime.coroSpawnFrame(SB)\nCALL runtime.coroChanRecv(SB)\n",
            "fanoutArgsWorker": "CALL runtime.coroChanSend(SB)\n",
        }
        # Public entry guard legitimately retains runtime.newproc for coroUseOrdinary fallback.
        entry_obj = {"runFanoutArgs": "CALL runtime.coroUseOrdinary(SB)\nCALL runtime.newproc(SB)\n"}
        # 1. Native coro (public entry newproc does NOT trigger false fallback!)
        res = run_v2.classify_benchmark_audit(spec, "", good_nm, good_obj, entry_obj, "native-coro")
        self.assertEqual(res["classification"], "native-coro")
        # 2. Managed coro
        res = run_v2.classify_benchmark_audit(spec, "", good_nm, good_obj, entry_obj, "managed-coro")
        self.assertEqual(res["classification"], "managed-coro")
        # 3. Ordinary fallback via coro: skip
        skip_log = "coro: skip pkg.runFanoutArgs: ordinary helper requires growing Go stack\n"
        res = run_v2.classify_benchmark_audit(spec, skip_log, set(), {}, entry_obj, "native-coro")
        self.assertEqual(res["classification"], "ordinary-fallback")
        # 4. Ordinary fallback via runtime.newproc inside lowered state machine
        fb_obj = dict(good_obj)
        fb_obj["runFanoutArgs"] += "CALL runtime.newproc(SB)\n"
        res = run_v2.classify_benchmark_audit(spec, "", good_nm, fb_obj, entry_obj, "native-coro")
        self.assertEqual(res["classification"], "ordinary-fallback")
        # 5. Compiler-eliminated: lowered to .coro, no fallback, but channel op disappeared!
        elim_obj = {
            "runFanoutArgs": "CALL runtime.coroSpawnFrame(SB)\n",  # coroChanRecv folded away!
            "fanoutArgsWorker": "",  # coroChanSend folded away!
        }
        res = run_v2.classify_benchmark_audit(spec, "", good_nm, elim_obj, entry_obj, "native-coro")
        self.assertEqual(res["classification"], "compiler-eliminated")
        self.assertEqual(len(res["missing_required_ops"]), 2)

    def test_paired_ratio_ci(self) -> None:
        info = run_v2.paired_ratio_ci([100.0, 110.0, 90.0, 100.0], [100.0, 100.0, 100.0, 100.0])
        self.assertAlmostEqual(info["median_ratio"], 1.0)
        self.assertLessEqual(info["ci95"][0], 1.0)
        self.assertGreaterEqual(info["ci95"][1], 1.0)


if __name__ == "__main__":
    unittest.main()
