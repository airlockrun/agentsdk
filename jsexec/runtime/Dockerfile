FROM denoland/deno@sha256:2014dc167ece617ef7e7ba40631ac2234c59e75ce693e7cc2dc2602b3c87859d
COPY jsexecutor /usr/local/bin/jsexecutor
COPY controller.mjs worker.mjs /opt/jsexec/
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/jsexecutor"]
CMD []
