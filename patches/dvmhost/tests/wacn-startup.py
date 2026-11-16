#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-2.0-only
# Open25 contributors, 2026-11-16: offline native startup policy regression.
"""Exercise real native process startup with software-only failure fixtures.

All configs have no modem device and no network/audio configuration. Allowed
identities stop at a later, explicit fixture error; this does not test RF use.
"""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--bin-dir", type=Path, required=True)
parser.add_argument("--work-dir", type=Path, required=True)
args = parser.parse_args()
args.work_dir = args.work_dir.resolve()
args.work_dir.mkdir(parents=True, exist_ok=True)
message = ("WACN {wacn:05X} is blocked for operation: BEE00-BEEFF is restricted "
           "to reduce unintended subscriber roaming risk")
base = """daemon: false
iAgreeNotToBeStupid: true
log:
    displayLevel: 1
    fileLevel: 0
    filePath: .
    activityFilePath: .
    fileRoot: offline-wacn
protocols:
    p25:
        enable: true
system:
    identity: OFFLINE
    localAudio: false
    netId: '{wacn:05X}'
    config:
        netId: '{wacn:05X}'
    modem:
        protocol:
            type: null
network:
    sourceTGID: 1
    destinationTGID: 1
"""
# Each sentinel is reached during parsing, before networking/modem/audio setup.
sentinels = {
    "dvmhost": "No bandplan identity table?",
    "dvmbridge": "Must at least local audio or UDP audio!",
    "dvmpatch": "Patch source TGID and destination TGID cannot be the same.",
}
report = {"blocked_per_binary": {}, "allowed_per_binary": {}, "binary_sha256": {}}
with tempfile.TemporaryDirectory(prefix="wacn-startup-", dir=args.work_dir) as work:
    work = Path(work)
    config = work / "offline.yml"
    for name, sentinel in sentinels.items():
        binary = (args.bin_dir / name).resolve(strict=True)
        report["binary_sha256"][name] = hashlib.sha256(binary.read_bytes()).hexdigest()
        def run(wacn):
            original = base.format(wacn=wacn)
            config.write_text(original)
            completed = subprocess.run([str(binary), "-f", "-c", str(config)],
                                       cwd=work, capture_output=True, text=True, timeout=5)
            output = completed.stdout + completed.stderr
            assert completed.returncode == 1, (name, hex(wacn), completed.returncode, output)
            assert config.read_text() == original, (name, "configuration changed")
            return output
        for wacn in range(0xBEE00, 0xBEF00):
            output = run(wacn)
            assert message.format(wacn=wacn) in output, (name, hex(wacn), output)
            assert sentinel not in output, (name, "reached later startup stage", output)
            assert "error 4" not in output and "error 8" not in output and "error 16" not in output
        report["blocked_per_binary"][name] = 256
        for wacn in [1, 0xBB800, 0xBEDFF, 0xBEF00, 0xFFFFE]:
            output = run(wacn)
            assert "blocked for operation" not in output, (name, hex(wacn), output)
            assert sentinel in output, (name, "expected software-only stop", output)
        report["allowed_per_binary"][name] = 5
report["result"] = "pass"
print(json.dumps(report, indent=2))
