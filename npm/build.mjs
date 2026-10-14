// Builds the six npm packages for a release from its archives. No
// dependencies. The publish workflow runs it; see RELEASING.md.
//
//   node npm/build.mjs --version v0.1.0 --assets <folder with the archives and checksums.txt> --out <folder>
//
// It writes <out>/ghostbranch (the launcher package) and one
// <out>/cli-<os>-<arch> folder per platform, holding that platform's binary.
import { createHash } from "node:crypto";
import { chmodSync, copyFileSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { gunzipSync, inflateRawSync } from "node:zlib";

const here = dirname(fileURLToPath(import.meta.url));

// PLATFORMS maps each npm platform package to its GoReleaser target.
export const PLATFORMS = [
  { npm: "darwin-arm64", os: "darwin", cpu: "arm64", target: "darwin_arm64", ext: ".tar.gz", bin: "ghostbranch" },
  { npm: "darwin-x64", os: "darwin", cpu: "x64", target: "darwin_amd64", ext: ".tar.gz", bin: "ghostbranch" },
  { npm: "linux-arm64", os: "linux", cpu: "arm64", target: "linux_arm64", ext: ".tar.gz", bin: "ghostbranch" },
  { npm: "linux-x64", os: "linux", cpu: "x64", target: "linux_amd64", ext: ".tar.gz", bin: "ghostbranch" },
  { npm: "win32-x64", os: "win32", cpu: "x64", target: "windows_amd64", ext: ".zip", bin: "ghostbranch.exe" },
];

const VERSION = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$/;

// fromTarGz returns the regular file called name from a .tar.gz archive.
export function fromTarGz(buf, name) {
  const tar = gunzipSync(buf);
  for (let off = 0; off + 512 <= tar.length; ) {
    const header = tar.subarray(off, off + 512);
    if (header.every((b) => b === 0)) break;
    const field = (start, len) => header.subarray(start, start + len).toString("latin1").replace(/\0.*$/s, "");
    const prefix = field(345, 155);
    const path = prefix ? `${prefix}/${field(0, 100)}` : field(0, 100);
    const size = parseInt(field(124, 12).trim() || "0", 8);
    const type = field(156, 1);
    if (path === name && (type === "0" || type === "")) return tar.subarray(off + 512, off + 512 + size);
    off += 512 + Math.ceil(size / 512) * 512;
  }
  throw new Error(`${name} isn't in the archive`);
}

// fromZip returns the file called name from a .zip archive.
export function fromZip(buf, name) {
  let eocd = -1;
  for (let i = buf.length - 22; i >= Math.max(0, buf.length - 22 - 65535); i--) {
    if (buf.readUInt32LE(i) === 0x06054b50) {
      eocd = i;
      break;
    }
  }
  if (eocd < 0) throw new Error("not a zip archive");
  const count = buf.readUInt16LE(eocd + 10);
  let off = buf.readUInt32LE(eocd + 16);
  for (let n = 0; n < count; n++) {
    if (buf.readUInt32LE(off) !== 0x02014b50) throw new Error("a broken zip directory");
    const method = buf.readUInt16LE(off + 10);
    const compressed = buf.readUInt32LE(off + 20);
    const nameLen = buf.readUInt16LE(off + 28);
    const extraLen = buf.readUInt16LE(off + 30);
    const commentLen = buf.readUInt16LE(off + 32);
    const local = buf.readUInt32LE(off + 42);
    const entry = buf.subarray(off + 46, off + 46 + nameLen).toString("utf8");
    if (entry === name) {
      const start = local + 30 + buf.readUInt16LE(local + 26) + buf.readUInt16LE(local + 28);
      const data = buf.subarray(start, start + compressed);
      if (method === 0) return data;
      if (method === 8) return inflateRawSync(data);
      throw new Error(`${name} uses zip method ${method}`);
    }
    off += 46 + nameLen + extraLen + commentLen;
  }
  throw new Error(`${name} isn't in the archive`);
}

// build writes the six package folders for version from the archives in assets.
export function build({ version, assets, out, root = join(here, "..") }) {
  version = String(version ?? "").replace(/^v/, "");
  if (!VERSION.test(version)) throw new Error(`version "${version}" isn't X.Y.Z or X.Y.Z-pre`);
  const license = join(root, "LICENSE");
  const template = readFileSync(join(here, "platform", "package.json.tmpl"), "utf8");
  // Every archive is checked against the release's checksums.txt before
  // anything is written, so npm never packs an archive the release didn't list.
  const sums = readFileSync(join(assets, "checksums.txt"), "utf8").split(/\r?\n/).filter(Boolean).map((l) => l.split(/\s+/));
  const archives = PLATFORMS.map((p) => {
    const name = `ghostbranch_${version}_${p.target}${p.ext}`;
    const data = readFileSync(join(assets, name));
    const lines = sums.filter((f) => f.length === 2 && f[1] === name);
    if (lines.length === 0) throw new Error(`checksums.txt has no line for ${name}`);
    if (lines.length > 1) throw new Error(`checksums.txt has ${lines.length} lines for ${name}`);
    const got = createHash("sha256").update(data).digest("hex");
    if (got !== lines[0][0].toLowerCase()) throw new Error(`checksum mismatch for ${name} (expected ${lines[0][0]}, got ${got})`);
    return data;
  });
  for (const [i, p] of PLATFORMS.entries()) {
    const archive = archives[i];
    const bin = p.ext === ".zip" ? fromZip(archive, p.bin) : fromTarGz(archive, p.bin);
    const dir = join(out, `cli-${p.npm}`);
    mkdirSync(join(dir, "bin"), { recursive: true });
    writeFileSync(join(dir, "bin", p.bin), bin);
    chmodSync(join(dir, "bin", p.bin), 0o755);
    const json = template.replaceAll("@@PLATFORM@@", p.npm).replaceAll("@@VERSION@@", version)
      .replaceAll("@@OS@@", p.os).replaceAll("@@CPU@@", p.cpu);
    writeFileSync(join(dir, "package.json"), JSON.stringify(JSON.parse(json), null, 2) + "\n");
    writeFileSync(join(dir, "README.md"),
      `# @ghostbranch/cli-${p.npm}\n\nThe ghostbranch binary for ${p.os} ${p.cpu}. Install the \`ghostbranch\` package instead: \`npm install -g ghostbranch\`.\n`);
    copyFileSync(license, join(dir, "LICENSE"));
  }
  const main = JSON.parse(readFileSync(join(here, "ghostbranch", "package.json"), "utf8"));
  main.version = version;
  for (const dep of Object.keys(main.optionalDependencies)) main.optionalDependencies[dep] = version;
  const dir = join(out, "ghostbranch");
  mkdirSync(join(dir, "bin"), { recursive: true });
  writeFileSync(join(dir, "package.json"), JSON.stringify(main, null, 2) + "\n");
  copyFileSync(join(here, "ghostbranch", "bin", "ghostbranch.js"), join(dir, "bin", "ghostbranch.js"));
  chmodSync(join(dir, "bin", "ghostbranch.js"), 0o755);
  copyFileSync(join(here, "ghostbranch", "README.md"), join(dir, "README.md"));
  copyFileSync(license, join(dir, "LICENSE"));
  return [...PLATFORMS.map((p) => `cli-${p.npm}`), "ghostbranch"];
}

function flags(argv) {
  const f = {};
  for (let i = 0; i < argv.length; i += 2) {
    if (!argv[i].startsWith("--") || argv[i + 1] === undefined) throw new Error(`bad argument ${argv[i]}`);
    f[argv[i].slice(2)] = argv[i + 1];
  }
  return f;
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    const f = flags(process.argv.slice(2));
    if (!f.version || !f.assets || !f.out) throw new Error("usage: node npm/build.mjs --version vX.Y.Z --assets <dir> --out <dir>");
    for (const dir of build({ version: f.version, assets: f.assets, out: f.out })) console.log(join(f.out, dir));
  } catch (e) {
    console.error(`build: ${e.message}`);
    process.exit(1);
  }
}
