#!/usr/bin/env python3
"""Prepare a stopped service's configuration DB for separate encryption settings.

Writes a new DB; never replaces the source or accesses a running service.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import sqlite3


def prepare_document(value):
    count = 0
    if isinstance(value, dict):
        for name, items in value.items():
            if name == "talkgroups" and isinstance(items, list):
                for group in items:
                    if not isinstance(group, dict) or "tgid" not in group:
                        continue
                    before = dict(group)
                    old = group.pop("secure_mode", "clear")
                    if old == "":
                        old = "clear"
                    group.setdefault("admission_mode", "both" if old == "selectable" else old)
                    group.setdefault("transmit_mode", old)
                    count += group != before
            count += prepare_document(items)
    elif isinstance(value, list):
        for item in value:
            count += prepare_document(item)
    return count


def prepare(source, destination):
    source, destination = Path(source).resolve(), Path(destination).resolve()
    if source == destination or destination.exists():
        raise ValueError("choose a new destination file; the source is kept unchanged")
    source_hash = hashlib.sha256(source.read_bytes()).hexdigest()
    changed_documents = changed_groups = 0
    try:
        with sqlite3.connect(source.as_uri() + "?mode=ro", uri=True) as original:
            fd = os.open(destination, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            os.close(fd)
            with sqlite3.connect(destination) as output:
                original.backup(output)
                rows = output.execute("SELECT name,document FROM framework_documents").fetchall()
                for name, raw in rows:
                    try:
                        value = json.loads(raw)
                    except (ValueError, UnicodeDecodeError):
                        continue
                    changed = prepare_document(value)
                    if changed:
                        output.execute("UPDATE framework_documents SET document=? WHERE name=?",
                                       (json.dumps(value, separators=(",", ":")).encode(), name))
                        changed_documents += 1
                        changed_groups += changed
                output.commit()
                if output.execute("PRAGMA integrity_check").fetchone()[0] != "ok":
                    raise ValueError("prepared database integrity check failed")
    except BaseException:
        destination.unlink(missing_ok=True)
        raise
    if hashlib.sha256(source.read_bytes()).hexdigest() != source_hash:
        destination.unlink(missing_ok=True)
        raise ValueError("source changed while preparing; stop the service and retry")
    return {"source": str(source), "source_sha256": source_hash,
            "prepared": str(destination),
            "prepared_sha256": hashlib.sha256(destination.read_bytes()).hexdigest(),
            "changed_documents": changed_documents, "changed_talkgroup_records": changed_groups}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", help="stopped service configuration.db")
    parser.add_argument("destination", help="new prepared DB on the Mac SSD")
    args = parser.parse_args()
    print(json.dumps(prepare(args.source, args.destination), indent=2))
