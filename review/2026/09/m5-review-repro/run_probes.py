#!/usr/bin/env python3
"""Run the M5 review probes through a Go build overlay.

The .go.txt files assert the behaviour DESIGN.md and docs/leap-distribution.md
describe, so a nonzero exit is expected on 3413e23 until the corresponding
findings in review/2026/09/REVIEW_M5_FABLE51.md are fixed or the design is
amended. The overlay adds tests without editing production packages. Extra
arguments are passed to `go test`; for example:
  python3 review/2026/09/m5-review-repro/run_probes.py -race
  python3 review/2026/09/m5-review-repro/run_probes.py -race ./internal/engine
Every probe uses clock.Fake; nothing touches the host clock or the network
beyond a refused loopback connection.
"""
import json
from pathlib import Path
import subprocess
import sys
import tempfile

here = Path(__file__).resolve().parent
root = here.parents[3]
replace = {}
for probe in sorted(here.glob("*_test.go.txt")):
    package = probe.name.split("_", 1)[0]
    virtual = root / "internal" / package / ("m5_review_" + probe.name.removesuffix(".txt"))
    replace[str(virtual)] = str(probe)
with tempfile.TemporaryDirectory(prefix="carillon-m5-overlay-") as temporary:
    overlay = Path(temporary) / "overlay.json"
    overlay.write_text(json.dumps({"Replace": replace}))
    args = sys.argv[1:]
    if not any(arg.startswith("./") for arg in args):
        args.extend(sorted({"./internal/" + probe.name.split("_", 1)[0] for probe in here.glob("*_test.go.txt")}))
    command = ["go", "test", "-overlay", str(overlay), "-count=1", "-run", "^TestVerification", "-v", *args]
    sys.exit(subprocess.run(command, cwd=root).returncode)
