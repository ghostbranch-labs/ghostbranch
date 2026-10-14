#!/usr/bin/env node
// Runs the ghostbranch binary from this platform's package
// (@ghostbranch/cli-<os>-<arch>), where npm installed it inside
// node_modules, so `ghostbranch` shows the right update command.
"use strict";
const { spawnSync } = require("child_process");

const PACKAGES = {
  "darwin arm64": "@ghostbranch/cli-darwin-arm64",
  "darwin x64": "@ghostbranch/cli-darwin-x64",
  "linux arm64": "@ghostbranch/cli-linux-arm64",
  "linux x64": "@ghostbranch/cli-linux-x64",
  "win32 x64": "@ghostbranch/cli-win32-x64",
};

function binaryPath(platform, arch) {
  const pkg = PACKAGES[`${platform} ${arch}`];
  if (!pkg) {
    throw new Error(`there is no ghostbranch build for ${platform} ${arch}. Supported: ${Object.keys(PACKAGES).join(", ")}.` +
      (platform === "win32" ? " On Windows on ARM, use the install script, which installs the amd64 build: https://ghostbranch.site" : ""));
  }
  const file = `${pkg}/bin/ghostbranch${platform === "win32" ? ".exe" : ""}`;
  try {
    return require.resolve(file);
  } catch {
    throw new Error(`the package ${pkg} isn't installed. It holds the ghostbranch binary for ${platform} ${arch}, and npm skips it when optional dependencies are turned off (--omit=optional or --no-optional). Reinstall with them on: npm install -g ghostbranch`);
  }
}

function main() {
  let bin;
  try {
    bin = binaryPath(process.platform, process.arch);
  } catch (e) {
    console.error(`ghostbranch: ${e.message}`);
    return process.exit(1);
  }
  // Ctrl-C reaches the binary from the terminal; wait for it to finish
  // instead of dying first.
  process.on("SIGINT", () => {});
  const r = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
  if (r.error) {
    console.error(`ghostbranch: couldn't run ${bin}: ${r.error.message}`);
    return process.exit(1);
  }
  if (r.signal) {
    process.removeAllListeners("SIGINT");
    process.kill(process.pid, r.signal);
  }
  process.exit(r.status ?? 1);
}

if (require.main === module) main();
module.exports = { PACKAGES, binaryPath };
