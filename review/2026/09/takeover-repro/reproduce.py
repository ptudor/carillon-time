#!/usr/bin/env python3
"""Run takeover regressions with a Go overlay; never edit production sources.

Both variants are expected to fail on adbfbfe. The distance variant changes
only the under-Allan ranking to delay/2 + dispersion and demonstrates that
this proposal does not resolve every delayed-feedback case or persistence.
"""

import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("variant", choices=("baseline", "distance"))
    parser.add_argument("--go", default=shutil.which("go") or "/opt/local/bin/go")
    args = parser.parse_args()
    assets = Path(__file__).resolve().parent
    root = assets.parents[3]
    with tempfile.TemporaryDirectory(prefix="carillon-takeover-") as name:
        scratch = Path(name)
        mapping = {
            str(root / "internal/discipline/takeover_probe_test.go"):
                str(assets / "discipline_test.go.txt"),
            str(root / "internal/engine/takeover_probe_test.go"):
                str(assets / "engine_test.go.txt"),
        }
        if args.variant == "distance":
            source = root / "internal/discipline/filter.go"
            code = source.read_text()
            if code.count("d = s.delay\n") != 1:
                parser.error("filter ranking changed; review the candidate overlay")
            candidate = scratch / "filter.go"
            candidate.write_text(code.replace("d = s.delay\n", "d = s.delay/2 + s.disp\n"))
            mapping[str(source)] = str(candidate)
        overlay = scratch / "overlay.json"
        overlay.write_text(json.dumps({"Replace": mapping}))
        env = os.environ.copy()
        env["CGO_ENABLED"] = "0"
        env.setdefault("GOCACHE", str(scratch / "gocache"))
        env.setdefault("GOMODCACHE", str(scratch / "gomodcache"))
        result = subprocess.run(
            [args.go, "test", "-overlay", str(overlay), "./internal/discipline",
             "./internal/engine", "-run", "TestTakeover", "-v", "-count=1"],
            cwd=root, env=env, check=False,
        )
        return result.returncode


if __name__ == "__main__":
    raise SystemExit(main())
