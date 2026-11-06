#!/usr/bin/env python3
"""Compare a local native codec with public, independent Phase 1 oracle output.

This opens no radio device or network connection. The key values in the supplied
fixture are public test material from the named optional-module source, never
operational key inventories.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import struct
import subprocess
import tempfile


def check(binary, fixture):
    document = json.loads(Path(fixture).read_text())
    vectors = document["vectors"]
    identities = {}
    packets = []
    for number, vector in enumerate(vectors):
        algorithm = vector["algorithm_id"]
        identities[algorithm] = vector["fixture_key_hex"]
        packet = bytearray(75)
        packet[:4] = b"DV25"
        packet[4], packet[6], packet[7] = 3, algorithm, vector["index"]
        struct.pack_into(">H", packet, 8, 7)
        struct.pack_into(">II", packet, 12, 10003, 1001)
        struct.pack_into(">Q", packet, 24, number + 1)
        packet[32:48] = hashlib.sha256(str(algorithm).encode()).digest()[:16]
        packet[48:57] = bytes.fromhex(vector["mi"])
        struct.pack_into(">H", packet, 58, 11)
        packets.append(struct.pack(">I", len(packet)) + packet)
    with tempfile.TemporaryDirectory(prefix="p25-public-crypto-") as folder:
        key_file = Path(folder) / "public-fixture-keys"
        descriptor = os.open(key_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "w") as stream:
            for algorithm, material in identities.items():
                stream.write(f"{algorithm}:7:{material}\n")
        result = subprocess.run(
            [str(Path(binary).resolve()), "--packet-codec", str(key_file)],
            input=b"".join(packets), capture_output=True, timeout=30, check=False,
        )
    if result.returncode:
        raise RuntimeError("native fixture codec exited unsuccessfully")
    cursor = 0
    for vector in vectors:
        if len(result.stdout) - cursor < 4:
            raise RuntimeError("native fixture response was truncated")
        size = struct.unpack_from(">I", result.stdout, cursor)[0]
        cursor += 4
        packet = result.stdout[cursor:cursor + size]
        cursor += size
        if len(packet) != 75 or packet[:4] != b"DV25" or packet[4:6] != b"\x83\x00":
            raise RuntimeError("native fixture transform was rejected")
        if packet[6] != vector["algorithm_id"] or packet[7] != vector["index"]:
            raise RuntimeError("native fixture response changed algorithm or voice position")
        if packet[64:].hex().upper() != vector["xor_stream_11"]:
            raise RuntimeError(
                f"independent stream mismatch: algorithm {vector['algorithm_id']}, "
                f"voice position {vector['index']}"
            )
    if cursor != len(result.stdout):
        raise RuntimeError("unexpected trailing native fixture response")
    return {
        "passed": True,
        "scope": "Native Phase 1 packed IMBE compared with unchanged optional-module Java output; no RF",
        "source_commit": document["source_commit"],
        "vectors": len(vectors),
        "algorithm_ids": sorted(identities),
        "fixture_sha256": hashlib.sha256(Path(fixture).read_bytes()).hexdigest(),
        "harris_tested": False,
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary")
    parser.add_argument("fixture")
    args = parser.parse_args()
    print(json.dumps(check(args.binary, args.fixture), indent=2))
