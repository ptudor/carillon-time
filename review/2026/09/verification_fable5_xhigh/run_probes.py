#!/usr/bin/env python3
"""Run independent verification assertions through a Go build overlay.

The .go.txt files deliberately include assertions for unresolved findings.
A nonzero exit is expected until those findings are fixed. The overlay adds
tests without editing production packages or the parallel session's files.
Extra arguments are passed to `go test`; for example:
  python3 review/2026/09/verification_fable5_xhigh/run_probes.py -race
  python3 review/2026/09/verification_fable5_xhigh/run_probes.py -c -o /tmp/server.test ./internal/server
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
    virtual = root / "internal" / package / ("verification_fable5_xhigh_" + probe.name.removesuffix(".txt"))
    replace[str(virtual)] = str(probe)
with tempfile.TemporaryDirectory(prefix="carillon-fable5-overlay-") as temporary:
    overlay = Path(temporary) / "overlay.json"
    overlay.write_text(json.dumps({"Replace": replace}))
    args = sys.argv[1:]
    if not any(arg.startswith("./") for arg in args):
        args.extend(sorted({"./internal/" + probe.name.split("_", 1)[0] for probe in here.glob("*_test.go.txt")}))
    command = ["go", "test", "-overlay", str(overlay), "-count=1", "-run", "^TestVerification", "-v", *args]
    sys.exit(subprocess.run(command, cwd=root).returncode)
