"use strict";

const { execFileSync, spawn } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

const projectRoot = path.resolve(__dirname, "../..");
const fixtureParent = path.join(projectRoot, "test-results", "browser-fixtures");
fs.mkdirSync(fixtureParent, { recursive: true });
const fixtureRoot = fs.mkdtempSync(path.join(fixtureParent, "ewasd-browser-"));
const repository = path.join(fixtureRoot, "repository");
const secondaryRepository = path.join(fixtureRoot, "secondary");
const dataRoot = path.join(fixtureRoot, "state");
const binary = path.join(fixtureRoot, "ewasd");
const token = process.env.EWASD_BROWSER_TOKEN;

function run(file, args, options = {}) {
  return execFileSync(file, args, {
    cwd: options.cwd || projectRoot,
    env: { ...process.env, EWASD_HOME: dataRoot },
    encoding: "utf8",
    stdio: options.stdio || ["ignore", "pipe", "pipe"]
  });
}

function prepareRepository(root, remote) {
  fs.mkdirSync(root, { recursive: true });
  run("git", ["init", "-q"], { cwd: root });
  if (remote) run("git", ["remote", "add", "origin", remote], { cwd: root });
}

function planAndApply(action, relativePath) {
  const plan = JSON.parse(run(binary, [action, "--root", repository, "--json", relativePath]));
  run(binary, [
    action,
    "--root", repository,
    "--revision", String(plan.expected_revision),
    "--fingerprint", plan.fingerprint,
    "--apply",
    "--json",
    relativePath
  ]);
}

prepareRepository(repository, "git@github.com:example/browser-fixture.git");
prepareRepository(secondaryRepository, "");
fs.mkdirSync(path.join(repository, ".claude"));
fs.writeFileSync(path.join(repository, ".claude", "settings.json"), "{}\n");
fs.writeFileSync(path.join(repository, "AGENT.md"), "# browser fixture\n");
for (const target of ["desktop-standard", "desktop-1440p"]) {
  fs.writeFileSync(path.join(repository, `adopt-${target}.txt`), `${target}\n`);
}

run("go", ["build", "-trimpath", "-o", binary, "./cmd/ewasd"]);
run(binary, ["register", "--root", repository, "--name", "Browser Fixture", "--json"]);
run(binary, ["register", "--root", secondaryRepository, "--name", "Empty Comparison", "--json"]);
planAndApply("adopt", "AGENT.md");
planAndApply("adopt", ".claude");

const child = spawn(binary, ["serve", "--listen", "127.0.0.1:17337"], {
  cwd: projectRoot,
  env: {
    ...process.env,
    EWASD_HOME: dataRoot,
    EWASD_TOKEN: token
  },
  stdio: ["ignore", "inherit", "inherit"]
});

let stopping = false;
function stop(signal = "SIGTERM") {
  if (stopping) return;
  stopping = true;
  if (!child.killed) child.kill(signal);
  setTimeout(() => {
    fs.rmSync(fixtureRoot, { recursive: true, force: true });
    process.exit(0);
  }, 250).unref();
}

process.on("SIGTERM", () => stop("SIGTERM"));
process.on("SIGINT", () => stop("SIGINT"));
child.on("exit", code => {
  fs.rmSync(fixtureRoot, { recursive: true, force: true });
  process.exit(code ?? 0);
});
