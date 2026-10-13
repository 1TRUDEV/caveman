import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";

import { parseWindowsExeShim, parseWindowsNodeShim, portableInvocation } from "../dist/portable-command.js";

test("parses managed Pi's Node command shim", () => {
  assert.equal(parseWindowsNodeShim('@ECHO off\r\nnode "%~dp0pi-launcher.js" %*\r\n'), "pi-launcher.js");
});

test("parses npm and pnpm Node command shims", () => {
  assert.equal(
    parseWindowsNodeShim('endLocal & "%_prog%" "%dp0%\\..\\pkg\\cli.js" %*'),
    "..\\pkg\\cli.js",
  );
  assert.equal(
    parseWindowsNodeShim('node "%~dp0\\..\\pkg\\cli.mjs" %*'),
    "..\\pkg\\cli.mjs",
  );
  // Exact npm cmd-shim@7 payload line (PATHEXT strip + two quoted segments).
  assert.equal(
    parseWindowsNodeShim(
      'endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & set PATHEXT=%PATHEXT:;.JS;=;% & "%_prog%"  "%dp0%\\..\\pkg\\cli.js" %*\r\n',
    ),
    "..\\pkg\\cli.js",
  );
  // pnpm / yarn-classic (@zkochan/cmd-shim) IF EXIST form — the node.exe quoted
  // prefix must not shadow the .js target.
  assert.equal(
    parseWindowsNodeShim(
      '@SETLOCAL\r\n@IF EXIST "%~dp0\\node.exe" (\r\n  "%~dp0\\node.exe"   "%~dp0\\..\\pkg\\cli.js" %*\r\n) ELSE (\r\n  @SET PATHEXT=%PATHEXT:;.JS;=;%\r\n  node   "%~dp0\\..\\pkg\\cli.js" %*\r\n)\r\n',
    ),
    "..\\pkg\\cli.js",
  );
  // pnpm cross-drive: bin dir and store on different drives makes the target
  // absolute instead of %~dp0-relative.
  assert.equal(
    parseWindowsNodeShim(
      '@SETLOCAL\r\n@IF EXIST "%~dp0\\node.exe" (\r\n  "%~dp0\\node.exe"   "D:\\pnpm-store\\pkg\\cli.js" %*\r\n) ELSE (\r\n  @SET PATHEXT=%PATHEXT:;.JS;=;%\r\n  node   "D:\\pnpm-store\\pkg\\cli.js" %*\r\n)\r\n',
    ),
    "D:\\pnpm-store\\pkg\\cli.js",
  );
  // pnpm pinned-node variant: absolute node interpreter, %~dp0-relative target.
  assert.equal(
    parseWindowsNodeShim('@SETLOCAL\r\n@"C:\\Program Files\\nodejs\\node.exe"  "%~dp0\\..\\pkg\\cli.js" %*\r\n'),
    "..\\pkg\\cli.js",
  );
});

test("Windows Node shim launches target with Node and preserves argument bytes", () => {
  const root = mkdtempSync(join(tmpdir(), "cave-win-shim-"));
  try {
    const target = join(root, "pkg", "cli.js");
    mkdirSync(join(root, "pkg"), { recursive: true });
    writeFileSync(target, "process.exit(0);\n");
    const shim = join(root, "agent.cmd");
    writeFileSync(shim, 'endLocal & "%_prog%" "%dp0%\\pkg\\cli.js" %*\r\n');
    const args = ["--prompt", "100% & literal", 'quote"kept'];
    assert.deepEqual(portableInvocation(shim, args, "win32"), {
      command: process.execPath,
      args: [target, ...args],
    });
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("Pi-style nested Windows shim launches JS without a shell", () => {
  const root = mkdtempSync(join(tmpdir(), "cave nested shim "));
  try {
    const child = join(root, "node_modules", ".bin", "pi.cmd");
    const script = join(root, "pi package", "cli.mjs");
    mkdirSync(join(root, "node_modules", ".bin"), { recursive: true });
    mkdirSync(join(root, "pi package"), { recursive: true });
    writeFileSync(script, "// fixture\n");
    writeFileSync(child, 'node "%~dp0\\..\\..\\pi package\\cli.mjs" %*\r\n');
    const shim = join(root, "pi.CMD");
    writeFileSync(shim, '@ECHO off\r\n"%~dp0node_modules\\.bin\\pi.cmd" %*\r\n');
    const args = ["space & %PATH%", 'quote"kept'];
    assert.deepEqual(portableInvocation(shim, args, "win32"), {
      command: process.execPath, args: [script, ...args],
    });
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("nested shims reject unsafe wrappers, missing targets, cycles and excessive depth", () => {
  const root = mkdtempSync(join(tmpdir(), "cave nested reject "));
  try {
    const shim = join(root, "pi.cmd");
    const child = join(root, "child.bat");
    writeFileSync(join(root, "cli.js"), "// fixture\n");
    const args = ["x&y"];
    const invoke = () => portableInvocation(shim, args, "win32");
    for (const content of [
      '"%~dp0child.bat" %*\r\necho unsafe\r\n',
      '"%~dp0child.bat" %* & echo unsafe\r\n',
      '"%~dp0child.ps1" %*\r\n',
      '"%~dp0child.bat" %*\r\nnode "%~dp0missing.js" %*\r\n',
      '"%~dp0child.bat" %*\r\nnode "%~dp0cli.js" %*\r\n',
    ]) {
      writeFileSync(shim, content);
      assert.throws(invoke, /cannot safely launch non-Node Windows command shim/);
    }
    writeFileSync(shim, '"%~dp0child.bat" %*\r\n');
    assert.throws(invoke, /Windows command shim target is missing/);
    writeFileSync(child, '"%~dp0pi.cmd" %*\r\n');
    assert.throws(invoke, /Windows command shim cycle/);
    for (let i = 0; i <= 5; i++) {
      writeFileSync(join(root, `shim${i}.cmd`), `"%~dp0shim${i + 1}.cmd" %*\r\n`);
    }
    assert.throws(() => portableInvocation(join(root, "shim0.cmd"), args, "win32"), /nesting too deep/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("non-Node Windows shims fail closed instead of using injectable shell mode", () => {
  const root = mkdtempSync(join(tmpdir(), "cave-win-shim-"));
  try {
    const shim = join(root, "agent.cmd");
    writeFileSync(shim, "@echo off\r\necho %*\r\n");
    assert.throws(
      () => portableInvocation(shim, ["unsafe&arg"], "win32"),
      /cannot safely launch non-Node Windows command shim/,
    );
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

// #1252: npm's cmd-shim forwards a package whose `bin` is a native executable
// with one %*-terminated line and no interpreter. The parser understood only
// Node-script and nested-batch wrappers, so an npm-installed OpenCode read as
// `version_probe_failed` on Windows — doctor called the host unavailable and
// refused to enable or repair it, while `opencode --version` worked in the same
// shell. The advice it printed ("install a native .exe") was already satisfied:
// the tool IS one, just reached through a shim.
test("npm .cmd shim forwarding to a native .exe launches the exe directly (#1252)", () => {
  const root = mkdtempSync(join(tmpdir(), "cave-win-exe-shim-"));
  try {
    const exe = join(root, "node_modules", "@opencode", "cli", "bin", "opencode.exe");
    mkdirSync(dirname(exe), { recursive: true });
    writeFileSync(exe, "MZ native fixture");
    const shim = join(root, "opencode.cmd");
    // Verbatim npm cmd-shim@7 non-Node payload, %dp0% subroutine form and all.
    writeFileSync(shim, [
      "@ECHO off",
      "GOTO start",
      ":find_dp0",
      "SET dp0=%~dp0",
      "EXIT /b",
      ":start",
      "SETLOCAL",
      "CALL :find_dp0",
      '"%dp0%\\node_modules\\@opencode\\cli\\bin\\opencode.exe"   %*',
      "",
    ].join("\r\n"));
    // No process.execPath in front: a native binary is not a Node script.
    assert.deepEqual(portableInvocation(shim, ["--version"], "win32"), {
      command: exe,
      args: ["--version"],
    });
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

// Spellings are covered as a pure parse, the way the pnpm cross-drive Node
// forms are: a drive-absolute target cannot be laid down on a POSIX runner.
test("parses the native-exe shim spellings and only the bare forwarding form (#1252)", () => {
  assert.equal(
    parseWindowsExeShim('@ECHO off\r\nSET dp0=%~dp0\r\n"%dp0%\\node_modules\\@opencode\\cli\\bin\\opencode.exe"   %*\r\n'),
    "node_modules\\@opencode\\cli\\bin\\opencode.exe",
  );
  assert.equal(parseWindowsExeShim('"%~dp0\\bin\\tool.exe" %*\r\n'), "bin\\tool.exe");
  assert.equal(parseWindowsExeShim('"%~dp0tool.exe" %*\r\n'), "tool.exe");
  assert.equal(parseWindowsExeShim('@"D:\\store\\pkg\\bin\\tool.exe" %*\r\n'), "D:\\store\\pkg\\bin\\tool.exe");

  // A Node shim names node.exe on the same line as its script. The anchored
  // form cannot mistake that interpreter for the forwarding target, which is
  // what keeps every existing pnpm/npm Node shim on the Node path.
  assert.equal(parseWindowsExeShim('@IF EXIST "%~dp0\\node.exe" (\r\n  "%~dp0\\node.exe"   "%~dp0\\..\\pkg\\cli.js" %*\r\n)\r\n'), null);
  assert.equal(parseWindowsExeShim('@"C:\\Program Files\\nodejs\\node.exe"  "%~dp0\\..\\pkg\\cli.js" %*\r\n'), null);
  // Anything beyond a bare forward stays unrecognized.
  assert.equal(parseWindowsExeShim('"%~dp0tool.exe" %* & echo unsafe\r\n'), null);
  assert.equal(parseWindowsExeShim('"%~dp0tool.ps1" %*\r\n'), null);
  assert.equal(parseWindowsExeShim('"%~dp0tool.exe"\r\n'), null);
  // `..` is ordinary in a cmd-shim target (the bin dir sits beside the package
  // tree), exactly as on the Node path, so it must not be treated as suspect.
  assert.equal(parseWindowsExeShim('"%dp0%\\..\\pkg\\bin\\tool.exe" %*\r\n'), "..\\pkg\\bin\\tool.exe");
});

test("a native-exe shim that does more than forward still fails closed (#1252)", () => {
  const root = mkdtempSync(join(tmpdir(), "cave-win-exe-reject-"));
  try {
    const exe = join(root, "child.exe");
    writeFileSync(exe, "MZ native fixture");
    const shim = join(root, "agent.cmd");
    const invoke = () => portableInvocation(shim, ["unsafe&arg"], "win32");
    for (const content of [
      // Trailing batch syntax on the forwarding line: not a bare forward.
      '"%~dp0child.exe" %* & echo unsafe\r\n',
      // Batch forwarding owns the whole wrapper; an .exe line inside one is
      // never picked out of it (the nested parser must match end to end).
      '"%~dp0child.bat" %*\r\n"%~dp0child.exe" %*\r\n',
      // Still not an executable we know how to launch without cmd.exe.
      '"%~dp0child.ps1" %*\r\n',
    ]) {
      writeFileSync(shim, content);
      assert.throws(invoke, /cannot safely launch non-Node Windows command shim/, content);
    }
    // A recognized forward whose target is absent reports THAT, not a parse
    // failure: the shim is fine, the install is broken.
    writeFileSync(shim, '"%~dp0missing.exe" %*\r\n');
    assert.throws(invoke, /Windows command shim target is missing/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("native executables and POSIX commands pass through", () => {
  assert.deepEqual(portableInvocation("agent.exe", ["x"], "win32"), {
    command: "agent.exe",
    args: ["x"],
  });
  assert.deepEqual(portableInvocation("agent", ["x"], "darwin"), {
    command: "agent",
    args: ["x"],
  });
});
