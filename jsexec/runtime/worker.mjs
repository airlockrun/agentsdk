import { getActiveResourcesInfo } from "node:process";
import timers from "node:timers";

// Captured operations and a private MessagePort keep protocol traffic separate
// from user-visible stdio/postMessage. Deno permissions, Go capability checks,
// and container limits enforce privilege isolation, not these global wrappers.
const AsyncFunction = (async function () {}).constructor;
const stringify = JSON.stringify, parse = JSON.parse, create = Object.create;
const keys = Object.keys,
  freeze = Object.freeze,
  define = Object.defineProperty,
  getProto = Object.getPrototypeOf,
  setProto = Object.setPrototypeOf;
const apply = Reflect.apply, construct = Reflect.construct;
// Define own entries without invoking retained prototype setters or iterators.
const push = (array, ...values) => {
  for (let i = 0; i < values.length; i++) {
    define(array, array.length, record([
      "value", values[i], "writable", true, "enumerable", true, "configurable", true,
    ]));
  }
};
const then = Function.call.bind(Promise.prototype.then);
const NativePromise = Promise, NativeError = Error, NativeString = String;
const reject = Promise.reject.bind(Promise);
const mapGet = Function.call.bind(Map.prototype.get),
  mapSet = Function.call.bind(Map.prototype.set);
const mapDelete = Function.call.bind(Map.prototype.delete);
const mapSize = Function.call.bind(
  Object.getOwnPropertyDescriptor(Map.prototype, "size").get,
);
const NativeMap = Map;
const textEncode = Function.call.bind(TextEncoder.prototype.encode);
const encoder = new TextEncoder();
const descriptor = Object.getOwnPropertyDescriptor;
const eventData = (event) => descriptor(event, "data").value;
const nativeSetTimeout = setTimeout, nativeClearTimeout = clearTimeout;
const nativeSetInterval = setInterval, nativeClearInterval = clearInterval;
const nativeWorker = Worker;
const timersOpen = new NativeMap();
// The pinned runtime's native leak tracker includes unreferenced timers from
// node:timers/promises. Node's public active-resource list excludes them.
const core = Deno[Deno.internal].core;
const enableTracing = core.setLeakTracingEnabled;
const leakTraces = core.getAllLeakTraces;
enableTracing(true);
// Access to low-level runtime controls makes the invocation non-reusable. The
// native core object remains available; its accounting cannot certify reuse
// after user code obtains controls that can disable the native tracer.
define(Deno[Deno.internal], "core", {
  get() {
    dirty();
    return core;
  },
  configurable: false,
});
let scope = null;
let port, send;

function record(fields) {
  const out = create(null);
  for (let i = 0; i < fields.length; i += 2) out[fields[i]] = fields[i + 1];
  return out;
}
function emit(fields) {
  send(stringify(record(fields)));
}
function bytes(value) {
  return textEncode(encoder, value).length;
}
function dirty() {
  if (scope) scope.dirty = true;
}

globalThis.setTimeout = function (fn, delay, ...args) {
  let id;
  id = nativeSetTimeout(() => {
    mapDelete(timersOpen, id);
    apply(fn, globalThis, args);
  }, delay);
  mapSet(timersOpen, id, true);
  return id;
};
globalThis.clearTimeout = function (id) {
  mapDelete(timersOpen, id);
  nativeClearTimeout(id);
};
globalThis.setInterval = function (fn, delay, ...args) {
  const id = nativeSetInterval(fn, delay, ...args);
  mapSet(timersOpen, id, true);
  return id;
};
globalThis.clearInterval = function (id) {
  mapDelete(timersOpen, id);
  nativeClearInterval(id);
};
if (Deno.unrefTimer) {
  const unref = Deno.unrefTimer;
  Deno.unrefTimer = function (id) {
    dirty();
    return unref(id);
  };
}
// V8 waitAsync timeouts are not Node timer handles.
if (Atomics.waitAsync) {
  const wait = Atomics.waitAsync;
  Atomics.waitAsync = function (...args) {
    dirty();
    return apply(wait, Atomics, args);
  };
}
// Unreferenced Node timers are not reported by getActiveResourcesInfo.
const sample = timers.setTimeout(() => {}, 1);
const timerProto = getProto(sample), unref = timerProto.unref;
define(timerProto, "unref", {
  value: function () {
    dirty();
    return apply(unref, this, []);
  },
  writable: false,
  configurable: false,
});
timers.clearTimeout(sample);
function TrackedWorker(...args) {
  dirty();
  return new nativeWorker(...args);
}
TrackedWorker.prototype = nativeWorker.prototype;
define(nativeWorker.prototype, "constructor", {
  value: TrackedWorker,
  writable: false,
  configurable: false,
});
globalThis.Worker = TrackedWorker;

function invoke(s, name, args) {
  if (!s.open) return reject(new NativeError("invocation scope closed"));
  if (
    mapSize(s.pending) >= s.limits.concurrentCalls || s.calls >= s.limits.calls
  ) return reject(new NativeError("broker concurrency or call limit exceeded"));
  const json = stringify(args);
  if (!s.open || bytes(json) > s.limits.outputBytes) {
    return reject(new NativeError("broker argument limit or closed scope"));
  }
  const call = ++s.calls;
  const p = new NativePromise((resolve, reject) => {
    mapSet(s.pending, call, record(["resolve", resolve, "reject", reject]));
  });
  // Mark the underlying broker promise handled even when user code drops it.
  then(p, undefined, () => {});
  emit(["type", "call", "id", s.id, "call", call, "name", name, "args", json]);
  return p;
}

async function execute(m) {
  const s = record([
    "id",
    m.id,
    "limits",
    m.options.limits,
    "pending",
    new NativeMap(),
    "calls",
    0,
    "open",
    true,
    "dirty",
    false,
    "drained",
    null,
  ]);
  scope = s;
  let logs = [], logBytes = 0, logOverflow = false;
  const log = (level, args) => {
    if (!s.open) return;
    let message = "";
    for (let i = 0; i < args.length; i++) {
      if (i) message += " ";
      message += typeof args[i] === "string"
        ? args[i]
        : stringify(args[i]) ?? NativeString(args[i]);
    }
    logBytes += bytes(message);
    if (logBytes > s.limits.logBytes || logs.length >= s.limits.logs) {
      logOverflow = true;
      return;
    }
    push(logs, record(["level", level, "message", message]));
  };
  const console = record([
    "log",
    (...a) => log("info", a),
    "info",
    (...a) => log("info", a),
    "warn",
    (...a) => log("warn", a),
    "error",
    (...a) => log("error", a),
  ]);
  const call = (name, ...args) => invoke(s, name, args);
  const roots = create(null);
  roots.air = record(["log", console.log]);
  for (let i = 0; i < (m.options.bindings?.length ?? 0); i++) {
    const binding = m.options.bindings[i], path = binding.path;
    let node = roots;
    for (let j = 0; j < path.length - 1; j++) {
      node = node[path[j]] ?? (node[path[j]] = create(null));
    }
    node[path[path.length - 1]] = (...args) => invoke(s, binding.name, args);
  }
  const names = keys(roots),
    params = ["invoke", "console", "user"],
    values = [call, console, m.options.user ? freeze(record([
      "id", m.options.user.id,
      "email", m.options.user.email,
      "displayName", m.options.user.displayName,
    ])) : null];
  for (let i = 0; i < names.length; i++) {
    push(params, names[i]);
    push(values, roots[names[i]]);
  }
  push(params, '"use strict";\n' + m.code);
  let result = record(["undefined", true, "logs", logs]), exception;
  try {
    const fn = construct(AsyncFunction, params);
    const value = await apply(fn, undefined, values);
    const json = stringify(value);
    if (json !== undefined) {
      if (bytes(json) > s.limits.outputBytes) {
        throw new NativeError("output limit exceeded");
      }
      result = record(["output", parse(json), "logs", logs]);
    }
  } catch (e) {
    exception = record([
      "name",
      NativeString(e?.name ?? "Error"),
      "message",
      NativeString(e?.message ?? e),
    ]);
  }
  s.open = false;
  // No new calls may enter while accepted calls drain. Replies resolve the
  // original invocation's promises, never a mutable current-callback slot.
  while (mapSize(s.pending) > 0) {
    await new NativePromise((resolve) => {
      s.drained = resolve;
    });
  }
  // Serialization can run user toJSON hooks. Finish it before resource checks
  // and send the resulting bytes without re-serializing them afterwards.
  let resultJSON;
  try {
    resultJSON = stringify(result);
  } catch (e) {
    resultJSON = '{"undefined":true,"logs":[]}';
    exception = record([
      "name", NativeString(e?.name ?? "Error"),
      "message", NativeString(e?.message ?? e),
    ]);
  }
  // Allow settled promise continuations to run before examining resources.
  await new NativePromise((resolve) => nativeSetTimeout(resolve, 0));
  // Native leak-trace kind 2 is a timer in the pinned Deno runtime.
  let terminal = s.dirty || mapSize(timersOpen) > 0 ||
    getActiveResourcesInfo().length > 0 ||
    mapGet(leakTraces(), 2) !== undefined;
  if (terminal && !exception) {
    exception = record([
      "name",
      "ResourceError",
      "message",
      "background resources prevent session reuse",
    ]);
  }
  if (logOverflow) {
    exception = record(["name", "RangeError", "message", "log limit exceeded"]);
    terminal = true;
  }
  scope = null;
  send('{"type":"done","id":' + stringify(s.id) + ',"result":' + resultJSON +
    ',"terminal":' + (terminal ? "true" : "false") +
    (exception ? ',"exception":' + stringify(exception) : "") + '}');
}

globalThis.onmessage = function (event) {
  port = eventData(event);
  send = port.postMessage.bind(port);
  globalThis.onmessage = null;
  port.onmessage = function (event) {
    const m = setProto(parse(eventData(event)), null);
    if (m.type === "execute") {
      if (scope) throw new NativeError("overlapping invocation");
      execute(m).catch(() => close());
    } else if (m.type === "reply") {
      const s = scope;
      if (!s || m.id !== s.id) throw new NativeError("reply scope mismatch");
      const p = mapGet(s.pending, m.call);
      if (!p) throw new NativeError("unknown reply");
      mapDelete(s.pending, m.call);
      if (m.error) p.reject(new NativeError(m.error));
      else p.resolve(m.value);
      if (mapSize(s.pending) === 0 && s.drained) s.drained();
    } else throw new NativeError("unknown controller message");
  };
  emit(["type", "ready"]);
};
