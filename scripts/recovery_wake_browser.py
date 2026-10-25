from pathlib import Path
from urllib.parse import urlsplit
import json
from playwright.sync_api import sync_playwright


def main():
    static = Path(__file__).resolve().parents[1] / 'internal/serverui/static'
    names = ['invalid-offline','stopped-offline','client-offline','old-client','disabled','expired','online']
    records = [dict(owner=f'{i:064x}',name=name,group='test',type='ssh',client_name='test-client',listen_host='127.0.0.1',listen_port=30000+i,target='127.0.0.1:8080',last_seen='2026-10-25T00:00:00Z',desired_running=name!='stopped-offline',invalid=name=='invalid-offline',disabled=name=='disabled',expires_at='2000-01-01T00:00:00Z' if name=='expired' else '',status='invalid' if name=='invalid-offline' else 'stopped',online=name=='online',client_online=name!='client-offline',recovery_protocol=0 if name=='old-client' else 2) for i,name in enumerate(names)]
    posted=[]
    with sync_playwright() as p:
        browser=p.chromium.launch(headless=True)
        page=browser.new_page(viewport={'width':1440,'height':950});errors=[]
        page.on('pageerror',lambda err:errors.append(str(err)))
        def handle(route):
            path=urlsplit(route.request.url).path
            if path=='/auth/session':route.fulfill(json={'username':'qa','csrf_token':'qa-only'});return
            if path=='/api/tunnels':route.fulfill(json=records);return
            if path=='/api/domain-zones':route.fulfill(json={'domain_zone':'port.example','domain_zones':['port.example']});return
            if path.endswith('/recover'):
                owner=path.split('/')[-2];posted.append(owner)
                assert route.request.headers['x-csrf-token']=='qa-only'
                record=next(r for r in records if r['owner']==owner)
                record['recovery_status']='waiting_client' if not record['client_online'] else 'waiting_upgrade' if record['recovery_protocol']<2 else 'pending'
                record['recovery_message']='请求已保存，等待客户端连接或升级'
                route.fulfill(status=202,json={'message':'手动唤醒请求已保存'});return
            file=static/('index.html' if path=='/' else path.rsplit('/',1)[-1])
            if file.exists():route.fulfill(body=file.read_bytes(),content_type={'.html':'text/html','.js':'application/javascript','.css':'text/css','.svg':'image/svg+xml'}.get(file.suffix,'text/plain'))
            else:route.fulfill(status=404,body='')
        page.route('http://portmap-wake.test/**',handle);page.goto('http://portmap-wake.test/',wait_until='networkidle')
        for name in names[:4]:
            button=page.locator('tbody tr').filter(has_text=name).get_by_role('button',name='唤醒隧道')
            assert button.is_enabled(),name
            button.click();page.wait_for_timeout(50)
        assert len(posted)==4
        assert '等待客户端连接' in page.locator('tbody').inner_text()
        assert '等待客户端升级' in page.locator('tbody').inner_text()
        for name in ['disabled','expired']:
            row=page.locator('tbody tr').filter(has_text=' / '+name)
            assert row.get_by_role('button',name='唤醒隧道').is_disabled()
            assert '请先' in row.inner_text()
        assert page.locator('tbody tr').filter(has_text=' / online').get_by_role('button',name='重新连接').is_enabled()
        assert not errors,errors
        browser.close()
    print(json.dumps({'passed':True,'offline_buttons_clicked':4,'policy_reasons_visible':True,'pending_is_not_success':True,'javascript_errors':errors}))


if __name__=='__main__':main()
