import importlib.util
import json
from pathlib import Path
import sqlite3
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("prepare_encryption", Path(__file__).with_name("prepare-encryption-config.py"))
tool = importlib.util.module_from_spec(spec)
spec.loader.exec_module(tool)


class EncryptionPreparationTest(unittest.TestCase):
    def test_current_fields_and_unrelated_configuration_survive(self):
        with tempfile.TemporaryDirectory() as folder:
            source, destination = Path(folder)/"source.db", Path(folder)/"prepared.db"
            original = {"id":"system-1", "talkgroups":[
                {"tgid":1,"secure_mode":"clear","algorithm_id":132,"key_id":7},
                {"tgid":2,"secure_mode":"secure","site_ids":["site-1"]},
                {"tgid":3,"secure_mode":"selectable"},
                {"tgid":4,"admission_mode":"future","transmit_mode":"future"},
                {"tgid":5}], "modems":[{"device":"/dev/trunksystem-control"}]}
            raw=json.dumps(original).encode()
            with sqlite3.connect(source) as db:
                db.execute("CREATE TABLE framework_documents(name TEXT PRIMARY KEY,document BLOB,at TEXT)")
                db.execute("INSERT INTO framework_documents VALUES('config',?,'unchanged-time')",(raw,))
                db.execute("INSERT INTO framework_documents VALUES('cache:system',?,'cached-time')",(raw,))
                db.execute("INSERT INTO framework_documents VALUES('cache:users',?,'user-time')",(b'[{"talkgroups":[1,2],"profile":{"desktop":{"theme":"old"}}}]',))
            before=source.read_bytes()
            receipt=tool.prepare(source,destination)
            self.assertEqual(source.read_bytes(),before)
            self.assertEqual(receipt["changed_documents"],2)
            self.assertEqual(receipt["changed_talkgroup_records"],8)
            with sqlite3.connect(destination) as db:
                saved=json.loads(db.execute("SELECT document FROM framework_documents WHERE name='config'").fetchone()[0])
                self.assertEqual(db.execute("SELECT at FROM framework_documents WHERE name='config'").fetchone()[0],"unchanged-time")
                self.assertEqual(json.loads(db.execute("SELECT document FROM framework_documents WHERE name='cache:users'").fetchone()[0]),[{"talkgroups":[1,2],"profile":{"desktop":{"theme":"old"}}}])
            groups=saved["talkgroups"]
            self.assertEqual([(g["admission_mode"],g["transmit_mode"]) for g in groups],[("clear","clear"),("secure","secure"),("both","selectable"),("future","future"),("clear","clear")])
            self.assertEqual(groups[0]["algorithm_id"],132)
            self.assertEqual(groups[0]["key_id"],7)
            self.assertEqual(saved["modems"],original["modems"])
            self.assertTrue(all("secure_mode" not in group for group in groups))
            with self.assertRaises(ValueError):tool.prepare(source,destination)

    def test_unknown_programmed_value_is_preserved(self):
        value={"talkgroups":[{"tgid":8,"secure_mode":"vendor-future"}]}
        self.assertEqual(tool.prepare_document(value),1)
        self.assertEqual(value["talkgroups"][0],{"tgid":8,"admission_mode":"vendor-future","transmit_mode":"vendor-future"})


if __name__ == "__main__":
    unittest.main()
