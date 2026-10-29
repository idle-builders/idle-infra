#!/usr/bin/env python3
import json, os, pathlib, subprocess, tempfile, unittest
ROOT = pathlib.Path(__file__).resolve().parents[1]

class Selection(unittest.TestCase):
    def test_selection_and_credential_cleanup(self):
        with tempfile.TemporaryDirectory() as d:
            base = pathlib.Path(d).resolve()
            instances = []
            for name in ('first', 'second'):
                p = base / name
                (p / 'terraform/main').mkdir(parents=True)
                (p / '.infra-instance.json').write_text(json.dumps({'terraform_root':'terraform/main', 'credentials_script':'loader.sh'}))
                (p / 'loader.sh').write_text('export TEST_INSTANCE='+name+'\ntrap \'rm -f "$INFRA_REPO/temporary-kubeconfig"\' EXIT\ntouch "$INFRA_REPO/temporary-kubeconfig"\n')
                instances.append(p)
            config = base / 'config/idle-infra'
            config.mkdir(parents=True)
            (config / 'default-instance').write_text(str(instances[0]))
            env = dict(os.environ, XDG_CONFIG_HOME=str(base/'config'), CLAUDE_PLUGIN_ROOT=str(ROOT))
            env.pop('INFRA_REPO',None)
            def resolve(cwd, extra=None):
                return subprocess.run([str(ROOT/'scripts/infra-root.sh')],cwd=cwd,env=env| (extra or {}),capture_output=True,text=True)
            self.assertEqual(resolve(base).stdout.strip(),str(instances[0]))
            self.assertEqual(resolve(instances[1]).stdout.strip(),str(instances[1]))
            self.assertEqual(resolve(instances[1],{'INFRA_REPO':str(instances[0])}).stdout.strip(),str(instances[0]))
            self.assertNotEqual(resolve(base,{'INFRA_REPO':str(ROOT)}).returncode,0)
            self.assertNotEqual(resolve(base,{'INFRA_REPO':str(base/'missing')}).returncode,0)
            result = subprocess.run([str(ROOT/'scripts/with-infra.sh'),'bash','-c','test "$TEST_INSTANCE" = second && test -f "$INFRA_REPO/temporary-kubeconfig"'],env=env|{'INFRA_REPO':str(instances[1])},capture_output=True,text=True)
            self.assertEqual(result.returncode,0,result.stderr)
            self.assertFalse((instances[1]/'temporary-kubeconfig').exists())
            (config / 'default-instance').unlink()
            self.assertNotEqual(resolve(ROOT).returncode,0)

if __name__ == '__main__': unittest.main()
