// Local-only navigation policy; use the external Playwright environment in
// web-ui.browser.test.cjs.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { createServer } = require('node:http');
const { chromium } = require('playwright');

test('app links stay framed, external tabs are isolated, and host links are blocked', { timeout: 40000 }, async () => {
  const source = readFileSync(__dirname + '/web-ui.js', 'utf8');
  const servers = [];
  let browser, host, app, hostHits = 0;
  async function serve(handler) {
    const server = createServer(handler); servers.push(server);
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
    return `http://127.0.0.1:${server.address().port}`;
  }
  try {
    const external = await serve((_req, res) => res.end('external app'));
    app = await serve((req, res) => {
      if (req.url === '/web-ui.js') {
        res.setHeader('Content-Type', 'application/javascript');
        return res.end(`(function(trustedParentOrigin){${source}\n})(${JSON.stringify(host)});`);
      }
      if (req.url === '/download') {
        res.setHeader('Content-Disposition', 'attachment; filename="report.txt"');
        return res.end('report');
      }
      res.setHeader('Content-Type', 'text/html');
      res.end(`<!doctype html><script src="/web-ui.js"></script>
        <a id="relative" href="next?q=1#row" target="_blank"><span>Relative</span></a>
        <a id="absolute" href="${app}/absolute" target="_top">Absolute</a>
        <a id="external" href="${external}/outside" target="_self" rel="author">External</a>
        <a id="download" href="/download" download target="_self">Download</a>
        <script>
        for (const type of ['click','auxclick']) addEventListener(type, event => {
          if (event.target.closest('a')?.id !== 'blocked') return;
          const prevented = event.defaultPrevented;
          setTimeout(() => parent.postMessage({action:'blocked-click',prevented}, ${JSON.stringify(host)}), 0);
        });
        </script>`);
    });
    host = await serve((req, res) => {
      res.setHeader('Content-Type', 'text/html');
      if (req.url.startsWith('/blocked')) { hostHits++; return res.end('Host destination'); }
      res.end(`<!doctype html><iframe src="${app}/" ${req.url === '/sandbox' ? 'sandbox="allow-scripts allow-same-origin allow-forms allow-downloads allow-popups"' : ''}></iframe>
        <script>
        window.reports=[];window.navigationRequests=[];
        const frame=document.querySelector('iframe');
        frame.onload=()=>frame.contentWindow.postMessage({type:'airlock:web-ui:v1',action:'probe',nonce:'link-fixture'}, ${JSON.stringify(app)});
        addEventListener('message',event=>{
          if(event.source!==frame.contentWindow||event.origin!==${JSON.stringify(app)})return;
          if(event.data.action==='blocked-click')reports.push(event.data);
          if(event.data.action==='navigate')navigationRequests.push(event.data);
        });
        </script>`);
    });
    browser = await chromium.launch({headless:true,
      ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH ? {executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH} : {})});
    const context = await browser.newContext();
    await context.addInitScript(() => Object.defineProperty(window, 'navigation', { value: undefined, configurable: true }));
    const page = await context.newPage();
    page.setDefaultTimeout(3000); page.setDefaultNavigationTimeout(3000);
    for (const path of ['/', '/sandbox']) {
      await page.goto(host + path);
      const frame = page.frames().find(frame => frame.url().startsWith(app));
      const ready = () => frame.waitForFunction(() => document.documentElement.dataset.airlockEmbedded === 'true');
      await ready();
      await frame.locator('#relative span').click();
      await frame.waitForURL(app + '/next?q=1#row'); await ready();
      await frame.locator('#absolute').click();
      await frame.waitForURL(app + '/absolute'); await ready();
      assert.equal(page.url(), host + path);
      const popupPromise = page.waitForEvent('popup');
      await frame.locator('#external').click();
      const popup = await popupPromise; await popup.waitForURL(external + '/outside');
      assert.equal(await popup.evaluate(() => window.opener), null);
      assert.equal(await popup.evaluate(() => document.referrer), '');
      await popup.close();
      const downloadPromise = page.waitForEvent('download');
      await frame.locator('#download').click();
      assert.equal((await downloadPromise).suggestedFilename(), 'report.txt');
      let count = 0;
      const cases = [
        {href:host+'/blocked'}, {href:host+'/blocked',modifiers:['Control']},
        {href:host+'/blocked',modifiers:['Meta']}, {href:host+'/blocked',modifiers:['Shift']},
        {href:host+'/blocked',modifiers:['Alt']}, {href:host+'/blocked',button:'middle'},
        {href:host+'/blocked',download:true}, {href:host.replace('127','%31%32%37')+'/blocked'},
        {href:host.replace('://','://user:secret@')+'/blocked'}, {href:'blocked',base:host+'/'},
      ];
      for (const item of cases) {
        await frame.evaluate(item => {
          document.querySelector('#blocked')?.remove(); document.querySelector('base')?.remove();
          if(item.base){const base=document.createElement('base');base.href=item.base;document.head.append(base)}
          const link=document.createElement('a');link.id='blocked';link.href=item.href;link.target='_top';
          link.textContent='Blocked host';if(item.download)link.download='file';document.body.append(link);
        }, item);
        await frame.locator('#blocked').click({modifiers:item.modifiers || [],button:item.button || 'left'});
        await page.waitForFunction(count => reports.length === count, ++count);
        assert.equal(await page.evaluate(() => reports.at(-1).prevented), true);
        assert.equal(frame.url(), app + '/absolute'); assert.equal(page.url(), host + path);
        assert.equal(context.pages().length, 1); assert.equal(hostHits, 0);
        assert.deepEqual(await page.evaluate(() => navigationRequests), []);
      }
      await frame.evaluate(() => {
        document.querySelector('base')?.remove();
        window.cancelled=[];
        addEventListener('click',event=>{
          if(event.target.closest('a')?.id!=='cancelled')return;
          const before=event.defaultPrevented;event.preventDefault();window.cancelled.push(before);
        });
      });
      for (const href of ['/cancelled', external + '/cancelled']) {
        await frame.evaluate(href => {
          document.querySelector('#cancelled')?.remove();const link=document.createElement('a');
          link.id='cancelled';link.href=href;link.textContent='App action';document.body.append(link);
        }, href);
        await frame.locator('#cancelled').click();
        assert.equal(frame.url(), app + '/absolute');assert.equal(context.pages().length, 1);
      }
      assert.deepEqual(await frame.evaluate(() => cancelled), [false,false]);
    }
  } finally {
    if(browser)await browser.close();
    await Promise.all(servers.map(server=>new Promise(resolve=>server.close(resolve))));
  }
});
