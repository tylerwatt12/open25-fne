#!/usr/bin/env python3
"""Prepare a separate Application Server state copy from a stopped backup.

No source file, running service, runtime, radio or firmware is changed. Only
service roles, declared paths in moved directories, database role metadata and the
topology URL field change; identities, credentials and key bytes are preserved.
"""

import argparse
from contextlib import closing
import ctypes
import fcntl
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import sqlite3
import stat
import sys
import tempfile


class PreparationError(Exception):
    pass


class JSONNumber(str):
    """An already parsed decimal token, retained without binary rounding."""


def nonfinite_number(_value):
    raise PreparationError("a JSON document contains a non-finite number")


def decode_json(raw):
    return json.loads(raw, object_pairs_hook=unique_object,
                      parse_float=JSONNumber, parse_constant=nonfinite_number)


def encode_json(value, indent=None, depth=0):
    if isinstance(value, JSONNumber):
        return str(value)
    if isinstance(value, (dict, list)):
        opening, closing = ("{", "}") if isinstance(value, dict) else ("[", "]")
        if not value:
            return opening + closing
        if isinstance(value, dict):
            entries = [json.dumps(key, ensure_ascii=True) + (": " if indent else ":") +
                       encode_json(item, indent, depth + 1) for key, item in value.items()]
        else:
            entries = [encode_json(item, indent, depth + 1) for item in value]
        if indent:
            return opening + "\n" + ",\n".join(" " * indent * (depth + 1) + entry for entry in entries) + "\n" + " " * indent * depth + closing
        return opening + ",".join(entries) + closing
    return json.dumps(value, ensure_ascii=True, allow_nan=False)


ROLES = {"system", "zone", "site", "dispatch"}
RECEIPTS = {"application-role-source-manifest.json",
            "application-role-prepared-manifest.json", "application-role-preparation.json"}
TABLE_COLUMNS = {
    "framework_metadata": ["key", "value"],
    "framework_documents": ["name", "document", "at"],
    "framework_administrator": ["singleton", "username", "password_hash"],
    "framework_events": ["id", "at", "kind", "message"],
}


def unique_object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise PreparationError("a JSON document contains duplicate fields")
        value[key] = item
    return value


def inventory(root):
    result = {}
    for current, directories, files in os.walk(root, followlinks=False):
        paths = [Path(current), *(Path(current) / name for name in files),
                 *(Path(current) / name for name in directories)]
        for path in paths:
            relative, info = path.relative_to(root).as_posix(), path.lstat()
            item = {"mode": stat.S_IMODE(info.st_mode)}
            if stat.S_ISDIR(info.st_mode):
                item["kind"] = "directory"
            elif stat.S_ISREG(info.st_mode):
                digest = hashlib.sha256()
                with path.open("rb") as content:
                    for block in iter(lambda: content.read(1 << 20), b""):
                        digest.update(block)
                item.update(kind="file", size=info.st_size,
                            sha256=digest.hexdigest())
            else:
                raise PreparationError("source contains a symlink or special file")
            result[relative] = item
    return dict(sorted(result.items()))


def read_json(path):
    try:
        value = decode_json(path.read_bytes())
    except (OSError, ValueError):
        raise PreparationError("an expected private JSON document is unreadable") from None
    if not isinstance(value, dict):
        raise PreparationError("an expected private JSON document is not an object")
    return value


def write_json(path, value, mode=0o600):
    descriptor, temporary = tempfile.mkstemp(prefix=".application-role-json-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "wb") as output:
            output.write((encode_json(value, indent=2) + "\n").encode())
            output.flush()
            os.fchmod(output.fileno(), mode)
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.lexists(temporary):
            os.unlink(temporary)


def moved(relative):
    parts = relative.parts
    if parts and parts[0] == "dispatch":
        return Path("application", *parts[1:])
    if parts and parts[0] == "dispatches":
        return Path("applications", *parts[1:])
    return relative


def deployment_path(value, deployment_root, owner):
    if type(value) is not str or not value or "\x00" in value:
        raise PreparationError("a service state path is invalid")
    path = PurePosixPath(value)
    if not path.is_absolute() or ".." in path.parts:
        raise PreparationError("a service state path is not an explicit deployment path")
    try:
        relative = path.relative_to(deployment_root)
        relative.relative_to(PurePosixPath(owner.as_posix()))
    except ValueError:
        raise PreparationError("a service state path leaves its owning deployment directory") from None
    return Path(relative.as_posix())


def moved_declared_path(value, deployment_root):
    if type(value) is not str or not value or not PurePosixPath(value).is_absolute():
        return value
    try:
        relative = PurePosixPath(value).relative_to(deployment_root)
    except ValueError:
        return value
    target = moved(Path(relative.as_posix()))
    if target == Path(relative.as_posix()):
        return value
    if ".." in relative.parts or "\x00" in value:
        raise PreparationError("a declared path in a moved directory has an unsupported layout")
    return str(deployment_root / PurePosixPath(target.as_posix()))


def collect_services(source, deployment_root):
    expected = {Path(role) / "service.json": role for role in ("system", "zone", "site", "dispatch")}
    for directory, role in (("sites", "site"), ("dispatches", "dispatch")):
        parent = source / directory
        if parent.exists():
            if not parent.is_dir():
                raise PreparationError("an additional-service directory is invalid")
            for child in parent.iterdir():
                if child.is_dir():
                    expected[child.relative_to(source) / "service.json"] = role
    if {path.relative_to(source) for path in source.rglob("service.json")} != set(expected):
        raise PreparationError("source does not contain the complete known service-directory layout")
    if (source / "application").exists() or (source / "applications").exists():
        raise PreparationError("source already contains an Application Server destination directory")
    services = []
    for relative, role in sorted(expected.items()):
        value = read_json(source / relative)
        if value.get("role") not in ROLES or value["role"] != role:
            raise PreparationError("a service role does not match its known directory")
        if type(value.get("instance_id")) is not str or not value["instance_id"]:
            raise PreparationError("a service instance identity is missing")
        state = deployment_path(value.get("state_dir"), deployment_root, relative.parent)
        credentials = deployment_path(value.get("credential_file"), deployment_root, relative.parent)
        if not (source / state).is_dir() or not (source / credentials).is_file():
            raise PreparationError("a service state or credential path is absent from the backup")
        database = state / "configuration.db"
        if not (source / database).is_file():
            raise PreparationError("a configuration database is absent from the backup")
        services.append({"config": relative, "role": role, "value": value,
                         "state": state, "credentials": credentials, "database": database})
    return services


def database_rows(connection):
    tables = [row[0] for row in connection.execute("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")]
    if {table for table in tables if not table.startswith("sqlite_")} != set(TABLE_COLUMNS):
        raise PreparationError("a configuration database is not in the expected current schema")
    if connection.execute("SELECT 1 FROM sqlite_master WHERE type='trigger' LIMIT 1").fetchone():
        raise PreparationError("a configuration database has unexpected triggers")
    for table, expected in TABLE_COLUMNS.items():
        columns = [row[1] for row in connection.execute('PRAGMA table_info("' + table + '")')]
        if columns != expected:
            raise PreparationError("a configuration database has unexpected columns")
    return {table: connection.execute('SELECT * FROM "' + table + '" ORDER BY rowid').fetchall() for table in tables}


def prepare_database(path, expected_role):
    mode = stat.S_IMODE(path.stat().st_mode)
    for suffix in ("", "-wal", "-shm", "-journal"):
        item = Path(str(path) + suffix)
        if item.exists():
            item.chmod(stat.S_IMODE(item.stat().st_mode) | 0o600)
    # This is the staging copy. Rebuild its shared-memory index from its WAL;
    # SQLite never opens the original source backup, even in read-only mode.
    Path(str(path) + "-shm").unlink(missing_ok=True)
    descriptor, name = tempfile.mkstemp(prefix=".application-role-db-", dir=path.parent)
    os.close(descriptor)
    temporary, changed = Path(name), []
    try:
        with closing(sqlite3.connect(path)) as original, closing(sqlite3.connect(temporary)) as prepared:
            original.backup(prepared)
            if prepared.execute("PRAGMA quick_check").fetchall() != [("ok",)]:
                raise PreparationError("a copied configuration database failed its integrity check")
            before = database_rows(prepared)
            role = prepared.execute("SELECT value FROM framework_metadata WHERE key='role'").fetchone()
            if role != (expected_role,):
                raise PreparationError("a database role does not match its service document")
            if not prepared.execute("SELECT 1 FROM framework_documents WHERE name='config'").fetchone():
                raise PreparationError("a database configuration document is missing")
            with prepared:
                if expected_role == "dispatch":
                    prepared.execute("UPDATE framework_metadata SET value='application' WHERE key='role'")
                    changed.append("framework_metadata[role].value")
                topology = prepared.execute("SELECT document FROM framework_documents WHERE name='cache:topology'").fetchone()
                if topology:
                    try:
                        value = decode_json(topology[0])
                    except (TypeError, ValueError):
                        raise PreparationError("a cached topology document is unreadable") from None
                    if not isinstance(value, dict):
                        raise PreparationError("a cached topology document is not an object")
                    if "dispatch_url" in value:
                        if "application_url" in value:
                            raise PreparationError("a cached topology contains both old and current URL fields")
                        value["application_url"] = value.pop("dispatch_url")
                        encoded = encode_json(value)
                        if isinstance(topology[0], bytes):
                            encoded = encoded.encode()
                        prepared.execute("UPDATE framework_documents SET document=? WHERE name='cache:topology'", (encoded,))
                        changed.append("framework_documents[cache:topology].document.dispatch_url -> application_url")
            after = database_rows(prepared)
            for table in before:
                old_rows, new_rows = before[table], after[table]
                if len(old_rows) != len(new_rows):
                    raise PreparationError("database preparation changed an unrelated row")
                for old, new in zip(old_rows, new_rows):
                    if table == "framework_metadata" and old[0] == "role" and expected_role == "dispatch":
                        if new != (old[0], "application"):
                            raise PreparationError("database role preparation did not preserve its row")
                    elif table == "framework_documents" and old[0] == "cache:topology":
                        if old[0] != new[0] or old[2:] != new[2:]:
                            raise PreparationError("topology preparation changed unrelated row fields")
                    elif old != new:
                        raise PreparationError("database preparation changed unrelated data")
        temporary.chmod(mode)
        os.replace(temporary, path)
        for suffix in ("-wal", "-shm", "-journal"):
            Path(str(path) + suffix).unlink(missing_ok=True)
    except sqlite3.Error:
        raise PreparationError("a copied configuration database could not be prepared") from None
    finally:
        temporary.unlink(missing_ok=True)
        for suffix in ("-wal", "-shm", "-journal"):
            Path(str(temporary) + suffix).unlink(missing_ok=True)
    return changed


def publish(stage, output):
    if os.path.lexists(output):
        raise PreparationError("output already exists; choose a new destination")
    library = ctypes.CDLL(None, use_errno=True)
    if sys.platform == "darwin":
        # macOS SDK sys/stdio.h: RENAME_EXCL = 0x00000004.
        rename = library.renamex_np
        rename.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_uint]
        arguments = (os.fsencode(stage), os.fsencode(output), 4)
    elif sys.platform.startswith("linux") and hasattr(library, "renameat2"):
        rename = library.renameat2
        rename.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
        arguments = (-100, os.fsencode(stage), -100, os.fsencode(output), 1)
    else:
        raise PreparationError("atomic exclusive output publication is unavailable on this platform")
    rename.restype = ctypes.c_int
    if rename(*arguments) != 0:
        code = ctypes.get_errno()
        raise OSError(code, os.strerror(code))


def prepare(source, output, deployment_root):
    source, output = Path(source), Path(output)
    if source.is_symlink() or not source.is_dir():
        raise PreparationError("source must be a private stopped-state directory")
    source = source.resolve()
    output = output.absolute()
    if not output.parent.is_dir() or output.parent.is_symlink():
        raise PreparationError("output parent must be an existing directory")
    output = output.parent.resolve() / output.name
    if output == source or source in output.parents or output in source.parents:
        raise PreparationError("source and output must be separate directories")
    if os.path.lexists(output):
        raise PreparationError("output already exists; choose a new destination")
    info = source.stat()
    if info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) & 0o077:
        raise PreparationError("source directory must belong to this account and be owner-only")
    deployment_root = PurePosixPath(deployment_root)
    if not deployment_root.is_absolute() or deployment_root == PurePosixPath("/") or ".." in deployment_root.parts:
        raise PreparationError("deployment root must be the explicit original absolute state root")
    before = inventory(source)
    if RECEIPTS.intersection(before):
        raise PreparationError("source contains a previous preparation receipt")
    services = collect_services(source, deployment_root)
    source_bytes = sum(item.get("size", 0) for item in before.values())
    largest_database = max(sum(before.get(service["database"].as_posix() + suffix, {}).get("size", 0)
                               for suffix in ("", "-wal")) for service in services)
    required = source_bytes + largest_database + len(before) * 600 + 1048576
    if shutil.disk_usage(output.parent).free < required:
        raise PreparationError("output storage has insufficient free space for the copy and SQLite backup")
    locks, stage = [], None
    try:
        for service in services:
            lock_path = source / service["state"] / "service.lock"
            if lock_path.exists():
                lock = lock_path.open("rb")
                locks.append(lock)
                try:
                    fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                except BlockingIOError:
                    raise PreparationError("a source service is still running; use a stopped verified backup") from None
        stage = Path(tempfile.mkdtemp(prefix=".application-role-prepare-", dir=output.parent))
        shutil.copytree(source, stage, dirs_exist_ok=True, copy_function=shutil.copy2)
        stage.chmod(stat.S_IMODE(stage.stat().st_mode) | 0o700)
        for current, directories, _ in os.walk(stage):
            for name in directories:
                item = Path(current) / name
                item.chmod(stat.S_IMODE(item.stat().st_mode) | 0o700)
        changes, unit_plan = [], []
        for service in services:
            relative, value = service["config"], dict(service["value"])
            fields = []
            if value["role"] == "dispatch":
                value["role"] = "application"
                fields.append("role")
                unit_plan.append({"instance_id": value["instance_id"], "old_role": "dispatch", "role": "application",
                                  "config": str(deployment_root / PurePosixPath(moved(relative).as_posix())),
                                  "action": "replace the stopped unit's role argument and configuration path; no unit is changed by this preparation"})
            for field, old in (("state_dir", service["state"]), ("credential_file", service["credentials"])):
                if moved(old) != old:
                    value[field] = str(deployment_root / PurePosixPath(moved(old).as_posix()))
                    fields.append(field)
            for section, names in ((value, ("tls_certificate", "tls_key")),
                                   (value.get("radio"), ("engine", "fne", "bridge")),
                                   (value.get("media"), ("codec_binary", "speech_binary"))):
                if not isinstance(section, dict):
                    continue
                for name in names:
                    if name not in section:
                        continue
                    previous = section[name]
                    section[name] = moved_declared_path(previous, deployment_root)
                    if section[name] != previous:
                        prefix = "radio." if section is value.get("radio") else "media." if section is value.get("media") else ""
                        fields.append(prefix + name)
            if fields:
                write_json(stage / relative, value, before[relative.as_posix()]["mode"])
                changes.append({"file": moved(relative).as_posix(), "fields": fields})
            credential_path = stage / service["credentials"]
            credentials = read_json(credential_path)
            peers = credentials.get("peers")
            if peers is not None and not isinstance(peers, dict):
                raise PreparationError("a credential peer collection is not an object")
            fields = []
            for identity, peer in (peers or {}).items():
                if not isinstance(peer, dict) or peer.get("role") not in ROLES:
                    raise PreparationError("a credential peer has an unknown service role")
                if peer["role"] == "dispatch":
                    peer["role"] = "application"
                    fields.append("peers[" + identity + "].role")
            if fields:
                write_json(credential_path, credentials, before[service["credentials"].as_posix()]["mode"])
                changes.append({"file": moved(service["credentials"]).as_posix(), "fields": fields})
            fields = prepare_database(stage / service["database"], service["role"])
            if fields:
                changes.append({"file": moved(service["database"]).as_posix(), "fields": fields})
        (stage / "dispatch").rename(stage / "application")
        if (stage / "dispatches").exists():
            (stage / "dispatches").rename(stage / "applications")
        if inventory(source) != before:
            raise PreparationError("source changed during preparation; no prepared output was published")
        write_json(stage / "application-role-source-manifest.json", before)
        for relative, item in before.items():
            if item["kind"] == "directory" and relative != ".":
                (stage / moved(Path(relative))).chmod(item["mode"])
        prepared = inventory(stage)
        prepared.pop("application-role-source-manifest.json", None)
        prepared["."]["mode"] = stat.S_IMODE(info.st_mode)
        mutable = {item["file"] for item in changes}
        mutable.update(moved(service["database"]).as_posix() for service in services)
        sidecars = {moved(Path(service["database"].as_posix() + suffix)).as_posix()
                    for service in services for suffix in ("-wal", "-shm", "-journal")}
        file_changes = []
        for relative, item in before.items():
            target = moved(Path(relative)).as_posix()
            actual = prepared.get(target)
            if actual is None:
                if target not in sidecars:
                    raise PreparationError("preparation removed an unrelated source file")
                file_changes.append({"from": relative, "action": "SQLite sidecar consolidated into database"})
                continue
            if actual["mode"] != item["mode"]:
                raise PreparationError("preparation did not preserve a source permission mode")
            if item["kind"] == "file" and item["sha256"] != actual["sha256"]:
                if target not in mutable:
                    raise PreparationError("preparation changed an unrelated source file")
                file_changes.append({"from": relative, "to": target, "action": "updated",
                                     "sha256_before": item["sha256"], "sha256_after": actual["sha256"]})
            elif target != relative:
                file_changes.append({"from": relative, "to": target, "action": "moved; content unchanged"})
        write_json(stage / "application-role-prepared-manifest.json", prepared)
        receipt = {"source": str(source), "output": str(output), "deployment_root": str(deployment_root),
                   "source_unchanged": True, "prepared_only": True, "services": len(services),
                   "directory_moves": [{"from": "dispatch", "to": "application"}] +
                                      ([{"from": "dispatches", "to": "applications"}] if (source / "dispatches").exists() else []),
                   "changed_files": changes, "file_changes": file_changes, "unit_plan": unit_plan,
                   "preserved": ["instance IDs", "credential identity strings and tokens", "keys and TLS material", "all other settings and database rows"],
                   "sqlite": "consistent backup from the copied database and WAL; original backup is never opened by SQLite"}
        write_json(stage / "application-role-preparation.json", receipt)
        stage.chmod(stat.S_IMODE(info.st_mode))
        publish(stage, output)
        stage = None
        return receipt
    finally:
        if stage is not None:
            stage.chmod(0o700)
            for current, directories, _ in os.walk(stage):
                for name in directories:
                    item = Path(current) / name
                    item.chmod(stat.S_IMODE(item.stat().st_mode) | 0o700)
            shutil.rmtree(stage)
        for lock in locks:
            lock.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True, help="private verified stopped-state backup")
    parser.add_argument("--output", required=True, help="fresh prepared-copy destination")
    parser.add_argument("--deployment-root", required=True, help="original deployed absolute state root, not the local backup path")
    options = parser.parse_args()
    try:
        receipt = prepare(options.source, options.output, options.deployment_root)
    except (PreparationError, OSError) as error:
        if not isinstance(error, PreparationError):
            error = PreparationError("preparation could not finish; the source was not modified and no replacement was installed")
        parser.exit(1, "Preparation refused: " + str(error) + "\n")
    print("Prepared separate state copy for " + str(receipt["services"]) + " services.")
    print("Source unchanged; no services or runtime files were installed or controlled.")
    print("Review application-role-preparation.json and both manifests in the output directory.")


if __name__ == "__main__":
    main()
