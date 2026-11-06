#!/usr/bin/env python3
"""Independently derive bounded Phase1 AMBT and packet CRC fixtures.

Uses literal standard field layouts and MSB-first polynomial long division.
Does not import native codec tables, start a service, or access a modem.
Native tests consume the explicit byte strings checked here, not generated data.
"""
import argparse
import hashlib
import json
from pathlib import Path


def crc(data, width, polynomial):
    bits = [int(c) for byte in data for c in format(byte, "08b")] + [0] * width
    divisor = [int(c) for c in format(polynomial, f"0{width + 1}b")]
    for pos in range(len(data) * 8):
        if bits[pos]:
            for n, value in enumerate(divisor):
                bits[pos + n] ^= value
    return (int("".join(map(str, bits[-width:])), 2) ^ ((1 << width) - 1)).to_bytes(width // 8, "big")


# AABC-E April2019, seven ISP extended formats. Source working address123456,
# local home WACNABCDE/System789, target654321, statusC3D4/messageBEEF/groupA1B2.
FORMS = (
    ("status", 0x18, "C3D4", "ABCDE78965432100", "17FD001234568118C3D4127B", "ABCDE7896543210019753E47", "6.1.15.2/Fig6.1.15-2", 119, "100"),
    ("message", 0x1C, "BEEF", "ABCDE78965432100", "17FD00123456811CBEEF3786", "ABCDE7896543210019753E47", "6.1.11.2/Fig6.1.11-2", 112, "93"),
    ("call_alert", 0x1F, "ABCD", "E789654321000000", "17FD00123456811FABCD9670", "E789654321000000D4637F80", "6.1.4.2/Fig6.1.4-2", 100, "81"),
    ("ack", 0x20, "18AB", "CDE7896543210000", "17FD00123456812018AB38DA", "CDE7896543210000C431254E", "6.1.1.2/Fig6.1.1-2", 95, "76"),
    ("group_affiliation", 0x28, "ABCD", "E789A1B280000000", "17FD001234568128ABCDD645", "E789A1B2800000009F0BEF96", "6.1.9.2/Fig6.1.9-2", 109, "90"),
    ("cancel", 0x23, "8410", "00ABCDE789654321", "17FD00123456812384102ABC", "00ABCDE789654321F73F7DD3", "6.1.5.2/Fig6.1.5-2", 102, "83"),
    ("extended_function_response", 0x24, "ABCD", "E789008065432100", "17FD001234568124ABCDA324", "E789008065432100E07B8FE5", "6.1.7.2/Fig6.1.7-2", 106, "87"),
)
PACKETS = (
    (0x16, 16, 0, "000000000000000000000000FFFFFFFF"),
    (0x16, 16, 1, "010000000000000000000000F8091CF9"),
    (0x16, 16, 12, "0102030405060708090A0B0C3C183E49"),
    (0x16, 16, 28, "0102030405060708090A0B0C0D0E0F101112131415161718191A1B1C754815AA"),
    (0x15, 12, 0, "0000000000000000FFFFFFFF"),
    (0x15, 12, 1, "0100000000000000A45E2335"),
    (0x15, 12, 8, "010203040506070882F0C97E"),
    (0x15, 12, 20, "0102030405060708090A0B0C0D0E0F10111213143E889BD2"),
)


def check(native_source=None):
    messages = []
    for name, opcode, fields, body_hex, expected_h, expected_b, clause, pdf, printed in FORMS:
        header = bytes.fromhex("17FD0012345681") + bytes([opcode]) + bytes.fromhex(fields)
        body = bytes.fromhex(body_hex)
        actual_h = (header + crc(header, 16, 0x11021)).hex().upper()
        actual_b = (body + crc(body, 32, 0x104C11DB7)).hex().upper()
        assert actual_h == expected_h and actual_b == expected_b, name
        messages.append(dict(name=name, header=actual_h, body=actual_b,
                             clause=clause, pdf_page=pdf, printed_page=printed))
    packets = []
    for format_, block, user, expected in PACKETS:
        count = (user + 4 + block - 1) // block
        pad = count * block - user - 4
        body = bytes(range(1, user + 1)) + bytes(pad)
        actual = (body + crc(body, 32, 0x104C11DB7)).hex().upper()
        assert actual == expected, (format_, user)
        packets.append(dict(format=format_, user_length=user, blocks_to_follow=count,
                            pad_length=pad, body=actual))
    report = dict(passed=True, scope="Software fixtures; no RF or interoperability qualification",
                  messages=messages, packets=packets, native_fixture_hashes={},
                  message_source=dict(designation="TIA-102.AABC-E April2019",
                      document_id="p25_89a3b878eee70a14",
                      pdf="documents/tia-library/8.10_TIA-102-AABC-E_Final Published Document_2019_04_231.pdf"),
                  crc_source=dict(designation="TIA-102.BAAA-B June2017",
                      document_id="p25_d1460d9ae0713992", clauses="5.2,5.3.1,5.3.3",
                      pdf_pages=[38,39], printed_pages=["24","25"],
                      pdf="documents/tia-library/8.15_TIA-102.BAAA-B_Final Published Document_2017_06_22.pdf"))
    if native_source:
        root = Path(native_source)
        for name, literals in (("AMBTLayouts_Tests.cpp", [x for row in FORMS for x in row[4:6]]),
                               ("PacketLengthBoundary_Tests.cpp", [row[3] for row in PACKETS])):
            path = root / "tests/p25" / name
            text = path.read_text()
            assert all(value in text for value in literals), f"missing literal fixture in {name}"
            report["native_fixture_hashes"][str(path.relative_to(root))] = hashlib.sha256(path.read_bytes()).hexdigest()
    return report


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--native-source", type=Path)
    args = parser.parse_args()
    print(json.dumps(check(args.native_source), indent=2))
