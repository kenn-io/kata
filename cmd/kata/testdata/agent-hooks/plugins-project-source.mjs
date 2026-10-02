import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [pluginPath, workspace, nested] = process.argv.slice(2);
const pluginDir = path.dirname(pluginPath);
await fs.writeFile(path.join(pluginDir, "package.json"), JSON.stringify({ type: "module" }));
const sdk = path.join(pluginDir, "node_modules", "@opencode", "plugin");
await fs.mkdir(sdk, { recursive: true });
await fs.writeFile(path.join(sdk, "package.json"), JSON.stringify({ type: "module", exports: "./index.js" }));
await fs.writeFile(path.join(sdk, "index.js"), "export const Plugin = { define: value => value };");
const plugin = await import(pathToFileURL(pluginPath));
const handlers = new Map();
await plugin.default.setup({
  location: { directory: workspace },
  session: {
    get: async ({ sessionID }) => ({ id: sessionID, location: { directory: nested } }),
    hook: async (name, handler) => {
      handlers.set(name, handler);
      return { dispose: async () => handlers.delete(name) };
    }
  }
});
const system = [];
await handlers.get("context")({ sessionID: "nested-session", system });
assert.equal(system.length, 1);
assert.match(system[0].text, /<kata_contract>\nworkspace contract\n<\/kata_contract>/);
assert.doesNotMatch(system[0].text, /nested decoy/);
console.log("project workspace: " + workspace);
