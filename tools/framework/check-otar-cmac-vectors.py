#!/usr/bin/env python3
"""Independently reproduce public TIA-102.AACA-D CMAC examples; opens no radio.

Requires Python cryptography. The TEK below is published example material, never
an operational key. This oracle is separate from the native OpenSSL EVP code.
"""
import hashlib
import hmac
import json
from cryptography.hazmat.primitives.cmac import CMAC
from cryptography.hazmat.primitives.ciphers import algorithms


def check():
    tek = bytes.fromhex("168562453B3E7F618D68B387E0B997E1FB0F264FA83B74E43B172917BD39339F")
    cases = [
        ("rekey_with_message_number", 12,
         "1E004DA8643BA8712B1D1772008450BC010001842801000000498380289CF635FB68D345D34F62EF063BA4E05CAE4756E7D30446D1F07C6EB4E9E0840945372372FB80A2C1CCD14201738C08842F6241",
         "C24C24E41A89DB2DC18EB88A17E1A8F1FB554676ADE244052A1F5BAA6D6FDADF", "A2C1CCD14201738C"),
        ("hello_without_message_number", 10,
         "0C001508643BA8712B1D0018216AD91E3ED5AC08842F6241",
         "407177CBC982A9D8FAA662B79399C62C9AD64458DF834A3EE3040B9E300553B1", "18216AD91E3ED5AC"),
    ]
    results = []
    for name, context_length, message_hex, expected_key, expected_mac in cases:
        message = bytes.fromhex(message_hex)
        kdf_input = b"\x01OTAR MAC\x00" + message[:context_length] + b"\x01\x00"
        key = hmac.new(tek, kdf_input, hashlib.sha256).digest()
        mac = CMAC(algorithms.AES(key))
        mac.update(message[:-13] + message[-5:])
        code = mac.finalize()[:8]
        assert key.hex().upper() == expected_key, name + ": derived-key mismatch"
        assert code.hex().upper() == expected_mac, name + ": CMAC mismatch"
        results.append({"name": name, "message_bytes": len(message), "context_bytes": context_length,
                        "derived_key_matches_pdf": True, "cmac_matches_pdf": True})
    return {
        "passed": True, "scope": "Independent public-vector computation, no RF or OTAR activation",
        "source": {"standard": "TIA-102.AACA-D", "edition": "April 2025",
                   "clauses": ["13.5.2.2.2", "14.3.5.1", "14.3.5.2"],
                   "pdf_pages": [188, 207, 208, 209], "printed_pages": ["170", "189", "190", "191"],
                   "document_id": "p25_2cc930b78d55ee64",
                   "pdf": "documents/tia-library/8.3_TIA-102.AACA-D_Final Published Document_rv1_2025_05_16.pdf"},
        "vectors": results, "harris_tested": False,
    }


if __name__ == "__main__":
    print(json.dumps(check(), indent=2))
