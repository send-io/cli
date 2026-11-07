#!/usr/bin/env node
// Shim for the sendio CLI. The real binary ships in a per-platform package
// (@sendio/cli-<platform>-<arch>) that npm installs as an optional
// dependency; this file finds it and runs it with the same args, stdio and
// exit code.
"use strict";

const { spawnSync } = require("child_process");
const fs = require("fs");

const platform = process.platform; // darwin | linux | win32
const arch = process.arch; // x64 | arm64
const pkg = `@sendio/cli-${platform}-${arch}`;
const exe = platform === "win32" ? "sendio.exe" : "sendio";

function findBinary() {
  // Escape hatch for unusual setups (and for testing).
  if (process.env.SENDIO_BINARY_PATH) return process.env.SENDIO_BINARY_PATH;
  try {
    return require.resolve(`${pkg}/bin/${exe}`);
  } catch (_) {
    return null;
  }
}

const bin = findBinary();
if (!bin) {
  const supported = ["darwin-x64", "darwin-arm64", "linux-x64", "linux-arm64", "win32-x64", "win32-arm64"];
  const lines = [
    `sendio: the platform package ${pkg} is not installed.`,
    "",
  ];
  if (!supported.includes(`${platform}-${arch}`)) {
    lines.push(`Your platform (${platform}-${arch}) is not supported by the npm package.`);
    lines.push("Supported: " + supported.join(", "));
  } else {
    lines.push("npm skips optional dependencies when installing with --no-optional /");
    lines.push("--omit=optional, or when node_modules was copied from another OS/CPU.");
    lines.push("Reinstall without those flags, e.g.:");
    lines.push("  npm install -g @sendio/cli");
  }
  lines.push("");
  lines.push("Other install options: https://app.send.io/docs/cli (curl -fsSL https://app.send.io/install.sh | sh)");
  process.stderr.write(lines.join("\n") + "\n");
  process.exit(1);
}

// Some package managers drop the executable bit when unpacking.
if (platform !== "win32") {
  try {
    fs.accessSync(bin, fs.constants.X_OK);
  } catch (_) {
    try {
      fs.chmodSync(bin, 0o755);
    } catch (_) {
      // fall through; spawn will report the real error
    }
  }
}

const result = spawnSync(bin, process.argv.slice(2), {
  stdio: "inherit",
  windowsHide: false,
});

if (result.error) {
  process.stderr.write(`sendio: failed to run ${bin}: ${result.error.message}\n`);
  process.exit(1);
}
if (result.signal) {
  // Re-raise so the parent shell sees the same signal.
  process.kill(process.pid, result.signal);
}
process.exit(result.status === null ? 1 : result.status);
