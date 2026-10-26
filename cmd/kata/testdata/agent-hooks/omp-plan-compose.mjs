import assert from "node:assert/strict";
import { readFileSync, writeFileSync } from "node:fs";
import { pathToFileURL } from "node:url";
import { planHost } from "./omp-plan-host.mjs";
const file = process.argv[2],
  factory = (await import(pathToFileURL(file))).default;
process.env.KATA_INBOX_USER = "actor/teammate";
const entries = [
  {
    type: "custom",
    customType: "io.kata.omp.parent",
    data: { parent: "example-workspace#parent", workspace: process.cwd() },
  },
];
const h = planHost(file, { entries });
factory(h);
await h.emit("session_start");
await h.call({ op: "view" });
let prompt = { systemPrompt: ["authored entry"] };
for (let i = 0; i < 2; i++) {
  prompt = await h.emit("before_agent_start", prompt);
  const text = prompt.systemPrompt.join("\n");
  for (const block of ["kata_contract", "kata_inbox", "kata_todo"])
    assert.equal(
      text.split("<" + block + ">").length - 1,
      1,
      block + " appears once after refresh",
    );
  assert.equal(
    prompt.systemPrompt.filter((p) => p === "authored entry").length,
    1,
  );
}
// A project contract-only adapter beside the user plan adapter owns only its
// base capability. It must not replace the user's plan generation.
const project = file + ".project.mjs";
let source = readFileSync(file, "utf8");
source = source.replace(
  /const options = [^\n]+;/,
  `const options = ${JSON.stringify({ executable: "kata", scope: "project", workspace: process.cwd(), contract: true, plan: false })};`,
);
writeFileSync(project, source);
(await import(pathToFileURL(project))).default(h);
const result = await h.emit("before_agent_start", prompt);
const text = result.systemPrompt.join("\n");
assert.equal(text.split("<kata_contract>").length - 1, 1);
assert.equal(text.split("<kata_todo>").length - 1, 1);
assert.match(
  h.widgets.at(-1)[1][0],
  /Kata example-workspace#parent · 0\/1 done · ▶ Current task/,
);
console.log("PASS prompt block composition, refresh and capability precedence");
