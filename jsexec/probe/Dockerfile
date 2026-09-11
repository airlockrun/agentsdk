FROM denoland/deno@sha256:2014dc167ece617ef7e7ba40631ac2234c59e75ce693e7cc2dc2602b3c87859d
COPY feasibility.mjs /probe/feasibility.mjs
COPY local.mjs /probe/local.mjs
USER 65532:65532
ENV DENO_DIR=/tmp/deno
ENTRYPOINT ["/usr/bin/deno", "run", "--no-config", "--no-lock", "--no-remote", "--no-npm", "--no-code-cache", "--no-prompt", "--deny-read", "--deny-write", "--deny-net", "--deny-env", "--deny-sys", "--deny-run", "--deny-ffi", "--deny-import", "/probe/feasibility.mjs"]
