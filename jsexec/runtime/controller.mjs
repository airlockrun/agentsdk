// This isolate never evaluates snippets. Its only network grant is the Go
// supervisor's Unix socket; every snippet runs in a permission-denied worker.
// Deno 2.9.6 lazily starts its process-wide signal thread after 500 ms for
// SIGUSR1 inspector activation. Initialize it before ready so the supervisor's
// thread baseline includes runtime infrastructure, not just short-lived tasks.
const signalWarmup = () => {};
Deno.addSignalListener("SIGUSR1", signalWarmup);
Deno.removeSignalListener("SIGUSR1", signalWarmup);
const conn = await Deno.connect({ transport: "unix", path: Deno.args[0] });
const worker = new Worker(new URL("./worker.mjs", import.meta.url), {
  type: "module",
  deno: { permissions: "none" },
});
const channel = new MessageChannel();
const port = channel.port1;
worker.postMessage(channel.port2, [channel.port2]);
const encoder = new TextEncoder(), decoder = new TextDecoder();
let writing = Promise.resolve();
let active = null;
let limit = 1048576;
function send(message) {
  const body = encoder.encode(JSON.stringify(message));
  if (body.length > limit) throw new Error("controller frame limit");
  const bytes = new Uint8Array(body.length + 4);
  new DataView(bytes.buffer).setUint32(0, body.length);
  bytes.set(body, 4);
  writing = writing.then(async () => {
    let offset = 0;
    while (offset < bytes.length) {
      offset += await conn.write(bytes.subarray(offset));
    }
  });
  writing.catch(() => Deno.exit(1));
}
worker.onerror = () => Deno.exit(1);
// The default worker channel is deliberately not a control channel.
worker.onmessage = () => {};
port.onmessage = ({ data }) => {
  if (typeof data !== "string" || data.length > limit) Deno.exit(1);
  const m = JSON.parse(data);
  if (m.type === "ready" && active === null) {
    send(m);
    return;
  }
  if (!active || m.id !== active.id) Deno.exit(1);
  if (m.type === "call") {
    if (
      active.calls.has(m.call) ||
      active.calls.size >= active.options.limits.calls ||
      active.pending.size >= active.options.limits.concurrentCalls
    ) Deno.exit(1);
    active.calls.add(m.call);
    active.pending.add(m.call);
    send({
      type: "call",
      id: m.id,
      call: m.call,
      name: m.name,
      args: JSON.parse(m.args),
    });
  } else if (m.type === "done") {
    if (active.pending.size !== 0) Deno.exit(1);
    send(m);
    active = null;
  } else Deno.exit(1);
};
async function readExact(n) {
  const bytes = new Uint8Array(n);
  let offset = 0;
  while (offset < n) {
    const count = await conn.read(bytes.subarray(offset));
    if (count === null) Deno.exit(0);
    offset += count;
  }
  return bytes;
}
while (true) {
  const header = await readExact(4);
  const size = new DataView(header.buffer).getUint32(0);
  if (size < 2 || size > limit) Deno.exit(1);
  const raw = decoder.decode(await readExact(size));
  const m = JSON.parse(raw);
  if (m.type === "execute") {
    if (active) Deno.exit(1);
    active = {
      id: m.id,
      options: m.options,
      pending: new Set(),
      calls: new Set(),
    };
    limit = m.options.limits.frameBytes;
  } else if (m.type === "reply") {
    if (!active || m.id !== active.id || !active.pending.delete(m.call)) {
      Deno.exit(1);
    }
  } else Deno.exit(1);
  port.postMessage(raw);
}
