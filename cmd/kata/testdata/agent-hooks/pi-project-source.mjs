import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { pathToFileURL } from "node:url";

const [extensionPath, executable, workspace, cwd] = process.argv.slice(2);
const factory = (await import(pathToFileURL(extensionPath))).default;
let beforeAgentStart;
const pi = {
  on(name, handler) {
    if (name === "before_agent_start") beforeAgentStart = handler;
  },
  exec(file, args, options) {
    assert.equal(file, executable);
    return new Promise(resolve => execFile(file, args, { cwd: options.cwd, timeout: options.timeout }, (error, stdout, stderr) => {
      resolve({ code: error ? 1 : 0, stdout, stderr, killed: false });
    }));
  }
};
factory(pi);
assert.equal(typeof beforeAgentStart, "function");
const event = { systemPromptOptions: { sections: {} } };
await beforeAgentStart(event, { cwd, ui: { notify() {} } });
assert.equal(event.systemPromptOptions.sections.kata_contract, "workspace contract");
assert.notEqual(event.systemPromptOptions.sections.kata_contract, "nested decoy");
console.log("project workspace: " + workspace);
