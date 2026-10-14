// Tests for build.mjs: node --test npm/build.test.mjs npm/launcher.test.mjs
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { crc32, deflateRawSync, gzipSync } from "node:zlib";
import { build, fromTarGz, fromZip, PLATFORMS } from "./build.mjs";

// tarGz builds a .tar.gz holding files ({name: Buffer}), the way GoReleaser's
// archives hold ghostbranch, LICENSE and README.md.
function tarGz(files) {
  const blocks = [];
  for (const [name, data] of Object.entries(files)) {
    const h = Buffer.alloc(512);
    h.write(name, 0, "latin1");
    h.write("0000755\0", 100, "latin1");
    h.write("0000000\0", 108, "latin1");
    h.write("0000000\0", 116, "latin1");
    h.write(data.length.toString(8).padStart(11, "0") + "\0", 124, "latin1");
    h.write("00000000000\0", 136, "latin1");
    h.write("        ", 148, "latin1");
    h.write("0", 156, "latin1");
    h.write("ustar\0" + "00", 257, "latin1");
    let sum = 0;
    for (const b of h) sum += b;
    h.write(sum.toString(8).padStart(6, "0") + "\0 ", 148, "latin1");
    blocks.push(h, data, Buffer.alloc((512 - (data.length % 512)) % 512));
  }
  blocks.push(Buffer.alloc(1024));
  return gzipSync(Buffer.concat(blocks));
}

// zip builds a .zip holding files, deflated like Go's archive/zip.
function zip(files) {
  const locals = [];
  const central = [];
  let off = 0;
  for (const [name, data] of Object.entries(files)) {
    const body = deflateRawSync(data);
    const n = Buffer.from(name);
    const l = Buffer.alloc(30);
    l.writeUInt32LE(0x04034b50, 0);
    l.writeUInt16LE(20, 4);
    l.writeUInt16LE(8, 8);
    l.writeUInt32LE(crc32(data), 14);
    l.writeUInt32LE(body.length, 18);
    l.writeUInt32LE(data.length, 22);
    l.writeUInt16LE(n.length, 26);
    const c = Buffer.alloc(46);
    c.writeUInt32LE(0x02014b50, 0);
    c.writeUInt16LE(20, 4);
    c.writeUInt16LE(20, 6);
    c.writeUInt16LE(8, 10);
    c.writeUInt32LE(crc32(data), 16);
    c.writeUInt32LE(body.length, 20);
    c.writeUInt32LE(data.length, 24);
    c.writeUInt16LE(n.length, 28);
    c.writeUInt32LE(off, 42);
    locals.push(l, n, body);
    central.push(c, n);
    off += 30 + n.length + body.length;
  }
  const dir = Buffer.concat(central);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(Object.keys(files).length, 8);
  end.writeUInt16LE(Object.keys(files).length, 10);
  end.writeUInt32LE(dir.length, 12);
  end.writeUInt32LE(off, 16);
  return Buffer.concat([...locals, dir, end]);
}

// fixture writes the five archives of a fake 1.2.3 release, each binary
// naming its platform, their checksums.txt (GoReleaser's "<sha256>  <name>"
// lines, the install scripts listed too), plus a LICENSE in a fake repo root.
function fixture() {
  const dir = mkdtempSync(join(tmpdir(), "ghostbranch-npm-"));
  const sums = [];
  for (const p of PLATFORMS) {
    const files = { [p.bin]: Buffer.from(`binary for ${p.target}`), LICENSE: Buffer.from("license"), "README.md": Buffer.from("readme") };
    const name = `ghostbranch_1.2.3_${p.target}${p.ext}`;
    const data = p.ext === ".zip" ? zip(files) : tarGz(files);
    writeFileSync(join(dir, name), data);
    sums.push(`${createHash("sha256").update(data).digest("hex")}  ${name}`);
  }
  sums.push(`${"0".repeat(64)}  install.sh`, `${"1".repeat(64)}  install.ps1`);
  writeFileSync(join(dir, "checksums.txt"), sums.join("\n") + "\n");
  writeFileSync(join(dir, "LICENSE"), "Apache License 2.0");
  return dir;
}

// editSums rewrites the fixture's checksums.txt line by line.
function editSums(dir, f) {
  const path = join(dir, "checksums.txt");
  writeFileSync(path, f(readFileSync(path, "utf8").trimEnd().split("\n")).join("\n") + "\n");
}

test("the archive readers find the binary", () => {
  const files = { ghostbranch: Buffer.from("bin"), LICENSE: Buffer.from("l") };
  assert.equal(fromTarGz(tarGz(files), "ghostbranch").toString(), "bin");
  assert.equal(fromZip(zip(files), "ghostbranch").toString(), "bin");
  assert.throws(() => fromTarGz(tarGz(files), "nope"), /isn't in the archive/);
  assert.throws(() => fromZip(zip(files), "nope"), /isn't in the archive/);
});

test("build writes six packages at one version", (t) => {
  const dir = fixture();
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const out = join(dir, "out");
  const made = build({ version: "v1.2.3", assets: dir, out, root: dir });
  assert.deepEqual(made, ["cli-darwin-arm64", "cli-darwin-x64", "cli-linux-arm64", "cli-linux-x64", "cli-win32-x64", "ghostbranch"]);

  for (const p of PLATFORMS) {
    const pkgDir = join(out, `cli-${p.npm}`);
    const pkg = JSON.parse(readFileSync(join(pkgDir, "package.json"), "utf8"));
    assert.equal(pkg.name, `@ghostbranch/cli-${p.npm}`);
    assert.equal(pkg.version, "1.2.3");
    assert.deepEqual(pkg.os, [p.os]);
    assert.deepEqual(pkg.cpu, [p.cpu]);
    assert.equal(pkg.repository.url, "git+https://github.com/ghostbranch-labs/ghostbranch.git");
    assert.equal(readFileSync(join(pkgDir, "bin", p.bin), "utf8"), `binary for ${p.target}`);
    if (process.platform !== "win32") assert.ok(statSync(join(pkgDir, "bin", p.bin)).mode & 0o100, `${p.bin} isn't executable`);
    assert.equal(readFileSync(join(pkgDir, "LICENSE"), "utf8"), "Apache License 2.0");
  }

  const main = JSON.parse(readFileSync(join(out, "ghostbranch", "package.json"), "utf8"));
  assert.equal(main.name, "ghostbranch");
  assert.equal(main.version, "1.2.3");
  assert.deepEqual(main.bin, { ghostbranch: "bin/ghostbranch.js" });
  assert.deepEqual(Object.keys(main.optionalDependencies).sort(), PLATFORMS.map((p) => `@ghostbranch/cli-${p.npm}`).sort());
  for (const v of Object.values(main.optionalDependencies)) assert.equal(v, "1.2.3");
  assert.equal(main.scripts, undefined, "the main package must have no install script");
  assert.ok(readFileSync(join(out, "ghostbranch", "bin", "ghostbranch.js"), "utf8").includes("spawnSync"));
});

test("build refuses a bad version or a missing archive", (t) => {
  const dir = fixture();
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  assert.throws(() => build({ version: "latest", assets: dir, out: join(dir, "o1"), root: dir }), /isn't X.Y.Z/);
  rmSync(join(dir, "ghostbranch_1.2.3_linux_arm64.tar.gz"));
  assert.throws(() => build({ version: "1.2.3", assets: dir, out: join(dir, "o2"), root: dir }), /linux_arm64/);
});

test("build checks each archive against checksums.txt before packing", (t) => {
  const linux = "ghostbranch_1.2.3_linux_arm64.tar.gz";
  for (const [what, edit, want] of [
    ["a mismatch", (dir) => editSums(dir, (ls) => ls.map((l) => (l.endsWith(`  ${linux}`) ? `${"f".repeat(64)}  ${linux}` : l))), /checksum mismatch for ghostbranch_1\.2\.3_linux_arm64\.tar\.gz/],
    ["a missing line", (dir) => editSums(dir, (ls) => ls.filter((l) => !l.endsWith(`  ${linux}`))), /checksums\.txt has no line for ghostbranch_1\.2\.3_linux_arm64\.tar\.gz/],
    ["a doubled line", (dir) => editSums(dir, (ls) => [...ls, ls.find((l) => l.endsWith(`  ${linux}`))]), /checksums\.txt has 2 lines for ghostbranch_1\.2\.3_linux_arm64\.tar\.gz/],
    ["no checksums.txt", (dir) => rmSync(join(dir, "checksums.txt")), /checksums\.txt/],
  ]) {
    const dir = fixture();
    t.after(() => rmSync(dir, { recursive: true, force: true }));
    edit(dir);
    const out = join(dir, "out");
    assert.throws(() => build({ version: "1.2.3", assets: dir, out, root: dir }), want, what);
    assert.throws(() => statSync(out), /ENOENT/, `${what}: build wrote packages before failing`);
  }
});
