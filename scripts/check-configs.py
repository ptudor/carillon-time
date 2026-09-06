#!/usr/bin/env python3
"""Exercise shipped configurations with temporary paths and no clock/device access."""

import pathlib
import re
import subprocess
import sys
import tempfile


def main():
    root = pathlib.Path(__file__).resolve().parent.parent
    binary = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else root / "bin/carillon").resolve()
    for example in ("deploy/carillon.toml.example", "packaging/carillon.toml"):
        with tempfile.TemporaryDirectory(prefix="carillon-config-") as scratch:
            config = (root / example).read_text()
            for key, filename in (("drift_file", "drift"), ("control", "carillon.sock")):
                config, count = re.subn(
                    rf"(?m)^#?{key}\s*=.*$",
                    lambda _: f'{key} = "{scratch}/{filename}"',
                    config,
                )
                if count != 1:
                    raise ValueError(f"{example}: expected exactly one {key} example")
            path = pathlib.Path(scratch) / "config.toml"
            path.write_text(config)
            subprocess.run([str(binary), "-check", "-config", str(path)], check=True)
            print(f"{example}: passed", flush=True)


if __name__ == "__main__":
    main()
