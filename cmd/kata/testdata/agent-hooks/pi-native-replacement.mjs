import assert from 'node:assert/strict';
import {readFileSync,writeFileSync} from 'node:fs';
import {pathToFileURL} from 'node:url';
const [root,projectPath,userPath,cwd]=process.argv.slice(2);
const {ExtensionRunner}=await import(pathToFileURL(root+'/core/extensions/runner.js'));
const {createExtensionRuntime,loadExtensionFromFactory}=await import(pathToFileURL(root+'/core/extensions/loader.js'));
const {createEventBus}=await import(pathToFileURL(root+'/core/event-bus.js'));
const {SessionManager}=await import(pathToFileURL(root+'/core/session-manager.js'));
const project=(await import(pathToFileURL(projectPath))).default,user=(await import(pathToFileURL(userPath))).default;
const calls=()=>readFileSync(process.env.KATA_PI_TEST_CALLS,'utf8').trim().split('\n').filter(Boolean).map(line=>JSON.parse(line));
const base={customPrompt:'base',cwd,sections:{other:'preserved'}};
const errors=[];
async function load(manager,includeProject) {
 const runtime=createExtensionRuntime(),bus=createEventBus();
 const extensions=[await loadExtensionFromFactory(user,cwd,bus,runtime,userPath)];
 if(includeProject)extensions.push(await loadExtensionFromFactory(project,cwd,bus,runtime,projectPath));
 const runner=new ExtensionRunner(extensions,runtime,cwd,manager,{});runner.onError(error=>errors.push(error));return runner;
}
for(const reason of ['new','resume','fork','reload']) {
 writeFileSync(process.env.KATA_PI_TEST_STATE,JSON.stringify({contract:'old contract',inbox:'old request'}));
 const oldManager=SessionManager.inMemory(cwd),old=await load(oldManager,true);
 process.env.KATA_REF='captured-'+reason;
 await old.emit({type:'session_start',reason:'startup'});
 const initial=await old.emitBeforeAgentStart('initial',undefined,base);
 assert.equal(initial.systemPromptOptions.sections.kata_contract,'old contract');
 assert.equal(calls().filter(c=>c.args[0]==='agent-contract-hook').at(-1).args[2],'project.txt');
 const before=calls().filter(c=>c.args[0]==='agent-hook').length;
 await old.emit({type:'session_shutdown',reason});old.invalidate();
 assert.equal(calls().filter(c=>c.args[0]==='agent-hook').length,before,'replacement teardown must not end attention');
 // Discovery now omits the removed/disabled project extension. Recreate the
 // host runtime and extension APIs exactly as Pi's native replacement does.
 const manager=reason==='reload'?oldManager:SessionManager.inMemory(cwd);
 const current=await load(manager,false);
 await current.emit({type:'session_start',reason});
 assert.equal(calls().filter(c=>c.args[0]==='agent-hook').length,before+(reason==='reload'?0:1),'user attention fallback must become active');
 const activeStart=calls().filter(c=>c.args[0]==='agent-hook').at(-1);
 writeFileSync(process.env.KATA_PI_TEST_STATE,JSON.stringify({contract:'fresh user contract',inbox:'fresh user request'}));
 const fresh=await current.emitBeforeAgentStart('replacement',undefined,base);
 assert.equal(fresh.systemPromptOptions.sections.kata_contract,'fresh user contract',reason+' must retire stale project contract');
 assert.equal(fresh.systemPromptOptions.sections.kata_inbox,'fresh user request');
 assert.equal(fresh.systemPromptOptions.sections.other,'preserved');
 assert.deepEqual(calls().filter(c=>c.args[0]==='agent-contract-hook').at(-1).args,['agent-contract-hook','--source','user.txt']);
 process.env.KATA_REF='later-unrelated-ref';
 await current.emit({type:'session_shutdown',reason:'quit'});
 const terminal=calls().filter(c=>c.args[0]==='agent-hook').at(-1);
 assert.equal(terminal.args[3],'end');assert.equal(terminal.args[9],'captured-'+reason,'cleanup retains captured ref across replacement/reload');
 assert.equal(terminal.executable,activeStart.executable,'cleanup uses the executable that started this session');
 current.invalidate();
}
assert.deepEqual(errors,[]);
console.log('Pi installed replacement passed: new/resume/fork/reload retire disabled project capabilities and preserve terminal captured refs');
