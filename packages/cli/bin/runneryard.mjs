#!/usr/bin/env node

import { createHash } from "node:crypto";
import { chmod, mkdir, readFile, rename, rm, writeFile } from "node:fs/promises";
import { homedir, tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { spawn } from "node:child_process";

const packageRoot = dirname(dirname(fileURLToPath(import.meta.url)));
const packageData = JSON.parse(await readFile(join(packageRoot, "package.json"), "utf8"));
const version = packageData.version;
const repository = process.env.RUNNERYARD_REPOSITORY || "gwendall/runneryard";

function targetFor(platform = process.platform, arch = process.arch) {
  const platforms = { darwin: "Darwin", linux: "Linux" };
  const arches = { x64: "x86_64", arm64: "arm64" };
  if (!platforms[platform] || !arches[arch]) {
    throw new Error(`runneryard does not publish a binary for ${platform}/${arch}`);
  }
  return `${platforms[platform]}_${arches[arch]}`;
}

async function download(url) {
  const response = await fetch(url, { redirect: "follow" });
  if (!response.ok) {
    throw new Error(`download failed (${response.status}) from ${url}`);
  }
  return Buffer.from(await response.arrayBuffer());
}

function expectedChecksum(checksums, asset) {
  for (const line of checksums.split("\n")) {
    const [checksum, filename] = line.trim().split(/\s+/, 2);
    if (filename?.replace(/^\*/, "") === asset) return checksum;
  }
  throw new Error(`release checksums do not contain ${asset}`);
}

async function installBinary() {
  if (process.env.RUNNERYARD_BINARY) return process.env.RUNNERYARD_BINARY;
  const target = targetFor();
  const cacheDir = join(homedir(), ".cache", "runneryard", version, target);
  const binary = join(cacheDir, "runneryard");
  try {
    await chmod(binary, 0o755);
    return binary;
  } catch {}

  await mkdir(cacheDir, { recursive: true });
  const base = `https://github.com/${repository}/releases/download/v${version}`;
  const asset = `runneryard_${version}_${target}.tar.gz`;
  const [archive, sums] = await Promise.all([
    download(`${base}/${asset}`),
    download(`${base}/runneryard_${version}_checksums.txt`),
  ]);
  const actual = createHash("sha256").update(archive).digest("hex");
  const expected = expectedChecksum(sums.toString("utf8"), asset);
  if (actual !== expected) throw new Error(`checksum mismatch for ${asset}`);

  const staging = join(tmpdir(), `runneryard-${process.pid}-${Date.now()}`);
  const archivePath = `${staging}.tar.gz`;
  await writeFile(archivePath, archive);
  await mkdir(staging, { recursive: true });
  await new Promise((resolve, reject) => {
    const child = spawn("tar", ["-xzf", archivePath, "-C", staging], { stdio: "inherit" });
    child.once("error", reject);
    child.once("exit", (code) => (code === 0 ? resolve() : reject(new Error(`tar exited with ${code}`))));
  });
  await chmod(join(staging, "runneryard"), 0o755);
  await rename(join(staging, "runneryard"), binary);
  await rm(staging, { recursive: true, force: true });
  await rm(archivePath, { force: true });
  return binary;
}

function forwardSignals(child, host = process) {
  const handlers = new Map();
  for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) {
    const handler = () => child.kill(signal);
    handlers.set(signal, handler);
    host.on(signal, handler);
  }
  const cleanup = () => {
    for (const [signal, handler] of handlers) host.off(signal, handler);
  };
  child.once("error", cleanup);
  child.once("exit", cleanup);
  return cleanup;
}

async function main() {
  try {
    const binary = await installBinary();
    const child = spawn(binary, process.argv.slice(2), { stdio: "inherit" });
    forwardSignals(child);
    child.once("error", (error) => {
      console.error(`runneryard: ${error.message}`);
      process.exitCode = 1;
    });
    child.once("exit", (code, signal) => {
      if (signal) process.kill(process.pid, signal);
      else process.exitCode = code ?? 1;
    });
  } catch (error) {
    console.error(`runneryard: ${error.message}`);
    process.exitCode = 1;
  }
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  await main();
}

export { expectedChecksum, forwardSignals, main, targetFor };
