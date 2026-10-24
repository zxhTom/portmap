from pathlib import Path
import re
import unittest


ROOT = Path(__file__).parent


def read(name):
    path = ROOT / name
    return path.read_text() if path.exists() else ''


def domains(text):
    return {name for line in re.findall(r'^\s*server_name\s+([^;]+);', text, re.M) for name in line.split()}


class NginxRolesTest(unittest.TestCase):
    def test_server_domain_does_not_capture_client(self):
        names = domains(read('portmap-server.nginx.conf'))
        self.assertIn('server.port.zxhtom.store', names)
        self.assertNotIn('portmap.port.zxhtom.store', names)

    def test_client_entry_reaches_relay_instead_of_server_console(self):
        config = read('portmap-client-1822.nginx.conf')
        self.assertEqual(domains(config), {'portmap.port.zxhtom.store'})
        self.assertIn('proxy_pass http://127.0.0.1:9881;', config)
        self.assertNotIn('127.0.0.1:9890', config)
        self.assertIn('/live/portmap-client-1822/fullchain.pem', config)

    def test_namespaces_are_disjoint(self):
        self.assertFalse(domains(read('portmap-server.nginx.conf')) & domains(read('portmap-client-1822.nginx.conf')))


if __name__ == '__main__':
    unittest.main()
