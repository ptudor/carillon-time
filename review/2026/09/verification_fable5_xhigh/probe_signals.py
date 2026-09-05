#!/usr/bin/env python3
"""Exercise the daemon's real signal and auxiliary-shutdown wiring.

A Go overlay replaces clock.New with clock.Fake and bypasses the UDP/123
conflict probe. No production source files or host clocks are modified.
The only upstream is an unprivileged local UDP socket held by this harness.
"""
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import time

here = Path(__file__).resolve().parent
root = here.parents[3]
source = root / "cmd/carillon/main.go"
original = source.read_text()
replacements = {
    "clk, err := clock.New()": "clk, err := func() (clock.Clock, error) { return clock.NewFake(time.Now()), nil }()",
    "if err := probeNTPPort(log); err != nil {": "if err := error(nil); err != nil {",
}
for old, new in replacements.items():
    if original.count(old) != 1:
        raise SystemExit("main.go changed; inspect the fake-clock seams before running.")
    original = original.replace(old, new)

with tempfile.TemporaryDirectory(prefix="fable5-signals-", dir="/tmp") as temporary:
    folder = Path(temporary)
    substitute = folder / "main.go"
    substitute.write_text(original)
    overlay = folder / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {str(source): str(substitute)}}))
    binary = folder / "carillon-fake"
    subprocess.run(["go", "build", "-overlay", str(overlay), "-o", str(binary), "./cmd/carillon"], cwd=root, check=True)
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as upstream:
        upstream.bind(("127.0.0.1", 0))
        control = folder / "control.sock"
        config = folder / "test.toml"
        config.write_text(f'''[daemon]
drift_file = "{folder / 'drift'}"
control = "{control}"
[[server]]
address = "127.0.0.1:{upstream.getsockname()[1]}"
''')
        log = folder / "daemon.log"
        with log.open("w") as output:
            child = subprocess.Popen([str(binary), "-config", str(config)], stdout=output, stderr=output)
            try:
                def version():
                    with socket.socket(socket.AF_UNIX) as client:
                        client.settimeout(2)
                        client.connect(str(control))
                        client.sendall(b'{"command":"version"}\n')
                        return json.loads(client.recv(4096))
                deadline = time.monotonic() + 8
                while time.monotonic() < deadline:
                    if child.poll() is not None:
                        raise AssertionError(log.read_text())
                    try:
                        version()
                        break
                    except (FileNotFoundError, ConnectionRefusedError):
                        time.sleep(0.02)
                else:
                    raise AssertionError("control server did not become ready")
                for _ in range(2):
                    os.kill(child.pid, signal.SIGHUP)
                    time.sleep(0.05)
                    assert "version" in version(), log.read_text()
                os.kill(child.pid, signal.SIGTERM)
                assert child.wait(timeout=6) == 0, log.read_text()
                assert log.read_text().count("SIGHUP ignored") == 1, log.read_text()
                print("PASS: two SIGHUPs kept control serving, warning logged once; SIGTERM exited 0 within 6 s (clock.Fake).")
            finally:
                if child.poll() is None:
                    child.kill()
                    child.wait(timeout=3)
