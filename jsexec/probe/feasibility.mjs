// Diagnostic only. Keep the runtime globals intact so permissions, not a
// deleted-global shim or source-text filter, determine each outcome.
const write = Deno.stdout.write.bind(Deno.stdout);
const encode = new TextEncoder().encode.bind(new TextEncoder());
const stringify = JSON.stringify;
const AsyncFunction = (async function () {}).constructor;

const probes = [
  ["async_retained_state", async () => {
    await new AsyncFunction("globalThis.retained = await Promise.resolve(41)")();
    return await new AsyncFunction("return ++globalThis.retained")();
  }],
  ["network", () => fetch("http://127.0.0.1:8080/")],
  ["filesystem_read", () => Deno.readTextFile("/etc/passwd")],
  ["filesystem_write", () => Deno.writeTextFile("/tmp/probe", "x")],
  ["environment", () => Deno.env.get("PATH")],
  ["subprocess", () => new Deno.Command("/bin/sh", { args: ["-c", "true"] }).output()],
  ["ffi", () => Deno.dlopen("/does-not-exist.so", {})],
  // Imports are compiled at snippet execution, not included in the trusted
  // entry module's dependency graph during Deno startup.
  ["remote_import", new AsyncFunction(`return await import("https://example.com/probe.mjs")`)],
  ["computed_remote_import", new AsyncFunction(`return await import(["https:", "", "example.com", "probe.mjs"].join("/"))`)],
  ["file_import", new AsyncFunction(`return (await import("file:///probe/local.mjs")).default`)],
  ["npm_import", new AsyncFunction(`return await import("npm:is-number@7.0.0")`)],
  ["data_import", new AsyncFunction(`return (await import("data:text/javascript,export default 42")).default`)],
  ["computed_data_import", new AsyncFunction(`return (await import(["da", "ta:text/javascript,export default 42"].join(""))).default`)],
  ["node_import", new AsyncFunction(`return typeof (await import("node:fs")).readFileSync`)],
  ["node_filesystem_read", new AsyncFunction(`return (await import("node:fs")).readFileSync("/etc/passwd", "utf8")`)],
  ["function_import", () => Function("return import('data:text/javascript,export default 42').then(m => m.default)")()],
  ["eval_import", () => (0, eval)("import('data:text/javascript,export default 42').then(m => m.default)")],
  ["worker_data_import", () => new Promise((resolve, reject) => {
    const worker = new Worker("data:text/javascript,postMessage(42)", { type: "module" });
    const timer = setTimeout(() => { worker.terminate(); reject(new Error("worker timeout")); }, 2000);
    worker.onmessage = ({ data }) => { clearTimeout(timer); worker.terminate(); resolve(data); };
    worker.onerror = (event) => { event.preventDefault(); clearTimeout(timer); worker.terminate(); reject(new Error(event.message)); };
  })],
  ["bootstrap_global_tamper", () => {
    // This tests same-realm mutability, not an escape from Deno permissions.
    const original = JSON.stringify;
    JSON.stringify = () => "tampered";
    const result = JSON.stringify({});
    JSON.stringify = original;
    return result;
  }],
];

for (const [name, fn] of probes) {
  let observation;
  try {
    observation = { name, allowed: true, value: await fn() };
  } catch (error) {
    observation = { name, allowed: false, error: String(error) };
  }
  const bytes = encode(stringify(observation) + "\n");
  let offset = 0;
  while (offset < bytes.length) offset += await write(bytes.subarray(offset));
}
