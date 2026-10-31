const assert = require('node:assert/strict');
const { mkdirSync } = require('node:fs');
const { chromium } = require('playwright');

(async () => {
  const app = process.env.UI_CHANGE_FIXTURE_URL;
  const host = process.env.UI_CHANGE_HOST_URL;
  assert.ok(app && host, 'run through TestUIChangesBrowser with an isolated PostgreSQL fixture');
  const browser = await chromium.launch({ headless: true, ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH ?
    { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : {}) });
  const screenshots = process.env.WEB_UI_SCREENSHOT_DIR;
  if (screenshots) mkdirSync(screenshots, { recursive: true });
  try {
    const contexts = await Promise.all([process.env.UI_CHANGE_USER_A, process.env.UI_CHANGE_USER_B].map(async user => {
      const context = await browser.newContext({ viewport: { width: 900, height: 800 } });
      await context.addCookies([{ name: 'fixture_user', value: user, url: app }]);
      return context;
    }));
    const page = await contexts[0].newPage();
    const other = await contexts[1].newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    let fragmentReads = 0, documents = 0;
    page.on('request', request => {
      if (request.url().startsWith(app + '/word?')) {
        fragmentReads++;
        assert.equal(new URL(request.url()).searchParams.get('filter'), 'keep');
      }
      if (request.isNavigationRequest()) documents++;
    });
    const initialFragment = page.waitForResponse(response => response.url().startsWith(app + '/word?'));
    await Promise.all([page.goto(app), other.goto(app)]);
    await initialFragment;
    await page.locator('input[name=word]').fill('unfinished edit');
    await page.locator('input[name=word]').focus();
    await page.evaluate(() => { document.documentElement.dataset.marker = 'same-document'; window.scrollTo(0, 60); });
    const before = await page.evaluate(() => ({ y: scrollY, reads: performance.getEntriesByType('navigation').length }));
    const navCount = documents;
    if (screenshots) await page.screenshot({ path: screenshots + '/standalone-before.png' });
    assert.equal((await contexts[0].request.post(app + '/tool/set_word', { form: { word: 'Bonjour' } })).status(), 204);
    await page.waitForFunction(() => document.querySelector('#word').textContent === 'Bonjour');
    assert.equal(await other.locator('#word').textContent(), 'Hello');
    assert.equal(await page.locator('input[name=word]').inputValue(), 'unfinished edit');
    assert.equal(await page.locator('input[name=word]').evaluate(el => el === document.activeElement), true);
    assert.equal(await page.evaluate(() => scrollY), before.y);
    assert.equal(await page.locator('html').getAttribute('data-marker'), 'same-document');
    assert.equal(documents, navCount);
    if (screenshots) await page.screenshot({ path: screenshots + '/standalone-tool-change.png' });
    const idleReads = fragmentReads;
    await page.waitForTimeout(1200);
    assert.equal(fragmentReads, idleReads, 'idle page must not poll fragments');
    await page.locator('input[name=word]').fill('Hola');
    await page.locator('button').click();
    await page.waitForFunction(() => document.querySelector('#word').textContent === 'Hola');
    assert.equal(await page.locator('input[name=word]').inputValue(), 'Hola');
    // A disconnected document catches up from the durable state on reconnect.
    await contexts[0].setOffline(true);
    await page.waitForTimeout(100);
    const control = await browser.newContext();
    await control.addCookies([{ name: 'fixture_user', value: process.env.UI_CHANGE_USER_A, url: app }]);
    assert.equal((await control.request.post(app + '/tool/set_word', { form: { word: 'Salut' } })).status(), 204);
    assert.equal((await contexts[1].request.post(app + '/tool/set_word', { form: { word: 'Other user' } })).status(), 204);
    await contexts[0].setOffline(false);
    await page.waitForFunction(() => document.querySelector('#word').textContent === 'Salut');
    await control.close();
    // Removing/reinserting partial roots restarts exactly one active stream.
    await page.evaluate(() => { const root = document.querySelector('#word'); root.removeAttribute('data-airlock-topic'); });
    await page.waitForTimeout(100);
    await page.evaluate(() => document.querySelector('#word').setAttribute('data-airlock-topic', 'word'));
    await page.waitForTimeout(150);
    await page.goto(host);
    const frame = page.frames().find(frame => frame.url().startsWith(app));
    assert.ok(frame);
    await frame.waitForFunction(() => document.documentElement.dataset.airlockEmbedded === 'true');
    assert.equal(await frame.locator('header').isVisible(), false);
    await frame.locator('input[name=word]').fill('retain embedded edit');
    await frame.locator('input[name=word]').focus();
    assert.equal((await contexts[0].request.post(app + '/tool/set_word', { form: { word: 'Ciao' } })).status(), 204);
    await frame.waitForFunction(() => document.querySelector('#word').textContent === 'Ciao');
    assert.equal(await frame.locator('input[name=word]').inputValue(), 'retain embedded edit');
    assert.equal(await frame.locator('input[name=word]').evaluate(el => el === document.activeElement), true);
    assert.equal(await other.locator('#word').textContent(), 'Other user');
    if (screenshots) await page.screenshot({ path: screenshots + '/embedded-tool-change.png' });
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({ actualSDK: true, htmx: '4.0.0', standalone: true, embedded: true,
      userIsolation: true, focus: true, scroll: true, noReload: true, noPolling: true, fragmentReads, screenshots }));
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
