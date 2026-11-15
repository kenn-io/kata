#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";

const args = process.argv.slice(2);
if (!args.includes("agent-contract-hook")) process.exit(2);
const sourceIndex = args.indexOf("--source");
if (sourceIndex < 0 || !args[sourceIndex + 1]) process.exit(2);
const content = fs.readFileSync(path.resolve(process.cwd(), args[sourceIndex + 1]), "utf8");
// Report the session directory the hook gates on, which differs from the
// process cwd when a relative source resolves against the install workspace.
const workspaceIndex = args.indexOf("--workspace");
const session = workspaceIndex < 0 ? "(none)" : args[workspaceIndex + 1];
process.stdout.write(JSON.stringify({ hookSpecificOutput: { additionalContext: content + "\nsession: " + session } }));
