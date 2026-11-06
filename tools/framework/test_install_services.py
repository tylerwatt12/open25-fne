"""No production systemctl: exercise current unit names and explicit config paths."""
from pathlib import Path
import os
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]

class InstallTests(unittest.TestCase):
    def setUp(self):
        self.scratch = tempfile.TemporaryDirectory()
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name)
        self.package = self.root / "Open25 package"
        self.script = self.package / "tools/framework/install-services.sh"
        self.script.parent.mkdir(parents=True)
        shutil.copy2(ROOT / "tools/framework/install-services.sh", self.script)
        shutil.copytree(ROOT / "deploy", self.package / "deploy")
        self.fakebin = self.root / "bin"
        self.fakebin.mkdir()
        self.log = self.root / "systemctl.log"
        fake = self.fakebin / "systemctl"
        fake.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$OPEN25_INSTALL_LOG"\n')
        fake.chmod(0o700)
        self.units = self.root / "xdg/systemd/user"
        self.env = dict(os.environ, PATH=str(self.fakebin) + os.pathsep + os.environ['PATH'], XDG_CONFIG_HOME=str(self.root / "xdg"), OPEN25_INSTALL_LOG=str(self.log))

    def invoke(self, *args):
        subprocess.run(['sh',str(self.script),*args], env=self.env,check=True,capture_output=True,text=True)

    def test_fresh_install_enables_current_four_roles_without_start(self):
        self.invoke()
        units = sorted(p.name for p in self.units.glob('*.service'))
        self.assertEqual(units, ['open25-fne-application.service','open25-fne-site.service','open25-fne-system.service','open25-fne-zone.service'])
        self.assertNotIn(' start ',self.log.read_text())
        for unit in self.units.glob('*.service'):
            self.assertIn('/open25-fne/open25-fne ',unit.read_text())
            self.assertNotIn('trunksystem-framework',unit.read_text())

    def test_second_site_keeps_explicit_existing_configuration(self):
        config='/home/example/.local/state/trunksystem-framework/sites/site-2/service.json'
        self.invoke('--role','site','--unit','open25-fne-site-2.service','--config',config)
        text=(self.units/'open25-fne-site-2.service').read_text()
        self.assertIn('"'+config+'"',text)
        self.assertIn(str(self.package)+'/open25-fne',text)
        self.assertNotIn(' start ',self.log.read_text())

    def test_explicit_start_starts_only_selected_instance(self):
        self.invoke('--role','site','--unit','open25-fne-site-2.service','--config','/existing/site-2.json','--start')
        self.assertEqual([x for x in self.log.read_text().splitlines() if ' start ' in x],['--user start open25-fne-site-2.service'])

if __name__=='__main__':
    unittest.main()
