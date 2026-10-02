#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";

const args = process.argv.slice(2);
if (!args.includes("agent-contract-hook")) process.exit(2);
const sourceIndex = args.indexOf("--source");
if (sourceIndex < 0 || !args[sourceIndex + 1]) process.exit(2);
const content = fs.readFileSync(path.resolve(process.cwd(), args[sourceIndex + 1]), "utf8");
process.stdout.write(JSON.stringify({ hookSpecificOutput: { additionalContext: content } }));
