import copy
import http.client
import json
from pathlib import Path
import sys
import tempfile
import threading
import unittest
from unittest.mock import patch

ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT/'manager'))
import server
from configuration import Configuration,ConfigError,atomic,is_lan_address

class HomeLanTest(unittest.TestCase):
 def setUp(self):
  self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
  self.store=Configuration(Path(self.temp.name)/'state','home',repository=ROOT)
 def make_server(self):
  management=self.store.blank()['management']
  # A literal interface IP must work without being in this explicit DNS list.
  management['allowedHosts']=['configured-only.example']
  app=server.Server(self.store,management,address=('0.0.0.0',0))
  self.addCleanup(app.server_close)
  return app
 def test_default_and_first_setup_support_lan_without_media_dependencies(self):
  management=self.store.blank()['management']
  self.assertEqual((management['listen'],management['port']),('0.0.0.0',9080))
  self.store.validate_management(management)
  with patch.object(sys,'argv',['server.py','--config-dir',str(self.store.directory)]),patch.object(server,'Server') as factory:
   factory.return_value.scheme='http';factory.return_value.serve_forever.side_effect=KeyboardInterrupt
   server.main()
   self.assertEqual(factory.call_args.args[1]['listen'],'0.0.0.0')
   factory.return_value.server_close.assert_called_once()
 def test_only_real_destination_ip_and_lan_peers_are_accepted(self):
  app=self.make_server();port=app.server_address[1]
  origin=f'http://192.168.50.10:{port}'
  self.assertIn(origin,app.request_origins('192.168.50.20','192.168.50.10',origin))
  for authority in [f'http://192.168.50.99:{port}',f'http://rebinding.example:{port}',f'http://192.168.50.10:{port+1}',f'http://user@192.168.50.10:{port}']:
   with self.subTest(authority=authority):self.assertNotIn(authority,app.request_origins('192.168.50.20','192.168.50.10',authority))
  for peer,local in [('8.8.8.8','192.168.50.10'),('192.168.50.20','8.8.8.8'),('198.51.100.20','192.168.50.10')]:
   with self.subTest(peer=peer,local=local),self.assertRaises(ConfigError):app.request_origins(peer,local,origin)
  for address in ['127.0.0.1','192.168.1.2','10.0.0.2','172.16.0.2','fd12::1','::ffff:192.168.1.2']:
   self.assertTrue(is_lan_address(address),address)
  for address in ['8.8.8.8','172.32.0.1','198.51.100.2','rebinding.example']:
   self.assertFalse(is_lan_address(address),address)
 def test_ip_login_keeps_authentication_csrf_and_exact_origin_checks(self):
  app=self.make_server();thread=threading.Thread(target=app.serve_forever,daemon=True);thread.start();self.addCleanup(app.shutdown)
  port=app.server_address[1];origin=f'http://127.0.0.1:{port}'
  def request(path,body=None,cookie=None,csrf=None,host=None,request_origin=origin):
   connection=http.client.HTTPConnection('127.0.0.1',port,timeout=3)
   headers={'Host':host or f'127.0.0.1:{port}','Origin':request_origin}
   if cookie:headers['Cookie']=cookie
   if csrf:headers['X-CSRF-Token']=csrf
   if body is not None:headers['Content-Type']='application/json'
   connection.request('GET' if body is None else 'POST',path,None if body is None else json.dumps(body),headers)
   response=connection.getresponse();data=response.read();code=response.status;cookie=response.getheader('Set-Cookie');connection.close();return code,data,cookie
  self.assertEqual(request('/')[0],200)
  self.assertEqual(request('/api/config')[0],401)
  self.assertEqual(request('/',host=f'192.168.50.99:{port}')[0],403)
  self.assertEqual(request('/',host=f'rebinding.example:{port}')[0],403)
  token=(self.store.directory/'bootstrap-token').read_text().strip()
  code,raw,cookie=request('/api/login',{'password':token});self.assertEqual(code,200)
  csrf=json.loads(raw)['csrfToken'];cookie=cookie.split(';')[0]
  self.assertEqual(request('/api/config',cookie=cookie)[0],200)
  self.assertEqual(request('/api/logout',{},cookie=cookie)[0],403)
  self.assertEqual(request('/api/logout',{},cookie=cookie,csrf=csrf,request_origin=f'http://configured-only.example:{port}')[0],403)
  self.assertEqual(request('/api/logout',{},cookie=cookie,csrf=csrf)[0],200)
  self.assertEqual(request('/api/config',cookie=cookie)[0],401)
 def test_existing_loopback_configuration_requires_explicit_revisioned_migration(self):
  cfg=self.store.blank();cfg['management']['listen']='127.0.0.1'
  tls=self.store.directory/'tls';tls.mkdir();(tls/'ca.crt').write_text('fixture')
  cfg['frp']['caFile']='tls/ca.crt';cfg['services']=[{'serviceId':'jellyfin','relayPort':18096,'targetHost':'192.168.50.10','targetPort':8096}]
  atomic(self.store.directory/'grants.json',{'homeId':cfg['homeId'],'relayHost':cfg['proxy']['host'],'relayPort':443,'services':[['jellyfin',18096]]})
  initial=self.store.save(cfg,0,{'homeUUID':'11111111-1111-4111-8111-111111111111','frpToken':'test-token-'*4})
  self.assertEqual(self.store.load()['management']['listen'],'127.0.0.1')
  migrated=self.store.enable_lan()
  self.assertEqual(migrated['revision'],initial['revision']+1)
  self.assertEqual(migrated['management']['listen'],'0.0.0.0')
  self.assertEqual(migrated['proxy'],initial['proxy'])
  self.assertEqual(migrated['services'],initial['services'])
  self.assertFalse((self.store.directory/'status.json').exists())
 def test_recovery_does_not_publish_unrecognized_configuration(self):
  self.store.path.write_text('{broken')
  with patch.object(sys,'argv',['server.py','--config-dir',str(self.store.directory)]),patch.object(server,'Server') as factory:
   factory.return_value.scheme='http';factory.return_value.serve_forever.side_effect=KeyboardInterrupt
   server.main();self.assertEqual(factory.call_args.args[1]['listen'],'127.0.0.1')
  self.assertEqual(self.store.path.read_text(),'{broken')

if __name__=='__main__':unittest.main()
