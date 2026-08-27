import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import test from "node:test";

const { expectedChecksum, forwardSignals, targetFor } = await import("../bin/runneryard.mjs");

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
