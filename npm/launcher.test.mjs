// Tests for the launcher, npm/ghostbranch/bin/ghostbranch.js.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { copyFileSync, linkSync, mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const launcherSrc = join(here, "ghostbranch", "bin", "ghostbranch.js");
const { PACKAGES, binaryPath } = createRequire(import.meta.url)(launcherSrc);
const hostPkg = PACKAGES[`${process.platform} ${process.arch}`];
const win = process.platform === "win32";
const skipUnsupported = !hostPkg && "not a supported platform";

// install lays out node_modules the way npm does: the launcher package and,
// unless withPlatform is false, this platform's package. Its "binary" is a
// real file (a hard link to Node, or a copy where linking fails), so the
// launcher runs it where it sits. A test drives it with -e, and the child can
// report process.argv0, the path the launcher gave to spawn. It returns the
// launcher and the path the binary should be run from.
function install(t, withPlatform = true) {
  const root = mkdtempSync(join(tmpdir(), "ghostbranch-launcher-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const launcher = join(root, "node_modules", "ghostbranch", "bin", "ghostbranch.js");
  mkdirSync(dirname(launcher), { recursive: true });
  copyFileSync(launcherSrc, launcher);
  const bin = hostPkg ? join(root, "node_modules", hostPkg, "bin", `ghostbranch${win ? ".exe" : ""}`) : "";
  if (withPlatform && hostPkg) {
    mkdirSync(dirname(bin), { recursive: true });
    writeFileSync(join(root, "node_modules", hostPkg, "package.json"), JSON.stringify({ name: hostPkg, version: "0.0.0" }));
    try {
      linkSync(process.execPath, bin);
    } catch {
      copyFileSync(process.execPath, bin);
    }
  }
  return { launcher, bin };
}

const run = (launcher, ...args) => spawnSync(process.execPath, [launcher, ...args], { encoding: "utf8" });

// The "binary" prints what it was started as, and the arguments it got.
const report = "console.log(JSON.stringify({ ran: process.argv0, args: process.argv.slice(1) }))";

test("this platform maps to its @ghostbranch/cli-<os>-<arch> package", { skip: skipUnsupported }, () => {
  assert.equal(hostPkg, `@ghostbranch/cli-${process.platform}-${process.arch}`);
  assert.deepEqual(Object.values(PACKAGES).sort(), [
    "@ghostbranch/cli-darwin-arm64",
    "@ghostbranch/cli-darwin-x64",
    "@ghostbranch/cli-linux-arm64",
    "@ghostbranch/cli-linux-x64",
    "@ghostbranch/cli-win32-x64",
  ]);
});

test("the launcher runs that package's binary and passes arguments and the exit code through", { skip: skipUnsupported }, (t) => {
  const { launcher, bin } = install(t);
  const r = run(launcher, "-e", `${report}; process.exit(7)`, "--", "a b", "--json");
  assert.equal(r.status, 7, r.stderr);
  const got = JSON.parse(r.stdout);
  assert.equal(realpathSync(got.ran), realpathSync(bin), "the launcher ran a different binary");
  assert.deepEqual(got.args, ["a b", "--json"]);
});

test("the binary runs from inside node_modules, not from a copy elsewhere", { skip: skipUnsupported }, (t) => {
  const { launcher } = install(t);
  const r = run(launcher, "-e", report);
  assert.equal(r.status, 0, r.stderr);
  const { ran } = JSON.parse(r.stdout);
  assert.notEqual(ran, process.execPath, "the launcher ran the Node that runs the test, not the package binary");
  assert.match(ran.replaceAll("\\", "/"), /\/node_modules\/@ghostbranch\/cli-[a-z0-9-]+\/bin\/ghostbranch(\.exe)?$/);
});

test("a signal that ends the binary ends the launcher the same way", { skip: (win || !hostPkg) && "no POSIX signals here" }, (t) => {
  const { launcher } = install(t);
  const r = run(launcher, "-e", "process.kill(process.pid, 'SIGTERM')");
  assert.equal(r.signal, "SIGTERM", `status ${r.status} ${r.stderr}`);
});

test("a missing platform package says which one and how to fix it", { skip: skipUnsupported }, (t) => {
  const { launcher } = install(t, false);
  const r = run(launcher, "version");
  assert.equal(r.status, 1);
  assert.match(r.stderr, new RegExp(`the package ${hostPkg.replace("/", "\\/")} isn't installed`));
  assert.match(r.stderr, /--omit=optional/);
  assert.match(r.stderr, /npm install -g ghostbranch/);
});

test("an unsupported platform lists the supported ones", () => {
  assert.throws(() => binaryPath("freebsd", "x64"), /no ghostbranch build for freebsd x64\. Supported: darwin arm64, darwin x64, linux arm64, linux x64, win32 x64\./);
  assert.throws(() => binaryPath("win32", "arm64"), /install script, which installs the amd64 build/);
});
