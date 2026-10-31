const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { runInNewContext } = require('node:vm');
const script = readFileSync(__dirname + '/web-ui.js', 'utf8');
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));

function fixture(values, manualTimers = false) {
  const events = [], requests = [], observers = [], listeners = new Map();
  const timers = new Map();
  function schedule(callback, delay) {
    const id = {};
    timers.set(id, { callback, delay });
    return id;
  }
  const roots = values.map(value => ({ value, isConnected: true,
    getAttribute: () => value, dispatchEvent: event => events.push({ root: value, event }) }));
  function on(name, callback) { listeners.set(name, [...(listeners.get(name) || []), callback]); }
  const document = { readyState: 'complete', currentScript: { dataset: {} }, documentElement: {},
    querySelectorAll: () => roots.filter(root => root.isConnected), addEventListener: on };
  const window = { addEventListener: on, location: { reload: () => assert.fail('unexpected auth reload') } };
  window.parent = window;
  class Observer {
    constructor(callback) { this.callback = callback; observers.push(this); }
    observe() { this.active = true; }
    disconnect() { this.active = false; }
  }
  async function fetch(url, options) {
    const request = { url, options, aborted: false };
    const body = new ReadableStream({ start(controller) { request.controller = controller; },
      cancel() { request.cancelled = true; } });
    options.signal.addEventListener('abort', () => { request.aborted = true; if (!request.cancelled) request.controller.close(); });
    requests.push(request);
    return new Response(body, { headers: { 'content-type': 'text/event-stream' } });
  }
  runInNewContext('(function(trustedParentOrigin){' + script + '})("https://dashboard.example");', {
    document, window, fetch, MutationObserver: Observer, AbortController, URLSearchParams,
    TextDecoder, CustomEvent, setTimeout: manualTimers ? schedule : setTimeout,
    clearTimeout: manualTimers ? id => timers.delete(id) : clearTimeout,
  });
  function send(request, topic, revision) {
    request.controller.enqueue(new TextEncoder().encode('event: airlock:change\ndata: ' + JSON.stringify({ topic, revision }) + '\n\n'));
  }
  function fire(name, data = {}) { for (const listener of listeners.get(name) || []) listener(data); }
  return { roots, events, requests, observers, send, fire, timers,
    runTimers: delay => {
      for (const [id, timer] of [...timers]) {
        if (timer.delay !== delay) continue;
        timers.delete(id);
        timer.callback();
      }
    },
    update: () => observers.find(observer => observer.active)?.callback(),
    close: () => fire('pagehide') };
}

test('strict topic grammar, one optional standalone stream and exact nonbubbling root events', async () => {
  const f = fixture(['tasks', 'tasks users', 'tasks,users', 'Tasks', 'tasks tasks', 'tasks\tusers']);
  try {
    assert.equal(f.requests.length, 1);
    assert.equal(f.requests[0].url, '/__air/changes?topic=tasks&topic=users');
    assert.equal(f.requests[0].options.credentials, 'same-origin');
    const revision = '11111111-1111-4111-8111-111111111111';
    f.send(f.requests[0], 'tasks', revision);
    f.send(f.requests[0], 'tasks', revision);
    await pause(80);
    assert.deepEqual(f.events.map(item => item.root), ['tasks', 'tasks users', 'tasks\tusers']);
    for (const { event } of f.events) {
      assert.equal(event.bubbles, false);
      assert.equal(event.detail.topic, 'tasks');
      assert.equal(event.detail.revision, revision);
    }
    for (let n = 1; n <= 80; n++) {
      f.send(f.requests[0], 'tasks', '11111111-1111-4111-8111-' + String(n).padStart(12, '0'));
    }
    await pause(80);
    assert.equal(f.events.length, 6, 'a burst produces one event per matching root');
    assert.equal(f.events.at(-1).event.detail.revision, '11111111-1111-4111-8111-000000000080');
    await pause(100);
    assert.equal(f.requests.length, 1, 'no polling or duplicate streams');
  } finally { f.close(); }
});

test('partial unmount, pagehide and bfcache restore release subscriptions and observers', async () => {
  const f = fixture(['tasks']);
  try {
    await pause(0);
    f.roots[0].isConnected = false;
    f.update();
    assert.equal(f.requests[0].aborted, true);
    assert.equal(f.requests.length, 1);
    f.roots[0].isConnected = true;
    f.update();
    assert.equal(f.requests.length, 2);
    f.fire('pagehide');
    assert.equal(f.requests[1].aborted, true);
    assert.equal(f.observers.filter(observer => observer.active).length, 0);
    f.fire('pageshow', { persisted: true });
    assert.equal(f.requests.length, 3);
    assert.equal(f.observers.filter(observer => observer.active).length, 1);
  } finally { f.close(); }
});

test('unmarked pages and invalid or excessive topic sets open no connection', () => {
  for (const values of [[], ['tasks,users'], [Array.from({ length: 33 }, (_, n) => `topic${n}`).join(' ')],
    Array.from({ length: 33 }, (_, n) => `topic${n}`)]) {
    const f = fixture(values);
    try { assert.equal(f.requests.length, 0); } finally { f.close(); }
  }
});

const revision = n => '11111111-1111-4111-8111-' + String(n).padStart(12, '0');
const frame = (topic, n) => 'event: airlock:change\ndata: ' + JSON.stringify({ topic, revision: revision(n) }) + '\n\n';
const encode = text => new TextEncoder().encode(text);

test('one stream read exceeding 16 KiB drains bounded frames and coalesces the latest revision per topic', async () => {
  const f = fixture(['tasks', 'users'], true);
  try {
    const frames = Array.from({ length: 300 }, (_, n) => frame(n % 2 ? 'users' : 'tasks', n));
    const chunk = encode(frames.join(''));
    assert.ok(chunk.byteLength > 16384);
    assert.ok(frames.every(value => value.length < 16384));
    f.requests[0].controller.enqueue(chunk);
    await pause(0);
    assert.equal(f.requests[0].cancelled, undefined, 'valid aggregate chunk keeps its reader open');
    assert.deepEqual([...f.timers.values()].map(timer => timer.delay), [50], 'only the coalescing timer is scheduled');
    f.runTimers(50);
    assert.deepEqual(f.events.map(({ event }) => [event.detail.topic, event.detail.revision]),
      [['tasks', revision(298)], ['users', revision(299)]]);
    f.send(f.requests[0], 'tasks', revision(300));
    await pause(0);
    f.runTimers(50);
    assert.equal(f.events.at(-1).event.detail.revision, revision(300), 'subsequent reads remain usable');
    assert.equal(f.requests.length, 1);
  } finally { f.close(); }
});

test('streaming UTF-8 decoding and split LF frame delimiters retain complete and partial frames', async () => {
  const f = fixture(['tasks', 'users'], true);
  try {
    // Splitting one code point into three reads must not add replacement characters
    // that push an exactly bounded decoded frame over the limit.
    const first = ('event: airlock:change\ndata: ' + JSON.stringify({ topic: 'tasks', revision: revision(1), label: '🌍' })).padEnd(16384, ' ') + '\n\n';
    const bytes = encode(first);
    const multibyte = bytes.indexOf(0xf0);
    const chunks = [bytes.slice(0, multibyte + 1), bytes.slice(multibyte + 1, multibyte + 2), bytes.slice(multibyte + 2, -1),
      encode('\n' + frame('users', 2) + frame('tasks', 3).slice(0, 35)),
      encode(frame('tasks', 3).slice(35))];
    for (const chunk of chunks) {
      f.requests[0].controller.enqueue(chunk);
      await pause(0);
    }
    f.runTimers(50);
    assert.deepEqual(f.events.map(({ event }) => [event.detail.topic, event.detail.revision]),
      [['tasks', revision(3)], ['users', revision(2)]]);
    assert.equal(f.requests[0].cancelled, undefined);
    assert.equal(f.timers.size, 0);
  } finally { f.close(); }
});

test('frame limit counts decoded UTF-16 code units, excluding the LF delimiter', async () => {
  const f = fixture(['tasks'], true);
  try {
    const prefix = frame('tasks', 1).slice(0, -2);
    const content = prefix + ' '.repeat(16384 - prefix.length);
    // A full-sized frame may arrive before either delimiter character.
    for (const chunk of [content, '\n', '\n' + frame('tasks', 2)]) {
      f.requests[0].controller.enqueue(encode(chunk));
      await pause(0);
    }
    // An ignored UTF-8 comment is bounded by decoded code units, not bytes.
    const comment = ':' + '🌍'.repeat(8000);
    assert.ok(encode(comment).byteLength > 16384 && comment.length <= 16384);
    f.requests[0].controller.enqueue(encode(comment + '\n\n'));
    await pause(0);
    f.runTimers(50);
    assert.equal(f.events.length, 1);
    assert.equal(f.events[0].event.detail.revision, revision(2));
    assert.equal(f.requests[0].cancelled, undefined);
    assert.equal(f.timers.size, 0);
  } finally { f.close(); }
});

test('oversized complete and unterminated frames cancel the reader and schedule one bounded reconnect', async t => {
  for (const [name, chunks, expected] of [
    ['complete frame', [':' + 'x'.repeat(16384) + '\n\n' + frame('tasks', 9)], []],
    ['complete change frame', [frame('tasks', 1).slice(0, -2) + ' '.repeat(16384) + '\n\n' + frame('tasks', 9)], []],
    ['split unterminated frame', ['x'.repeat(8192), 'x'.repeat(8193)], []],
    ['complete frames then oversized trailing partial', [frame('tasks', 1) + frame('tasks', 2) + 'x'.repeat(16385)], [revision(2)]],
  ]) {
    await t.test(name, async () => {
      const f = fixture(['tasks'], true);
      try {
        for (const chunk of chunks) {
          f.requests[0].controller.enqueue(encode(chunk));
          await pause(0);
        }
        assert.equal(f.requests[0].cancelled, true);
        f.runTimers(50);
        assert.deepEqual(f.events.map(({ event }) => event.detail.revision), expected);
        assert.deepEqual([...f.timers.values()].map(timer => timer.delay), [1000]);
        assert.equal(f.requests.length, 1, 'no immediate reconnect loop');
        f.runTimers(1000);
        await pause(0);
        assert.equal(f.requests.length, 2);
        assert.equal(f.timers.size, 0, 'one replacement stream remains open');
        f.send(f.requests[1], 'tasks', revision(10));
        await pause(0);
        f.runTimers(50);
        assert.equal(f.events.at(-1).event.detail.revision, revision(10));
      } finally { f.close(); }
    });
  }
});
