#!/usr/bin/env python3
"""Apply RF5X-012's literal dispersion change only in a Go build overlay.

Runs existing simulations without changing the worktree or the jitter,
staleness, and Samples calculations. A failing simulation is evidence for
retaining the skip until candidate admission is designed together with it.
"""
import json
from pathlib import Path
import subprocess
import sys
import tempfile

here = Path(__file__).resolve().parent
root = here.parents[3]
source = root / "internal/discipline/filter.go"
old = "for i := len(order) - 1; i >= 0; i-- {\n\t\ts := &f.stages[order[i].idx]"
new = """for i := FilterStages - 1; i >= 0; i-- {
		if i >= len(order) {
			dispersion = 0.5 * (dispersion + MaxDispersion)
			continue
		}
		s := &f.stages[order[i].idx]"""
original = source.read_text()
if original.count(old) != 1:
    sys.exit("Filter has changed: inspect and update the experiment before running it.")
with tempfile.TemporaryDirectory(prefix="carillon-fable5-dispersion-") as temporary:
    folder = Path(temporary)
    replacement = folder / "filter.go"
    replacement.write_text(original.replace(old, new))
    overlay = folder / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {str(source): str(replacement)}}))
    sys.exit(subprocess.run([
        "go", "test", "-overlay", str(overlay), "-count=1", "-v",
        "-run", "^(TestSystemRemoveSource|TestSimFalseticker|TestSimConvergesFromUnknownFrequency)$",
        "./internal/discipline",
    ], cwd=root).returncode)
