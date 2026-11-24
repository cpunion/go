#!/usr/bin/env python3
# Copyright 2026 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import run_v2  # noqa: E402


class TestRunV2Harness(unittest.TestCase):
    def test_parse_go_benchmark_output(self):
        sample = """goos: darwin
goarch: arm64
BenchmarkFanoutArgs16-2    1000   7412.5 ns/op   2158500 ops/s   7100 p50-ns   8200 p95-ns   9500 p99-ns   1024 B/op   32 allocs/op
BenchmarkMassivePark10k       1   95000000 ns/op   3500 spawn-park-ns/task   420.5 parked-rss-B/task
"""
        parsed = run_v2.parse_go_benchmark_output(sample)
        self.assertIn(("FanoutArgs16", 2), parsed)
        self.assertAlmostEqual(parsed[("FanoutArgs16", 2)]["ns/op"], 7412.5)
        self.assertAlmostEqual(parsed[("FanoutArgs16", 2)]["p99-ns"], 9500.0)
        self.assertIn(("MassivePark10k", 1), parsed)
        self.assertAlmostEqual(parsed[("MassivePark10k", 1)]["parked-rss-B/task"], 420.5)

    def test_classify_runtime_and_static_audit(self):
        spec = next(s for s in run_v2.BENCHMARKS if s.name == "FanoutArgs16")
        nm = {"pkg.runFanoutArgs.coro", "pkg.fanoutArgsWorker.coro"}
        coro_ok = {
            "runFanoutArgs": "CALL runtime.coroSpawnFrame(SB)\nCALL runtime.coroChanRecv(SB)\n",
            "fanoutArgsWorker": "CALL runtime.coroChanSend(SB)\n",
        }
        entry_ok = {
            "runFanoutArgs": "CALL runtime.coroUseOrdinary(SB)\nCALL runtime.newproc(SB)\nCALL runtime.coroRunFrame(SB)\n",
            "fanoutArgsWorker": "",
        }
        rt_native = {1: {"observed_paths": ["native-coro"]}, 2: {"observed_paths": ["native-coro"]}}
        res = run_v2.classify_benchmark_audit(spec, "", nm, coro_ok, entry_ok, runtime_by_cpu=rt_native, target_cpus=[1, 2])
        self.assertEqual(res["classification"], "native-coro")
        self.assertEqual(res["per_cpu"]["2"]["classification"], "native-coro")

        # Runtime managed-coro observed even on a host capable of native: must report managed-coro.
        rt_managed = {1: {"observed_paths": ["managed-coro"]}, 2: {"observed_paths": ["managed-coro"]}}
        res_m = run_v2.classify_benchmark_audit(spec, "", nm, coro_ok, entry_ok, runtime_by_cpu=rt_managed, target_cpus=[1, 2])
        self.assertEqual(res_m["classification"], "managed-coro")

        # Runtime ordinary-fallback observed despite static .coro symbols (e.g. dynamic coroUseOrdinary()).
        rt_fb = {1: {"observed_paths": ["native-coro"]}, 2: {"observed_paths": ["ordinary-fallback"]}}
        res_fb = run_v2.classify_benchmark_audit(spec, "", nm, coro_ok, entry_ok, runtime_by_cpu=rt_fb, target_cpus=[1, 2])
        self.assertEqual(res_fb["per_cpu"]["1"]["classification"], "native-coro")
        self.assertEqual(res_fb["per_cpu"]["2"]["classification"], "ordinary-fallback")
        self.assertEqual(res_fb["classification"], "ordinary-fallback")

        # Compiler-eliminated required op inside .coro.func*.
        coro_elim = {
            "runFanoutArgs": "CALL runtime.coroSpawnFrame(SB)\n",
            "fanoutArgsWorker": "CALL runtime.coroChanSend(SB)\n",
        }
        res_e = run_v2.classify_benchmark_audit(spec, "", nm, coro_elim, entry_ok, runtime_by_cpu=rt_native, target_cpus=[1])
        self.assertEqual(res_e["classification"], "compiler-eliminated")

    def test_mode3_invalidation_on_backend_change_or_fallback(self):
        spec = next(s for s in run_v2.BENCHMARKS if s.name == "FanoutArgs16")
        fb_spec = next(s for s in run_v2.BENCHMARKS if s.name == "FallbackOrdinaryHost16")

        parent_audits = [
            {"benchmark": "FanoutArgs16", "per_cpu": {"1": {"classification": "native-coro"}, "2": {"classification": "native-coro"}}},
            {"benchmark": "FallbackOrdinaryHost16", "per_cpu": {"1": {"classification": "ordinary-fallback"}}},
        ]
        # Case 1: exact match -> valid
        cand_ok = [
            {"benchmark": "FanoutArgs16", "per_cpu": {"1": {"classification": "native-coro"}, "2": {"classification": "native-coro"}}},
            {"benchmark": "FallbackOrdinaryHost16", "per_cpu": {"1": {"classification": "ordinary-fallback"}}},
        ]
        v_ok = run_v2.validate_comparison_audits({"parent": parent_audits, "candidate": cand_ok}, [spec, fb_spec])
        self.assertTrue(v_ok["valid"])

        # Case 2: candidate switches from native-coro to managed-coro at P=2 -> INVALID
        cand_shift = [
            {"benchmark": "FanoutArgs16", "per_cpu": {"1": {"classification": "native-coro"}, "2": {"classification": "managed-coro"}}},
            {"benchmark": "FallbackOrdinaryHost16", "per_cpu": {"1": {"classification": "ordinary-fallback"}}},
        ]
        v_shift = run_v2.validate_comparison_audits({"parent": parent_audits, "candidate": cand_shift}, [spec, fb_spec])
        self.assertFalse(v_shift["valid"])
        self.assertIn(["FanoutArgs16", 2], v_shift["invalid_pairs"])

        # Case 3: candidate unexpectedly falls back to ordinary-fallback -> INVALID
        cand_fb = [
            {"benchmark": "FanoutArgs16", "per_cpu": {"1": {"classification": "ordinary-fallback"}, "2": {"classification": "native-coro"}}},
            {"benchmark": "FallbackOrdinaryHost16", "per_cpu": {"1": {"classification": "ordinary-fallback"}}},
        ]
        v_fb = run_v2.validate_comparison_audits({"parent": parent_audits, "candidate": cand_fb}, [spec, fb_spec])
        self.assertFalse(v_fb["valid"])
        self.assertIn(["FanoutArgs16", 1], v_fb["invalid_pairs"])

    def test_manifest_tampering_rejected(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            (root / "bin").mkdir()
            (root / "pkg" / "tool" / "linux_arm64").mkdir(parents=True)
            go_bin = root / "bin" / "go"
            compile_bin = root / "pkg" / "tool" / "linux_arm64" / "compile"
            go_bin.write_bytes(b"fake-go")
            compile_bin.write_bytes(b"fake-compile")
            sha40 = "e277e2c6e30e30ae4e3e15bb6a06c34918f70c64"
            manifest = {
                "schema_version": 1,
                "git_rev": sha40,
                "git_dirty": False,
                "goexperiment": "coro",
                "go_bin_sha256": "0" * 64,  # intentionally wrong
                "compile_sha256": run_v2.sha256_file(compile_bin),
            }
            (root / "CORO_BUILD_MANIFEST.json").write_text(json.dumps(manifest), encoding="utf-8")

            def fake_run_cmd(cmd, cwd=None, env=None):
                if cmd[1:] == ["version"]:
                    return mock.Mock(returncode=0, stdout=f"go version devel go1.28-{sha40[:10]} X:coro linux/arm64\n", stderr="")
                if cmd[1:] == ["env", "GOOS"]:
                    return mock.Mock(returncode=0, stdout="linux\n", stderr="")
                if cmd[1:] == ["env", "GOARCH"]:
                    return mock.Mock(returncode=0, stdout="arm64\n", stderr="")
                raise AssertionError(cmd)

            with mock.patch.object(run_v2, "run_cmd", side_effect=fake_run_cmd):
                with self.assertRaises(SystemExit) as ctx:
                    run_v2.inspect_sdk("coro", root, "coro", allow_dirty=False)
                self.assertIn("go_bin_sha256 mismatch", str(ctx.exception))

    def test_paired_ratio_ci_and_stability(self):
        r = run_v2.paired_ratio_ci([200.0, 210.0, 190.0, 205.0], [100.0, 100.0, 100.0, 100.0])
        self.assertAlmostEqual(r["median_ratio"], 2.025)
        self.assertGreaterEqual(r["ci95"][0], 1.9)
        self.assertLessEqual(r["ci95"][1], 2.1)

        stab = run_v2.compute_baseline_stability({
            "A": {"1": {"ratios": {"nocoro/upstream": {"median_ratio": 1.01, "ci95": [0.99, 1.03]}}}},
            "B": {"1": {"ratios": {"nocoro/upstream": {"median_ratio": 1.08, "ci95": [1.02, 1.12]}}}},
        })
        self.assertEqual(stab["total"], 2)
        self.assertEqual(stab["outside_095_105_count"], 1)
        self.assertEqual(stab["ci95_excludes_1_count"], 1)


if __name__ == "__main__":
    unittest.main()
