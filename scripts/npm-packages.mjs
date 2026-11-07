#!/usr/bin/env node
// Builds ready-to-publish npm package folders for the sendio CLI from
// GoReleaser's dist/ output. It never publishes; it prints the commands.
//
//   goreleaser release --snapshot --clean     # or a real release
//   node scripts/npm-packages.mjs [--dist dist] [--out dist/npm]
//
// Output (cli/dist/npm/ by default):
//   cli/                    @sendio/cli (shim + optionalDependencies)
//   cli-<platform>-<arch>/  @sendio/cli-<platform>-<arch> (bin/sendio[.exe])
//
// Platform/arch use Node's names (process.platform / process.arch), like
// esbuild: darwin|linux|win32 x x64|arm64.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const cliDir = path.resolve(here, "..");

function parseArgs(argv) {
  const opts = { dist: path.join(cliDir, "dist"), out: null };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const val = () => {
      if (i + 1 >= argv.length) fail(`${a} needs a value`);
      return argv[++i];
    };
    if (a === "--dist") opts.dist = path.resolve(val());
    else if (a === "--out") opts.out = path.resolve(val());
    else if (a === "-h" || a === "--help") {
      console.log("usage: node scripts/npm-packages.mjs [--dist <goreleaser dist>] [--out <dir>]");
      process.exit(0);
    } else fail(`unknown argument: ${a}`);
  }
  opts.out ??= path.join(opts.dist, "npm");
  return opts;
}

function fail(msg) {
  console.error(`npm-packages: ${msg}`);
  process.exit(1);
}

function readJSON(file) {
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"));
  } catch (err) {
    fail(`cannot read ${file}: ${err.message} (run goreleaser first)`);
  }
}

const GOOS_TO_NODE = { darwin: "darwin", linux: "linux", windows: "win32" };
const GOARCH_TO_NODE = { amd64: "x64", arm64: "arm64" };

const opts = parseArgs(process.argv.slice(2));
const metadata = readJSON(path.join(opts.dist, "metadata.json"));
const artifacts = readJSON(path.join(opts.dist, "artifacts.json"));
const version = String(metadata.version || "").replace(/^v/, "");
if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$/.test(version)) {
  fail(`metadata.json version "${version}" is not valid semver`);
}

const template = readJSON(path.join(cliDir, "npm", "cli", "package.json"));
const platformReadme = fs.readFileSync(path.join(cliDir, "npm", "platform", "README.md"), "utf8");

// Fields every package shares with the main one.
const shared = {
  version,
  license: template.license,
  homepage: template.homepage,
  repository: template.repository,
  bugs: template.bugs,
};

const binaries = artifacts.filter(
  (a) => a.type === "Binary" && GOOS_TO_NODE[a.goos] && GOARCH_TO_NODE[a.goarch],
);
if (binaries.length === 0) fail(`no binaries for supported platforms in ${opts.dist}/artifacts.json`);

// GoReleaser artifact paths are relative to the directory it ran in (cli/).
const resolveArtifact = (p) => {
  const candidates = [path.resolve(cliDir, p), path.resolve(path.dirname(opts.dist), p), path.resolve(p)];
  return candidates.find((c) => fs.existsSync(c)) ?? fail(`binary not found: ${p}`);
};

fs.rmSync(opts.out, { recursive: true, force: true });
fs.mkdirSync(opts.out, { recursive: true });

const platformPkgs = [];
for (const a of binaries.sort((x, y) => `${x.goos}${x.goarch}`.localeCompare(`${y.goos}${y.goarch}`))) {
  const os = GOOS_TO_NODE[a.goos];
  const cpu = GOARCH_TO_NODE[a.goarch];
  const name = `@sendio/cli-${os}-${cpu}`;
  const dir = path.join(opts.out, `cli-${os}-${cpu}`);
  const exe = os === "win32" ? "sendio.exe" : "sendio";
  fs.mkdirSync(path.join(dir, "bin"), { recursive: true });
  fs.copyFileSync(resolveArtifact(a.path), path.join(dir, "bin", exe));
  fs.chmodSync(path.join(dir, "bin", exe), 0o755);
  const pkg = {
    name,
    ...shared,
    description: `The sendio CLI binary for ${os}-${cpu} (installed by @sendio/cli)`,
    os: [os],
    cpu: [cpu],
    files: [`bin/${exe}`, "README.md"],
    preferUnplugged: true,
  };
  fs.writeFileSync(path.join(dir, "package.json"), JSON.stringify(pkg, null, 2) + "\n");
  fs.writeFileSync(
    path.join(dir, "README.md"),
    platformReadme.replaceAll("{{name}}", name).replaceAll("{{os}}", os).replaceAll("{{cpu}}", cpu),
  );
  platformPkgs.push({ name, dir });
}

// Main package: the template, with version and exact-pinned platform packages.
const mainDir = path.join(opts.out, "cli");
fs.mkdirSync(path.join(mainDir, "bin"), { recursive: true });
fs.copyFileSync(path.join(cliDir, "npm", "cli", "bin", "sendio.js"), path.join(mainDir, "bin", "sendio.js"));
fs.chmodSync(path.join(mainDir, "bin", "sendio.js"), 0o755);
fs.copyFileSync(path.join(cliDir, "npm", "cli", "README.md"), path.join(mainDir, "README.md"));
const mainPkg = {
  ...template,
  version,
  optionalDependencies: Object.fromEntries(platformPkgs.map((p) => [p.name, version])),
};
fs.writeFileSync(path.join(mainDir, "package.json"), JSON.stringify(mainPkg, null, 2) + "\n");

const rel = (p) => path.relative(process.cwd(), p) || ".";
console.log(`npm-packages: wrote ${platformPkgs.length + 1} packages for version ${version} to ${rel(opts.out)}`);
for (const p of platformPkgs) console.log(`  ${p.name.padEnd(26)} ${rel(p.dir)}`);
console.log(`  ${"@sendio/cli".padEnd(26)} ${rel(mainDir)}`);

// Prereleases (e.g. snapshots) must not become "latest".
const tag = version.includes("-") ? " --tag next" : "";
console.log("\nNot publishing. To publish, run (platform packages first):");
for (const p of platformPkgs) console.log(`  npm publish --access public${tag} ${rel(p.dir)}`);
console.log(`  npm publish --access public${tag} ${rel(mainDir)}`);
