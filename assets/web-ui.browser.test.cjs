// Install Playwright in an external test environment, then run:
// node --test assets/web-ui.browser.test.cjs
// WEB_UI_SCREENSHOT_DIR optionally retains standalone/embedded resize captures.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync, mkdirSync } = require('node:fs');
const { createServer } = require('node:http');
const { chromium } = require('playwright');

test('trusted embedded presentation, dynamic content and live viewport resizing', async () => {
  const script = readFileSync(__dirname + '/web-ui.js', 'utf8');
  const css = readFileSync(__dirname + '/web-ui.css', 'utf8');
  const servers = [];
  let browser;
  async function serve(handler) {
    const server = createServer(handler);
    servers.push(server);
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
    return `http://127.0.0.1:${server.address().port}`;
  }
  try {
    let hostOrigin;
    const appOrigin = await serve((req, res) => {
      if (req.url === '/web-ui.js') {
        res.setHeader('Content-Type', 'application/javascript');
        return res.end(`(function(trustedParentOrigin){${script}\n})(${JSON.stringify(hostOrigin)});`);
      }
      if (req.url === '/web-ui.css' || req.url === '/app.css') {
        res.setHeader('Content-Type', 'text/css');
        return res.end(req.url === '/web-ui.css' ? css : `
          *{box-sizing:border-box}body{margin:0;font:16px system-ui;background:#f5f7fa;color:#18202b}
          main{width:100%;min-width:0;padding:16px}header{margin-bottom:16px}h1{margin:0;font-size:20px}
          p{margin:4px 0;font-size:14px}nav{display:flex;flex-wrap:wrap;gap:8px;margin-bottom:16px}
          button,input{font:inherit;padding:8px;min-width:0}input{flex:1}
          .grid{display:grid;gap:12px;grid-template-columns:minmax(0,1fr)}
          article{min-width:0;padding:16px;background:white;border:1px solid #ccd3dc;border-radius:8px}
          @media(min-width:600px){.grid{grid-template-columns:repeat(2,minmax(0,1fr))}}
        `);
      }
      res.setHeader('Content-Type', 'text/html; charset=utf-8');
      res.end(`<!doctype html><html lang="en" data-theme="light"><head>
        <meta name="viewport" content="width=device-width,initial-scale=1">
        <link rel="stylesheet" href="/app.css"><link rel="stylesheet" href="/web-ui.css">
        <script src="/web-ui.js"></script></head><body><main>
        <header data-airlock-standalone-only class="flex" style="display:flex"><h1>Todo</h1><p>Team tasks</p></header>
        <nav><input aria-label="Filter tasks" placeholder="Filter tasks"><button>Add task</button><a href="/next">Next</a></nav>
        <section class="grid"><article>Review the weekly report</article><article>Prepare tomorrow’s meeting</article></section>
        </main></body></html>`);
    });
    function host(req, res) {
      res.setHeader('Content-Type', 'text/html; charset=utf-8');
      res.end(`<!doctype html><html><body style="margin:0;background:#e4e8ee;font:16px system-ui">
        <div style="padding:12px">Airlock · Todo</div>
        <iframe title="Todo" style="display:block;border:0;width:100%;height:650px" src="${appOrigin}/"></iframe>
        <script>const frame=document.querySelector('iframe');frame.onload=()=>frame.contentWindow.postMessage(
          {type:'airlock:web-ui:v1',action:'probe',nonce:'browser-test'},${JSON.stringify(appOrigin)});</script>
        </body></html>`);
    }
    hostOrigin = await serve(host);
    const evilOrigin = await serve(host);
    browser = await chromium.launch({ headless: true,
      ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : {}) });
    const page = await browser.newPage({ viewport: { width: 375, height: 800 } });
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const output = process.env.WEB_UI_SCREENSHOT_DIR;
    if (output) mkdirSync(output, { recursive: true });
    async function capture(name) {
      if (output) await page.screenshot({ path: `${output}/${name}.png` });
    }
    await page.goto(appOrigin);
    assert.equal(await page.locator('header').isVisible(), true);
    assert.equal(await page.locator('html').getAttribute('data-airlock-embedded'), null);
    await capture('standalone-375');

    await page.goto(evilOrigin);
    let frame = page.frames().find(frame => frame.url().startsWith(appOrigin));
    assert.equal(await frame.locator('header').isVisible(), true);
    assert.equal(await frame.locator('html').getAttribute('data-airlock-embedded'), null);

    await page.goto(hostOrigin);
    frame = page.frames().find(frame => frame.url().startsWith(appOrigin));
    await frame.waitForFunction(() => document.documentElement.dataset.airlockEmbedded === 'true');
    assert.equal(await frame.locator('header').isVisible(), false);
    assert.equal(await frame.locator('button').isVisible(), true);
    assert.equal(await frame.locator('html').getAttribute('data-theme'), 'light');
    assert.equal(await frame.locator('nav').evaluate(el => el.getBoundingClientRect().top), 16);
    await capture('embedded-375');
    await frame.evaluate(() => {
      const identity = document.createElement('header');
      identity.setAttribute('data-airlock-standalone-only', '');
      identity.textContent = 'Dynamically inserted identity';
      document.querySelector('main').prepend(identity);
      history.pushState({}, '', '/tasks');
    });
    assert.equal(await frame.locator('header').first().isVisible(), false);
    assert.equal(await frame.locator('header').count(), 2);
    await page.setViewportSize({ width: 1100, height: 800 });
    const initialFrame = frame;
    for (const [width, columns] of [[800, 2], [375, 1], [650, 2], [320, 1]]) {
      await page.locator('iframe').evaluate((el, width) => { el.style.width = `${width}px`; }, width);
      await frame.waitForFunction(width => innerWidth === width, width);
      const layout = await frame.evaluate(() => ({
        columns: getComputedStyle(document.querySelector('.grid')).gridTemplateColumns.split(' ').length,
        overflow: document.documentElement.scrollWidth > innerWidth,
        embedded: document.documentElement.dataset.airlockEmbedded,
        theme: document.documentElement.dataset.theme,
      }));
      assert.deepEqual(layout, { columns, overflow: false, embedded: 'true', theme: 'light' });
      assert.equal(frame, initialFrame);
      await capture(`embedded-pane-${width}`);
    }
    await frame.locator('a').click();
    await frame.waitForURL(`${appOrigin}/next`);
    await frame.waitForFunction(() => document.documentElement.dataset.airlockEmbedded === 'true');
    assert.equal(await frame.locator('header').isVisible(), false);
    // A nested app/viewer frame has the app as its parent, not the dashboard.
    // Iframe ancestry alone must not activate embedded presentation there.
    await frame.evaluate(origin => {
      const viewer = document.createElement('iframe');
      viewer.title = 'Nested viewer';
      viewer.src = origin + '/viewer';
      viewer.onload = () => viewer.contentWindow.postMessage(
        { type: 'airlock:web-ui:v1', action: 'probe', nonce: 'nested' }, origin);
      document.body.append(viewer);
    }, appOrigin);
    const viewerElement = await frame.locator('iframe').elementHandle();
    const viewer = await viewerElement.contentFrame();
    await viewer.waitForURL(`${appOrigin}/viewer`);
    assert.equal(await viewer.locator('header').isVisible(), true);
    assert.equal(await viewer.locator('html').getAttribute('data-airlock-embedded'), null);
    assert.deepEqual(errors, []);
  } finally {
    if (browser) await browser.close();
    await Promise.all(servers.map(server => new Promise(resolve => server.close(resolve))));
  }
});
