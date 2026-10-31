const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { runInNewContext } = require('node:vm');

const script = readFileSync(__dirname + '/web-ui.js', 'utf8');
const origin = 'https://dashboard.example:8443';

function fixture(withNavigation = true, disableNavigation = false, standalone = false) {
  const listeners = {};
  const messages = [];
  const traversals = [];
  let reloads = 0;
  const attributes = new Map([['data-theme', 'night']]);
  const document = { currentScript: { dataset: { airlockDisableNavigation: String(disableNavigation) } },
    documentElement: { setAttribute: (name, value) => attributes.set(name, value) },
    addEventListener: (event, callback) => { listeners[event] = callback; } };
  const parent = { postMessage: (message, target) => messages.push({ message, target }) };
  const navigation = { currentEntry: { index: 1 },
    entries: () => [{ index: 0, key: 'home', url: 'https://app.example/' }, { index: 1, key: 'page', url: 'https://app.example/page' }],
    traverseTo: key => { traversals.push(key); return { finished: Promise.resolve() }; },
    addEventListener: (event, callback) => { listeners[event] = callback; },
  };
  const window = { parent, addEventListener: (event, callback) => { listeners[event] = callback; } };
  const location = { origin: 'https://app.example', href: 'https://app.example/page' };
  window.history = Object.fromEntries(['pushState', 'replaceState'].map(method => [method, function(state, unused, url) {
    assert.equal(this, window.history);
    if (url === 'invalid') throw new Error('invalid URL');
    location.href = new URL(url, location.href).href;
  }]));
  window.location = { reload: () => { reloads++; } };
  if (standalone) window.parent = window;
  if (withNavigation) window.navigation = navigation;
  runInNewContext('(function(trustedParentOrigin) {' + script + '})("' + origin + '");',
    { window, document, navigation: withNavigation ? navigation : undefined, location, URL });
  const send = (data, source = parent, senderOrigin = origin) => listeners.message({ source, origin: senderOrigin, data: { type: 'airlock:web-ui:v1', ...data } });
  return { window, parent, navigation, messages, traversals, listeners, send, attributes, reloads: () => reloads };
}

test('trusts only the configured exact parent and binds commands to the probe nonce', () => {
  const f = fixture();
  assert.equal(f.messages[0].message.action, 'ready');
  assert.equal(f.messages[0].target, origin);
  f.send({ action: 'probe', nonce: 'n' }, {}, origin);
  f.send({ action: 'probe', nonce: 'n' }, f.parent, 'https://evil.example');
  f.send({ action: 'probe', nonce: 'n' }, f.parent, 'https://dashboard.example');
  f.send({ action: 'probe', nonce: '' });
  f.send({ action: 'probe', nonce: 'n'.repeat(129) });
  f.send({ action: 'probe', nonce: 'n', type: 'airlock:web-ui:v2' });
  assert.equal(f.messages.length, 1);
  assert.equal(f.attributes.has('data-airlock-embedded'), false);
  f.send({ action: 'probe', nonce: 'n' });
  assert.equal(f.attributes.get('data-airlock-embedded'), 'true');
  assert.equal(f.attributes.get('data-theme'), 'night');
  assert.deepEqual(Array.from(f.messages.at(-2).message.features), ['location', 'auth-recovery', 'navigation']);
  assert.equal(f.messages.at(-1).message.canGoBack, true);
  f.send({ action: 'back', nonce: 'stale' });
  f.send({ action: 'back', nonce: 'n' }, {}, origin);
  assert.equal(f.traversals.length, 0);
  f.send({ action: 'back', nonce: 'n' });
  assert.deepEqual(f.traversals, ['home']);
});

test('reports actual entry changes, excluding foreign, missing and auth entries', () => {
  const f = fixture();
  f.send({ action: 'probe', nonce: 'n' });
  for (const entries of [[], [{ index: 0, key: 'auth', url: 'https://app.example/__air/callback' }],
    [{ index: 0, key: 'foreign', url: 'https://other.example/' }], [{ index: -1, key: 'gap', url: 'https://app.example/' }]]) {
    f.navigation.entries = () => entries;
    f.listeners.currententrychange();
    assert.equal(f.messages.at(-1).message.canGoBack, false);
    f.send({ action: 'back', nonce: 'n' });
  }
  assert.equal(f.traversals.length, 0);
});

test('unsupported browsers honestly disable Back', () => {
  const f = fixture(false);
  f.send({ action: 'probe', nonce: 'n' });
  assert.equal(f.messages.at(-1).message.canGoBack, undefined);
  assert.deepEqual(Array.from(f.messages.at(-2).message.features), ['location', 'auth-recovery']);
  f.send({ action: 'back', nonce: 'n' });
  assert.equal(f.traversals.length, 0);
});

test('browsers without Navigation API report successful history URL changes without enabling Back', () => {
  const f = fixture(false);
  f.send({ action: 'probe', nonce: 'n' });
  for (const method of ['pushState', 'replaceState']) {
    f.window.history[method]({}, '', '/orders?q=one#row');
    assert.equal(f.messages.at(-1).message.url, 'https://app.example/orders?q=one#row');
    assert.equal(f.messages.at(-1).message.canGoBack, undefined);
  }
  const count = f.messages.length;
  assert.throws(() => f.window.history.pushState({}, '', 'invalid'));
  assert.equal(f.messages.length, count);
});

test('disabled navigation retains location and authentication recovery', () => {
  const f = fixture(true, true);
  f.send({ action: 'probe', nonce: 'n' });
  assert.deepEqual(Array.from(f.messages.at(-2).message.features), ['location', 'auth-recovery']);
  assert.equal(f.messages.at(-1).message.url, 'https://app.example/page');
  f.send({ action: 'back', nonce: 'n' });
  assert.equal(f.traversals.length, 0);
  f.listeners['htmx:response:error']({ detail: { ctx: { response: { status: 401 } } } });
  assert.equal(f.reloads(), 1);
});

test('standalone installs only optional htmx error integration', () => {
  const f = fixture(false, false, true);
  assert.equal(f.messages.length, 0);
  assert.equal(f.listeners.message, undefined);
  assert.equal(f.attributes.has('data-airlock-embedded'), false);
  for (const event of [{}, { detail: {} }, { detail: { ctx: { response: { status: 500 } } } }]) {
    f.listeners['htmx:response:error'](event);
  }
  assert.equal(f.reloads(), 0);
  f.listeners['htmx:response:error']({ detail: { ctx: { response: { status: 401 } } } });
  assert.equal(f.reloads(), 1);
});

test('embedded presentation is independent of navigation support and survives route changes', () => {
  for (const [withNavigation, disableNavigation] of [[true, false], [false, false], [true, true]]) {
    const f = fixture(withNavigation, disableNavigation);
    f.send({ action: 'back', nonce: 'n' });
    assert.equal(f.attributes.has('data-airlock-embedded'), false);
    f.send({ action: 'probe', nonce: 'n' });
    for (const event of ['popstate', 'hashchange', 'pageshow']) f.listeners[event]();
    assert.equal(f.attributes.get('data-airlock-embedded'), 'true');
    assert.equal(f.attributes.get('data-theme'), 'night');
  }
});
