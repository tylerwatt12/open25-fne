#!/usr/bin/env python3
"""Check native packet-codec encrypted MI admission/generation using public keys.

Runs only a local --packet-codec subprocess; opens no modem or network connection.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import struct
import subprocess
import tempfile


def next_mi(mi):
    # AAAD-B section3/Figure3-1, PDF15/printed5, independently expressed as
    # a 64-bit integer. The final CAI octet is reserved and generated as zero.
    state = int.from_bytes(mi[:8], "big")
    for _ in range(64):
        bit = ((state >> 63) ^ (state >> 61) ^ (state >> 45) ^
               (state >> 37) ^ (state >> 26) ^ (state >> 14)) & 1
        state = ((state << 1) | bit) & ((1 << 64) - 1)
    return state.to_bytes(8, "big") + b"\x00"


def packet(op, algorithm, sequence, mi, name):
    payload = bytes(320 if op == 1 else 11)
    value = bytearray(64 + len(payload))
    value[:4] = b"DV25"
    value[4], value[6] = op, algorithm
    struct.pack_into(">H", value, 8, 7)
    struct.pack_into(">II", value, 12, 10003, 1001)
    struct.pack_into(">Q", value, 24, sequence)
    value[32:48] = hashlib.sha256(name.encode()).digest()[:16]
    value[48:57] = mi
    struct.pack_into(">H", value, 58, len(payload))
    value[64:] = payload
    return struct.pack(">I", len(value)) + value


def check(binary):
    # Verify the local LFSR oracle against the published Table3-1 first.
    assert next_mi(bytes.fromhex("1234567890ABCDEF00")).hex().upper() == "6FE2802AA403828B00"
    algorithms = (0x81, 0x84, 0xAA)
    operations = []
    for algorithm in algorithms:
        for mi in (bytes(9), bytes(8) + b"\x01"):
            operations.append((algorithm, 3, 0, packet(3, algorithm, 1, mi, f"invalid-{algorithm}")))
        operations.append((algorithm, 3, 1, packet(3, algorithm, 1, b"\x01" + bytes(8), f"valid-{algorithm}")))
        for sequence in range(1, 38):
            operations.append((algorithm, 1, sequence, packet(1, algorithm, sequence, bytes(9), f"encode-{algorithm}")))
    with tempfile.TemporaryDirectory(prefix="p25-public-mi-") as folder:
        path = Path(folder) / "public-fixture-keys"
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "w") as stream:
            stream.write("129:7:465BA1A7F83D10C2\n132:7:D59DA7112B0D0AF44544C055F381F2D3900896D1C4456078E8ABCD33B1CBADE0\n170:7:DEADBEEF69\n")
        result = subprocess.run([str(Path(binary).resolve()), "--packet-codec", str(path)],
                                input=b"".join(item[3] for item in operations),
                                capture_output=True, timeout=30, check=False)
    assert result.returncode == 0, "native codec exited unsuccessfully"
    cursor = 0
    indicators = {algorithm: [] for algorithm in algorithms}
    for algorithm, op, sequence, _ in operations:
        assert len(result.stdout) - cursor >= 4, "truncated response prefix"
        size = struct.unpack_from(">I", result.stdout, cursor)[0]
        cursor += 4
        value = result.stdout[cursor:cursor + size]
        cursor += size
        assert len(value) == size and value[:4] == b"DV25" and value[4] == op | 0x80
        if op == 3 and sequence == 0:
            assert size == 64 and value[5] == 1, "null effective MI accepted"
        else:
            assert size == 75 and value[5] == 0 and value[6] == algorithm, "valid public packet rejected"
            if op == 1:
                mi = value[48:57]
                assert any(mi[:8]) and mi[8] == 0, "invalid generated MI"
                indicators[algorithm].append(mi)
    assert cursor == len(result.stdout), "unexpected trailing response"
    for values in indicators.values():
        assert all(value == values[0] for value in values[:18])
        assert all(value == next_mi(values[0]) for value in values[18:36])
        assert values[36] == next_mi(next_mi(values[0]))
    return {"passed": True, "scope": "Local native packet codec only; no RF or OTAR activation",
            "algorithm_ids": list(algorithms), "null_effective_mi_rejections": 6,
            "generated_frames": 111, "mi_transitions_match_independent_lfsr": 6,
            "published_lfsr_table_3_1_checked": True,
            "binary_sha256": hashlib.sha256(Path(binary).read_bytes()).hexdigest(),
            "harris_tested": False}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary")
    args = parser.parse_args()
    print(json.dumps(check(args.binary), indent=2))
