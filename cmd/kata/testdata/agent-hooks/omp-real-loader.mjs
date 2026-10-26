import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import {
  cpSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import os from "node:os";
import path from "node:path";
const [generated, omp, pluginSource] = process.argv.slice(2),
  execAsync = promisify(execFile);
const exec = (...args) => {
  const promise = execAsync(...args);
  promise.child.stdin.end();
  return promise;
};
const root = mkdtempSync(path.join(os.tmpdir(), "kata-omp-loader-"));
const provider = `import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
export default function probe(pi) {
 let requests=0;
 pi.registerProvider('loader-fixture',{
  api:'loader-fixture',baseUrl:'https://model.example/v1',apiKey:'literal-fixture-key',
  models:[{id:'fixture',name:'Loader fixture',reasoning:false,input:['text'],cost:{input:0,output:0,cacheRead:0,cacheWrite:0},contextWindow:32768,maxTokens:1024}],
  streamSimple(){
   requests++;
   const toolCall={type:'toolCall',id:'loader-todo',name:'todo',arguments:{op:'init',items:['Real loader task'],message:'Only Kata understands this extra argument',evidence:['test:loader']}};
   const message={role:'assistant',content:requests===1?[toolCall]:[{type:'text',text:'Fixture complete'}],api:'loader-fixture',provider:'loader-fixture',model:'fixture',usage:{input:0,output:0,cacheRead:0,cacheWrite:0,totalTokens:0,cost:{input:0,output:0,cacheRead:0,cacheWrite:0,total:0}},stopReason:requests===1?'toolUse':'stop',timestamp:Date.now()};
   return {async *[Symbol.asyncIterator](){yield {type:'start',partial:message};if(requests===1){yield {type:'toolcall_start',contentIndex:0,partial:message};yield {type:'toolcall_end',contentIndex:0,toolCall,partial:message};}yield {type:'done',reason:message.stopReason,message};},result:()=>Promise.resolve(message)};
  }
 });
 pi.on('session_start',async(_e,ctx)=>{
  const mode=process.env.KATA_LOADER_MODE;
  if(!mode.startsWith('plugin'))return;
  const native=globalThis[Symbol.for('kata.loader.tool')];assert.ok(native,'native factory loaded');
  const source=pi.getAllTools().find(t=>t.name==='todo')?.sourceInfo?.path;
  assert.equal(source.includes('released-plugin'),mode==='plugin-last','actual loader order');
  const before=readFileSync(process.env.KATA_LOADER_CALLS,'utf8');
  const result=await native.execute('probe',{op:'view'},undefined,undefined,ctx);
  assert.equal(result.isError,true);assert.match(result.content[0].text,/inactive/);
  assert.equal(readFileSync(process.env.KATA_LOADER_CALLS,'utf8'),before,'disarmed native never calls Kata');
  console.log('KATA_LOADER_PASS '+mode);process.exit(0);
 });
 pi.on('tool_result',(event)=>{
  if(event.toolName!=='todo')return;
  const record=globalThis[Symbol.for('kata.loader.execution')];assert.ok(record,'actual SDK wrapper executed native registration');
  assert.equal(record.invoke,process.env.KATA_LOADER_MODE==='native','actual native invokeTool injection');
  assert.equal(record.result.isError,undefined);assert.match(JSON.stringify(record.result),/Real loader task/);
  if(record.invoke)assert.equal(record.before,record.after,'forwarding performs no Kata command');
  else assert.match(record.result.content[0].text,/Session-local/);
  console.log('KATA_LOADER_PASS '+process.env.KATA_LOADER_MODE);process.exit(0);
 });
}
`;
const source = readFileSync(generated, "utf8");
const instrument = `const register=pi.registerTool.bind(pi);pi.registerTool=tool=>{if(tool.name==='todo'){globalThis[Symbol.for('kata.loader.tool')]=tool;const execute=tool.execute;tool.execute=async(...args)=>{const fs=await import('node:fs');const before=fs.readFileSync(process.env.KATA_LOADER_CALLS,'utf8');const result=await execute(...args);globalThis[Symbol.for('kata.loader.execution')]={invoke:!!args[4].invokeTool,result,before,after:fs.readFileSync(process.env.KATA_LOADER_CALLS,'utf8')};return result;};}return register(tool);};`;
for (const mode of [
  "active",
  "native",
  "plugin-first",
  "plugin-last",
  "profile",
]) {
  const dir = path.join(root, mode),
    home = path.join(dir, "home"),
    cwd = path.join(dir, "workspace"),
    agent = path.join(dir, "agent");
  mkdirSync(cwd, { recursive: true });
  mkdirSync(home);
  mkdirSync(agent);
  // Explicit allowlist: no real routing, proxies, auth, tokens, config or API keys.
  const env = {};
  for (const key of ["PATH", "SystemRoot", "WINDIR", "TEMP", "TMP", "TMPDIR"])
    if (process.env[key]) env[key] = process.env[key];
  Object.assign(env, {
    HOME: home,
    USERPROFILE: home,
    XDG_CONFIG_HOME: path.join(dir, "config"),
    XDG_DATA_HOME: path.join(dir, "data"),
    XDG_CACHE_HOME: path.join(dir, "cache"),
    PI_CODING_AGENT_DIR: agent,
    GOMAXPROCS: "2",
    KATA_LOADER_MODE: mode,
    KATA_LOADER_CALLS: path.join(dir, "calls.ndjson"),
  });
  const bin = path.join(dir, "bin");
  mkdirSync(bin);
  writeFileSync(env.KATA_LOADER_CALLS, "");
  const fake = path.join(dir, "kata.mjs");
  writeFileSync(
    fake,
    `#!/usr/bin/env node\nimport {appendFileSync} from 'node:fs';appendFileSync(process.env.KATA_LOADER_CALLS,JSON.stringify(process.argv.slice(2))+'\\n');console.error('project_not_initialized');process.exit(1);\n`,
    { mode: 0o700 },
  );
  // Keep runtime plugin-list probes on the exact release under test.
  if (process.platform !== "win32") {
    const { symlinkSync } = await import("node:fs");
    symlinkSync(omp, path.join(bin, "omp"));
    env.PATH = bin + path.delimiter + env.PATH;
  }
  if (mode === "profile") {
    delete env.PI_CODING_AGENT_DIR;
    env.OMP_PROFILE = "work";
  }
  const query = await exec(omp, ["config", "path"], {
    cwd,
    env,
    timeout: 20000,
  });
  const config = query.stdout.trim().split("\n").at(-1);
  if (mode === "profile")
    assert.match(config, /[\\/]profiles[\\/]work[\\/]agent$/);
  mkdirSync(path.join(config, "extensions"), { recursive: true });
  writeFileSync(path.join(config, "config.yml"), "todo:\n  enabled: false\n");
  if (mode === "native") {
    mkdirSync(path.join(cwd, ".omp"));
    writeFileSync(
      path.join(cwd, ".omp", "config.yml"),
      "todo:\n  enabled: true\n",
    );
    const check = await exec(omp, ["config", "get", "todo.enabled", "--json"], {
      cwd,
      env,
    });
    assert.equal(JSON.parse(check.stdout).value, true);
  }
  const metadata = JSON.parse(
    source.split("\n")[0].slice("// kata-native-omp ".length),
  );
  metadata.executable = fake;
  let extension = source.replace(/const options = [^\n]+;/, (match) =>
    match.replace(
      /"executable":"[^"]*"/,
      `"executable":${JSON.stringify(fake)}`,
    ),
  );
  extension = extension.replace(
    "export default function kataHooks(pi) {",
    "export default function kataHooks(pi) {" + instrument,
  );
  const extensionPath = path.join(config, "extensions", "kata.js");
  writeFileSync(extensionPath, extension);
  const probe = path.join(dir, "probe.mjs");
  writeFileSync(probe, provider);
  const args = [
    "-p",
    "--model",
    "loader-fixture/fixture",
    "--no-session",
    "--no-skills",
    "--no-rules",
    "--no-lsp",
    "--no-title",
    "--auto-approve",
  ];
  if (mode.startsWith("plugin")) {
    assert.ok(
      pluginSource,
      "KATA_TEST_OMP_PLUGIN must name the released oh-my-pi plugin directory",
    );
    const released = path.join(dir, "released-plugin");
    cpSync(pluginSource, released, { recursive: true });
    // Marketplace registry presence is real; explicit paths make both loader
    // orders reachable without changing the user's installed plugin tree.
    const registryDir = path.join(home, ".omp", "plugins");
    mkdirSync(registryDir, { recursive: true });
    writeFileSync(
      path.join(registryDir, "installed_plugins.json"),
      JSON.stringify({
        version: 2,
        plugins: {
          "kata@kata-plugin": [
            {
              scope: "user",
              installPath: released,
              version: "2.1.0",
              installedAt: "2026-01-01T00:00:00Z",
              lastUpdated: "2026-01-01T00:00:00Z",
            },
          ],
        },
      }),
    );
    // XDG-aware releases locate plugin data under XDG_DATA_HOME/omp/plugins.
    const xdg = path.join(env.XDG_DATA_HOME, "omp", "plugins");
    mkdirSync(xdg, { recursive: true });
    cpSync(
      path.join(registryDir, "installed_plugins.json"),
      path.join(xdg, "installed_plugins.json"),
    );
    const inventory = JSON.parse(
      (
        await exec(omp, ["plugin", "list", "--json"], {
          cwd,
          env,
          timeout: 20000,
        })
      ).stdout,
    );
    assert.ok(inventory.marketplace.some((p) => p.id === "kata@kata-plugin"));
    env.KATA_OMP_EXECUTABLE = fake;
    args.push(
      "--no-extensions",
      ...(mode === "plugin-first"
        ? [path.join(released, "kata.ts"), extensionPath]
        : [extensionPath, path.join(released, "kata.ts")]
      ).flatMap((file) => ["-e", file]),
    );
  }
  args.push("-e", probe, "Run fixture");
  const result = await exec(omp, args, {
    cwd,
    env,
    timeout: 60000,
    maxBuffer: 4 * 1024 * 1024,
  });
  assert.match(
    result.stdout,
    new RegExp("KATA_LOADER_PASS " + mode),
    result.stderr,
  );
  assert.equal(
    readFileSync(env.KATA_LOADER_CALLS, "utf8")
      .trim()
      .split("\n")
      .filter(Boolean)
      .some((line) =>
        ["create", "meta", "close", "claim"].includes(JSON.parse(line)[0]),
      ),
    false,
    "loader fixture never writes Kata state",
  );
  console.log("PASS " + mode + " " + omp);
}
