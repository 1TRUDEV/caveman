// Windows-safe command invocation.
//
// `execFile("caveman", …)` on Windows resolves to whatever PATH offers first.
// npm/pnpm .bin directories park a NON-EXECUTABLE Unix shim under the bare name
// right next to the real `.CMD`, and `~/.caveman/bin/caveman` is likewise an
// extensionless script. Handing either to execFile fails with `spawn EFTYPE`,
// which is what took down every pi-extension hook call on Windows.
//
// Resolve through PATHEXT the way Windows does, then, for a `.cmd`/`.bat` Node
// shim, read the shim and launch its target script with the current Node binary
// directly — no shell, so no quoting surface for user-controlled paths (#834).
//
// This is a deliberate third copy of the helper that already lives in
// installer/lib/portable-process.js and packages/cli/src/portable-command.ts. This
// package publishes with zero runtime dependencies and is esbuild-bundled, so
// importing across packages would mean taking on a dependency purely for ~50
// lines. Keep the three in sync.

import { existsSync, readFileSync, statSync } from "node:fs";
import { dirname, extname, isAbsolute, join, resolve } from "node:path";

export type PortableInvocation = { command: string; args: string[] };

export function parseWindowsNodeShim(source: string): string | null {
  for (const line of source.split(/\r?\n/)) {
    if (!/(?:\bnode(?:\.exe)?\b|_prog)/i.test(line) || !/%\*/.test(line)) continue;
    // Shim-relative target (npm cmd-shim, pnpm/yarn-classic @zkochan forms), or
    // a drive-absolute target (pnpm emits one when the global bin dir and the
    // store sit on different drives — path.relative crosses drives as absolute).
    // %~dp0 already ends with a separator; managed Pi adds none before its target.
    const match = line.match(/"%(?:dp0%|~dp0)\\?([^"\r\n]+\.(?:cjs|mjs|js))"\s+%\*/i)
      ?? line.match(/"([A-Za-z]:[\\/][^"\r\n]+\.(?:cjs|mjs|js))"\s+%\*/i);
    if (match) return match[1]!;
  }
  return null;
}

// npm's cmd-shim forwards a package whose `bin` is a native executable with one
// bare %*-terminated line and no interpreter in front of it:
//
//   "%dp0%\node_modules\@opencode\cli\bin\opencode.exe"   %*
//
// Only Node-script and nested-batch wrappers were recognized, so an
// npm-installed host read as unlaunchable while the same command worked in the
// user's shell (#1252). Anchoring the match to the whole line is what keeps
// every existing Node shim on the Node path: those name interpreter and script
// on one line, so the interpreter can never be taken for the target. The target
// is launched directly, so no cmd.exe parses the line and no argv is
// interpolated into it.
export function parseWindowsExeShim(source: string): string | null {
  for (const line of source.split(/\r?\n/)) {
    if (!/%\*/.test(line)) continue;
    const match = line.match(/^[ \t]*@?"%(?:dp0%|~dp0)\\?([^"\r\n]+\.exe)"[ \t]+%\*[ \t]*$/i)
      ?? line.match(/^[ \t]*@?"([A-Za-z]:[\\/][^"\r\n]+\.exe)"[ \t]+%\*[ \t]*$/i);
    if (match) return match[1]!;
  }
  return null;
}

// Accept only a single shim-relative forwarding command, optionally preceded by
// @echo off. Never evaluate batch syntax or interpolate caller-controlled argv.
function parseWindowsNestedShim(source: string): string | null {
  const match = source.match(/^(?:@echo[ \t]+off[ \t]*\r?\n)?[ \t]*"%~dp0\\?([\w .@+\-\\/]+\.(?:cmd|bat))"[ \t]+%\*[ \t]*(?:\r?\n)?$/i);
  return match?.[1] ?? null;
}

// A resolved shim target plus how to launch it: a Node script needs the current
// Node binary in front of it, a native executable must be launched as itself.
type ResolvedShim = { target: string; runtime: "node" | "native" };

function resolveWindowsShim(executable: string, depth = 0, seen = new Set<string>()): ResolvedShim {
  if (depth > 4) throw new Error("Windows command shim nesting too deep");
  const normalized = resolve(executable);
  if (seen.has(normalized.toLowerCase())) throw new Error("Windows command shim cycle");
  seen.add(normalized.toLowerCase());
  const stat = statSync(normalized);
  if (!stat.isFile() || stat.size > 256 * 1024) {
    throw new Error(`cannot safely launch Windows command shim: ${normalized}`);
  }
  const source = readFileSync(normalized, "utf8");
  // Batch forwarding must match the whole wrapper, not a later Node command.
  const nested = parseWindowsNestedShim(source);
  const forwardsToBatch = /"[^"\r\n]+\.(?:cmd|bat)"[ \t]+%\*/i.test(source);
  const jsTarget = forwardsToBatch ? null : parseWindowsNodeShim(source);
  // A wrapper that forwards to another batch file is the nested parser's to
  // read end to end; never pick an .exe line out of the middle of one.
  const exeTarget = forwardsToBatch || jsTarget ? null : parseWindowsExeShim(source);
  const child = jsTarget ?? exeTarget ?? nested;
  if (!child) {
    throw new Error(`cannot safely launch non-Node Windows command shim: ${normalized}`);
  }
  const target = /^[A-Za-z]:[\\/]/.test(child)
    ? child
    : resolve(dirname(normalized), ...child.split(/[\\/]+/));
  if (!existsSync(target) || !statSync(target).isFile()) throw new Error(`Windows command shim target is missing: ${target}`);
  if (jsTarget) return { target, runtime: "node" };
  if (exeTarget) return { target, runtime: "native" };
  return resolveWindowsShim(target, depth + 1, seen);
}

function envValue(env: NodeJS.ProcessEnv, name: string): string | undefined {
  const key = Object.keys(env).find((candidate) => candidate.toLowerCase() === name.toLowerCase());
  return key === undefined ? undefined : env[key];
}

export function resolveWindowsCommand(command: string, env: NodeJS.ProcessEnv): string | undefined {
  const pathExt = envValue(env, "PATHEXT") ?? ".COM;.EXE;.BAT;.CMD";
  // Extensionless names resolve ONLY through PATHEXT — probing the bare name
  // first picks the unusable Unix shim sitting beside the real .CMD (#834).
  const names = extname(command)
    ? [command]
    : pathExt.split(";").map((extension) =>
      `${command}${extension.startsWith(".") ? extension : `.${extension}`}`);
  // A path (~/.caveman/bin/caveman, the hook bridge's third candidate) skips
  // PATH lookup but NOT PATHEXT: returning the extensionless file straight to
  // execFile is the exact EFTYPE this file exists to prevent.
  if (isAbsolute(command) || /[\\/]/.test(command)) {
    for (const name of names) if (existsSync(name)) return name;
    return existsSync(command) ? command : undefined;
  }
  for (const directory of (envValue(env, "PATH") ?? "").split(";")) {
    if (!directory) continue;
    for (const name of names) {
      const candidate = join(directory, name);
      if (existsSync(candidate)) return candidate;
    }
  }
  return undefined;
}

// Returns the command/args pair to hand to execFile/spawn. Non-win32 is a
// pass-through, so this is safe to route every invocation through.
//
// Fail-open by contract: the hook bridge treats a throw the same as a spawn
// error, and an unresolvable command falls through to the next candidate, so a
// shim we cannot parse must never take down the caller.
export function portableInvocation(
  command: string,
  args: readonly string[],
  platform: NodeJS.Platform = process.platform,
  env: NodeJS.ProcessEnv = process.env,
): PortableInvocation {
  if (platform !== "win32") return { command, args: [...args] };
  const executable = resolveWindowsCommand(command, env) ?? command;
  if (!/\.(?:cmd|bat)$/i.test(executable)) return { command: executable, args: [...args] };
  const resolved = resolveWindowsShim(executable);
  return resolved.runtime === "native"
    ? { command: resolved.target, args: [...args] }
    : { command: process.execPath, args: [resolved.target, ...args] };
}
