import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { EventEmitter } from "node:events";
import { mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const { expectedChecksum, forwardSignals, targetFor } = await import("../bin/runneryard.mjs");
const run = promisify(execFile);

test("maps supported release targets", () => {
  assert.equal(targetFor("darwin", "arm64"), "Darwin_arm64");
  assert.equal(targetFor("linux", "x64"), "Linux_x86_64");
  assert.throws(() => targetFor("win32", "x64"), /does not publish/);
});

test("selects the checksum for the requested asset", () => {
  assert.equal(expectedChecksum("abc  one.tar.gz\ndef  two.tar.gz\n", "two.tar.gz"), "def");
  assert.throws(() => expectedChecksum("abc  one.tar.gz", "missing"), /do not contain/);
});

test("forwards supervisor termination signals to the controller", () => {
  const host = new EventEmitter();
  const child = new EventEmitter();
  const received = [];
  child.kill = (signal) => received.push(signal);
  forwardSignals(child, host);
  host.emit("SIGTERM");
  assert.deepEqual(received, ["SIGTERM"]);
  child.emit("exit", 0, null);
  host.emit("SIGINT");
  assert.deepEqual(received, ["SIGTERM"]);
});

test("starts when invoked through an npm-style bin symlink", async (context) => {
  const directory = await mkdtemp(join(tmpdir(), "runneryard-cli-test-"));
  context.after(() => rm(directory, { recursive: true, force: true }));
  const launcher = join(directory, "runneryard");
  const fakeBinary = join(directory, "fake-binary.mjs");
  const packageBinary = fileURLToPath(new URL("../bin/runneryard.mjs", import.meta.url));

  await writeFile(fakeBinary, '#!/usr/bin/env node\nconsole.log(`launched:${process.argv.slice(2).join(",")}`);\n', { mode: 0o755 });
  await symlink(packageBinary, launcher);

  const { stdout } = await run(launcher, ["version"], {
    env: { ...process.env, RUNNERYARD_BINARY: fakeBinary },
  });
  assert.equal(stdout, "launched:version\n");
});
