import fcntl
import importlib.util
import json
import os
from pathlib import Path
import shutil
import sqlite3
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


SCRIPT = Path(__file__).with_name("prepare-application-role.py")
SPEC = importlib.util.spec_from_file_location("prepare_application_role", SCRIPT)
PREPARE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PREPARE)
DEPLOYMENT_ROOT = "/home/example/.local/state/trunksystem-framework"


def private_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.write_text(json.dumps(value, ensure_ascii=False) + "\n")
    path.chmod(0o600)


def make_state(root, extra_application=True):
    root.mkdir(mode=0o700)
    services = [("system", "system", "system-1"), ("zone", "zone", "zone-1"),
                ("site", "site", "site-1"), ("dispatch", "dispatch", "dispatch-1"),
                ("sites/site-2", "site", "site-2")]
    if extra_application:
        services.append(("dispatches/dispatch-2", "dispatch", "dispatch-2"))
    for directory, role, identity in services:
        folder = root / directory
        data = folder / "data"
        data.mkdir(parents=True, mode=0o700)
        config = {"role": role, "instance_id": identity,
                  "state_dir": DEPLOYMENT_ROOT + "/" + directory + "/data",
                  "credential_file": DEPLOYMENT_ROOT + "/" + directory + "/service-credentials.json",
                  "tls_certificate": DEPLOYMENT_ROOT + "/tls/service.crt",
                  "tls_key": DEPLOYMENT_ROOT + "/tls/service.key",
                  "public_url": "https://example.invalid/", "listen": "0.0.0.0:5003",
                  "radio": {"allow_rf": False, "unchanged": "literal value"},
                  "media": {"codec_binary": "/opt/current/native/dvmbridge"},
                  "unknown_future_setting": {"entered": [0, False, None, "café"]}}
        private_json(folder / "service.json", config)
        credentials = {"outbound_id": role + ":" + identity, "outbound_token": "test-private-token-" + identity,
                       "peers": {"dispatch:dispatch-1": {"role": "dispatch", "instance_id": "dispatch-1", "token": "test-private-peer-token"},
                                 "site:site-1": {"role": "site", "instance_id": "site-1", "token": "site-private-token"}},
                       "crypto_recipients": {"dispatch-1": "unchanged-public-key"},
                       "crypto_system_public_key": "unchanged-signing-pin"}
        private_json(folder / "service-credentials.json", credentials)
        (data / "service.lock").write_bytes(b"unchanged lock contents")
        (data / "service.lock").chmod(0o600)
        (data / "endpoint.key").write_bytes(b"test-private-identity-material")
        (data / "endpoint.key").chmod(0o600)
        with sqlite3.connect(data / "configuration.db") as connection:
            connection.executescript("""
CREATE TABLE framework_metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE framework_documents(name TEXT PRIMARY KEY,document BLOB NOT NULL,at TEXT NOT NULL);
CREATE TABLE framework_administrator(singleton INTEGER PRIMARY KEY,username TEXT NOT NULL,password_hash BLOB NOT NULL);
CREATE TABLE framework_events(id INTEGER PRIMARY KEY AUTOINCREMENT,at TEXT NOT NULL,kind TEXT NOT NULL,message TEXT NOT NULL);
""")
            connection.execute("INSERT INTO framework_metadata VALUES('role',?)", (role,))
            connection.execute("INSERT INTO framework_metadata VALUES('untouched','literal metadata')")
            for name, value in [("config", {"unrestricted_setting": "entered value", "origin": "dispatch-1"}),
                                ("users", {"password_hash": "test-password-verifier", "profile": {"selected": 1001}}),
                                ("cache:topology", {"system_url": "https://system.invalid", "dispatch_url": "https://application.invalid", "future": [False, 42]}),
                                ("incidents", {"origin": "dispatch-1", "cleared": False})]:
                connection.execute("INSERT INTO framework_documents VALUES(?,?,?)", (name, json.dumps(value).encode(), "original observation time"))
            connection.execute("INSERT INTO framework_administrator VALUES(1,'owner',?)", (b"test-password-verifier",))
            connection.execute("INSERT INTO framework_events(at,kind,message) VALUES('original time','rf','dispatch-1 original event')")
        connection.close()
        (data / "configuration.db").chmod(0o600)
    tls = root / "tls"
    tls.mkdir(mode=0o700)
    (tls / "service.key").write_bytes(b"test-private-tls-key")
    (tls / "service.key").chmod(0o600)
    (tls / "service.crt").write_bytes(b"test-tls-certificate")
    (tls / "service.crt").chmod(0o644)
    for current, directories, _ in os.walk(root):
        Path(current).chmod(0o700)
        for directory in directories:
            (Path(current) / directory).chmod(0o700)
    return services


class PrepareApplicationRoleTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="application-role-prepare-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source, self.output = self.root / "source", self.root / "prepared"
        self.services = make_state(self.source)

    def assert_refused_preserving_source(self, operation):
        before = PREPARE.inventory(self.source)
        with self.assertRaises((PREPARE.PreparationError, OSError)):
            operation()
        self.assertEqual(before, PREPARE.inventory(self.source))
        self.assertFalse(self.output.exists())
        self.assertEqual([], list(self.root.glob(".application-role-prepare-*")))

    def test_prepares_only_exact_roles_paths_and_topology_with_identity_and_modes_preserved(self):
        original_config = json.loads((self.source / "dispatch/service.json").read_bytes())
        (self.source / "dispatch/service.json").chmod(0o400)
        before = PREPARE.inventory(self.source)
        receipt = PREPARE.prepare(self.source, self.output, DEPLOYMENT_ROOT)
        self.assertEqual(before, PREPARE.inventory(self.source))
        self.assertTrue(receipt["source_unchanged"])
        self.assertTrue(receipt["prepared_only"])
        self.assertEqual(6, receipt["services"])
        self.assertFalse((self.output / "dispatch").exists())
        self.assertFalse((self.output / "dispatches").exists())
        prepared_config = json.loads((self.output / "application/service.json").read_bytes())
        expected = dict(original_config, role="application",
                        state_dir=DEPLOYMENT_ROOT + "/application/data",
                        credential_file=DEPLOYMENT_ROOT + "/application/service-credentials.json")
        self.assertEqual(expected, prepared_config)
        self.assertEqual(0o400, stat.S_IMODE((self.output / "application/service.json").stat().st_mode))
        for directory, role, identity in self.services:
            new = PREPARE.moved(Path(directory))
            config = json.loads((self.output / new / "service.json").read_bytes())
            self.assertEqual(identity, config["instance_id"])
            old_credentials = json.loads((self.source / directory / "service-credentials.json").read_bytes())
            credentials = json.loads((self.output / new / "service-credentials.json").read_bytes())
            old_credentials["peers"]["dispatch:dispatch-1"]["role"] = "application"
            self.assertEqual(old_credentials, credentials)
            self.assertEqual((self.source / directory / "data/endpoint.key").read_bytes(), (self.output / new / "data/endpoint.key").read_bytes())
            with sqlite3.connect(self.output / new / "data/configuration.db") as connection:
                self.assertEqual(("application" if role == "dispatch" else role,), connection.execute("SELECT value FROM framework_metadata WHERE key='role'").fetchone())
                topology = json.loads(connection.execute("SELECT document FROM framework_documents WHERE name='cache:topology'").fetchone()[0])
                self.assertEqual({"system_url": "https://system.invalid", "application_url": "https://application.invalid", "future": [False, 42]}, topology)
                self.assertEqual((b"test-password-verifier",), connection.execute("SELECT password_hash FROM framework_administrator").fetchone())
                self.assertEqual(("dispatch-1 original event",), connection.execute("SELECT message FROM framework_events").fetchone())
            connection.close()
        self.assertEqual(before, json.loads((self.output / "application-role-source-manifest.json").read_bytes()))
        prepared = PREPARE.inventory(self.output)
        for name in PREPARE.RECEIPTS:
            prepared.pop(name)
        self.assertEqual(prepared, json.loads((self.output / "application-role-prepared-manifest.json").read_bytes()))
        self.assertEqual((self.source / "tls/service.key").read_bytes(), (self.output / "tls/service.key").read_bytes())

    def test_committed_wal_is_backed_up_without_sqlite_touching_the_source(self):
        database = self.source / "dispatch/data/configuration.db"
        connection = sqlite3.connect(database)
        connection.execute("PRAGMA journal_mode=WAL")
        connection.execute("PRAGMA wal_autocheckpoint=0")
        connection.execute("INSERT INTO framework_events(at,kind,message) VALUES('late time','rf','committed only in copied WAL')")
        connection.commit()
        self.assertGreater(Path(str(database) + "-wal").stat().st_size, 0)
        backup = self.root / "wal-backup"
        shutil.copytree(self.source, backup)
        connection.close()
        self.source = backup
        before = PREPARE.inventory(self.source)
        PREPARE.prepare(self.source, self.output, DEPLOYMENT_ROOT)
        self.assertEqual(before, PREPARE.inventory(self.source))
        target = self.output / "application/data/configuration.db"
        self.assertFalse(Path(str(target) + "-wal").exists())
        self.assertFalse(Path(str(target) + "-shm").exists())
        with sqlite3.connect(target) as prepared:
            self.assertEqual(("committed only in copied WAL",), prepared.execute("SELECT message FROM framework_events ORDER BY id DESC LIMIT 1").fetchone())
        prepared.close()

    def test_unrelated_decimal_precision_and_json_types_are_preserved(self):
        config = self.source / "dispatch/service.json"
        raw = config.read_text().replace('"unknown_future_setting":', '"exact_decimal": 1.234567890123456789, "unknown_future_setting":')
        config.write_text(raw)
        topology = b'{"dispatch_url":"https://application.invalid","exact_decimal":0.0000000000000001234567890123456789}'
        with sqlite3.connect(self.source / "zone/data/configuration.db") as connection:
            connection.execute("UPDATE framework_documents SET document=? WHERE name='cache:topology'", (topology,))
        connection.close()
        PREPARE.prepare(self.source, self.output, DEPLOYMENT_ROOT)
        self.assertIn('"exact_decimal": 1.234567890123456789', (self.output / "application/service.json").read_text())
        with sqlite3.connect(self.output / "zone/data/configuration.db") as connection:
            copied = connection.execute("SELECT document FROM framework_documents WHERE name='cache:topology'").fetchone()[0]
        connection.close()
        self.assertIn(b'"exact_decimal":0.0000000000000001234567890123456789', copied)

    def test_declared_ancillary_paths_follow_moved_files_without_rewriting_outside_paths(self):
        files = {"dispatch/tls/service.key": b"test-private-inline-key",
                 "dispatch/tls/service.crt": b"test-inline-certificate",
                 "dispatch/runtime/dvmbridge": b"test-codec-binary",
                 "dispatch/runtime/espeak-ng": b"test-speech-binary",
                 "dispatch/runtime/dvmhost": b"test-engine-binary",
                 "dispatch/runtime/dvmfne": b"test-network-binary",
                 "dispatches/dispatch-2/runtime/bridge": b"test-other-bridge"}
        for relative, contents in files.items():
            path = self.source / relative
            path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            path.write_bytes(contents)
            path.chmod(0o600)
        primary = self.source / "dispatch/service.json"
        config = json.loads(primary.read_bytes())
        config["tls_key"] = DEPLOYMENT_ROOT + "/dispatch/tls/service.key"
        config["tls_certificate"] = DEPLOYMENT_ROOT + "/dispatch/tls/service.crt"
        config["media"] = {"codec_binary": DEPLOYMENT_ROOT + "/dispatch/runtime/dvmbridge",
                           "speech_binary": DEPLOYMENT_ROOT + "/dispatch/runtime/espeak-ng"}
        private_json(primary, config)
        site_path = self.source / "site/service.json"
        site = json.loads(site_path.read_bytes())
        site["radio"] = {"engine": DEPLOYMENT_ROOT + "/dispatch/runtime/dvmhost",
                         "fne": DEPLOYMENT_ROOT + "/dispatch/runtime/dvmfne",
                         "bridge": DEPLOYMENT_ROOT + "/dispatches/dispatch-2/runtime/bridge"}
        private_json(site_path, site)
        receipt = PREPARE.prepare(self.source, self.output, DEPLOYMENT_ROOT)
        updated = json.loads((self.output / "application/service.json").read_bytes())
        self.assertEqual(DEPLOYMENT_ROOT + "/application/tls/service.key", updated["tls_key"])
        self.assertEqual(DEPLOYMENT_ROOT + "/application/tls/service.crt", updated["tls_certificate"])
        self.assertEqual(DEPLOYMENT_ROOT + "/application/runtime/dvmbridge", updated["media"]["codec_binary"])
        self.assertEqual(DEPLOYMENT_ROOT + "/application/runtime/espeak-ng", updated["media"]["speech_binary"])
        updated_site = json.loads((self.output / "site/service.json").read_bytes())
        self.assertEqual(DEPLOYMENT_ROOT + "/application/runtime/dvmhost", updated_site["radio"]["engine"])
        self.assertEqual(DEPLOYMENT_ROOT + "/application/runtime/dvmfne", updated_site["radio"]["fne"])
        self.assertEqual(DEPLOYMENT_ROOT + "/applications/dispatch-2/runtime/bridge", updated_site["radio"]["bridge"])
        self.assertEqual(site["tls_key"], updated_site["tls_key"])
        for relative, contents in files.items():
            self.assertEqual(contents, (self.output / PREPARE.moved(Path(relative))).read_bytes())
        changes = {change["file"]: change["fields"] for change in receipt["changed_files"]}
        self.assertIn("tls_key", changes["application/service.json"])
        self.assertIn("media.speech_binary", changes["application/service.json"])
        self.assertIn("radio.bridge", changes["site/service.json"])

    def test_unknown_role_refuses_without_mutating_any_source_byte(self):
        path = self.source / "dispatch/service.json"
        value = json.loads(path.read_bytes())
        value["role"] = "unknown"
        private_json(path, value)
        self.assert_refused_preserving_source(lambda: PREPARE.prepare(self.source, self.output, DEPLOYMENT_ROOT))

    def test_bad_owned_path_refuses_without_mutating_any_source_byte(self):
        path = self.source / "dispatch/service.json"
        value = json.loads(path.read_bytes())
        value["state_dir"] = DEPLOYMENT_ROOT + "/system/data"
        private_json(path, value)
        self.assert_refused_preserving_source(lambda: PREPARE.prepare(self.source, self.output, DEPLOYMENT_ROOT))

    def test_schema_error_rolls_back_the_entire_staged_copy(self):
        with sqlite3.connect(self.source / "zone/data/configuration.db") as connection:
            connection.execute("DROP TABLE framework_events")
        connection.close()
        self.assert_refused_preserving_source(lambda: PREPARE.prepare(self.source, self.output, DEPLOYMENT_ROOT))

    def test_sqlite_internal_statistics_are_preserved(self):
        with sqlite3.connect(self.source / "zone/data/configuration.db") as connection:
            connection.execute("ANALYZE")
            before = connection.execute("SELECT * FROM sqlite_stat1 ORDER BY rowid").fetchall()
        connection.close()
        PREPARE.prepare(self.source, self.output, DEPLOYMENT_ROOT)
        with sqlite3.connect(self.output / "zone/data/configuration.db") as connection:
            self.assertEqual(before, connection.execute("SELECT * FROM sqlite_stat1 ORDER BY rowid").fetchall())
        connection.close()

    def test_database_role_mismatch_is_not_automatically_repaired(self):
        with sqlite3.connect(self.source / "dispatch/data/configuration.db") as connection:
            connection.execute("UPDATE framework_metadata SET value='site' WHERE key='role'")
        connection.close()
        self.assert_refused_preserving_source(lambda: PREPARE.prepare(self.source, self.output, DEPLOYMENT_ROOT))

    def test_active_source_lock_refuses_before_copying(self):
        with (self.source / "dispatch/data/service.lock").open("rb") as active:
            fcntl.flock(active.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            self.assert_refused_preserving_source(lambda: PREPARE.prepare(self.source, self.output, DEPLOYMENT_ROOT))

    def test_publish_failure_cleans_staging_and_preserves_original(self):
        with mock.patch.object(PREPARE, "publish", side_effect=OSError("injected publication failure")):
            self.assert_refused_preserving_source(lambda: PREPARE.prepare(self.source, self.output, DEPLOYMENT_ROOT))

    def test_read_only_source_modes_are_retained_in_the_prepared_copy(self):
        (self.source / "site/data/configuration.db").chmod(0o400)
        (self.source / "dispatch/data").chmod(0o500)
        self.source.chmod(0o500)
        before = PREPARE.inventory(self.source)
        PREPARE.prepare(self.source, self.output, DEPLOYMENT_ROOT)
        self.assertEqual(before, PREPARE.inventory(self.source))
        self.assertEqual(0o500, stat.S_IMODE(self.output.stat().st_mode))
        self.assertEqual(0o500, stat.S_IMODE((self.output / "application/data").stat().st_mode))
        self.assertEqual(0o400, stat.S_IMODE((self.output / "site/data/configuration.db").stat().st_mode))
        actual = PREPARE.inventory(self.output)
        for name in PREPARE.RECEIPTS:
            actual.pop(name)
        self.assertEqual(actual, json.loads((self.output / "application-role-prepared-manifest.json").read_bytes()))

    def test_existing_output_is_never_replaced(self):
        self.output.mkdir(mode=0o700)
        marker = self.output / "keep-original"
        marker.write_bytes(b"untouched destination")
        before = PREPARE.inventory(self.source)
        with self.assertRaises(PREPARE.PreparationError):
            PREPARE.prepare(self.source, self.output, DEPLOYMENT_ROOT)
        self.assertEqual(b"untouched destination", marker.read_bytes())
        self.assertEqual(before, PREPARE.inventory(self.source))

    def test_output_created_during_publication_is_not_replaced_even_when_empty(self):
        before = PREPARE.inventory(self.source)
        real_lexists = os.path.lexists
        created = []
        normalized_output = self.output.parent.resolve() / self.output.name

        def racing_lexists(path):
            if Path(path) == normalized_output and list(self.root.glob(".application-role-prepare-*")):
                self.output.mkdir(mode=0o700)
                created.append(self.output.stat().st_ino)
                return False
            return real_lexists(path)

        with mock.patch.object(PREPARE.os.path, "lexists", side_effect=racing_lexists):
            with self.assertRaises(OSError):
                PREPARE.prepare(self.source, self.output, DEPLOYMENT_ROOT)
        self.assertEqual([self.output.stat().st_ino], created)
        self.assertEqual([], list(self.output.iterdir()))
        self.assertEqual([], list(self.root.glob(".application-role-prepare-*")))
        self.assertEqual(before, PREPARE.inventory(self.source))

    def test_symlink_source_is_refused(self):
        (self.source / "dispatch/data/linked-key").symlink_to(self.root / "outside-key")
        with self.assertRaises(PREPARE.PreparationError):
            PREPARE.prepare(self.source, self.output, DEPLOYMENT_ROOT)
        self.assertFalse(self.output.exists())
        self.assertTrue((self.source / "dispatch/data/linked-key").is_symlink())

    def test_cli_requires_deployment_root_and_never_prints_private_content(self):
        missing = subprocess.run([sys.executable, str(SCRIPT), "--source", str(self.source), "--output", str(self.output)], capture_output=True, text=True)
        self.assertEqual(2, missing.returncode)
        result = subprocess.run([sys.executable, str(SCRIPT), "--source", str(self.source), "--output", str(self.output), "--deployment-root", DEPLOYMENT_ROOT], capture_output=True, text=True)
        self.assertEqual(0, result.returncode, result.stderr)
        for secret in ("test-private", "test-password-verifier", "site-private-token"):
            self.assertNotIn(secret, result.stdout + result.stderr)
        self.assertTrue((self.output / "application-role-preparation.json").is_file())


if __name__ == "__main__":
    unittest.main()
