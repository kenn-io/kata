import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import http from 'node:http';
import net from 'node:net';
import {spawn} from 'node:child_process';
const [runtime,root]=process.argv.slice(2),configPath=path.join(root,'.openclaw','openclaw.json'),workspace=path.join(root,'workspace');
fs.mkdirSync(workspace,{recursive:true});fs.writeFileSync(path.join(root,'contract'),'native-contract');fs.writeFileSync(path.join(root,'inbox'),'native-request');
try{fs.unlinkSync(path.join(root,'calls.jsonl'))}catch{}
const requests=[];
const model=http.createServer(async(req,res)=>{
 let body='';for await(const chunk of req)body+=chunk;const json=JSON.parse(body);requests.push(json);fs.appendFileSync(path.join(root,'model-requests.jsonl'),body+'\n');
 if(json.stream){res.writeHead(200,{'content-type':'text/event-stream'});for(const data of [{id:'mock',object:'chat.completion.chunk',created:1,model:'example-model',choices:[{index:0,delta:{role:'assistant',content:'mock reply'},finish_reason:null}]},{id:'mock',object:'chat.completion.chunk',created:1,model:'example-model',choices:[{index:0,delta:{},finish_reason:'stop'}],usage:{prompt_tokens:12,completion_tokens:3,total_tokens:15}}])res.write('data: '+JSON.stringify(data)+'\n\n');res.end('data: [DONE]\n\n')}
 else {res.setHeader('content-type','application/json');res.end(JSON.stringify({id:'mock',object:'chat.completion',created:1,model:'example-model',choices:[{index:0,message:{role:'assistant',content:'mock reply'},finish_reason:'stop'}],usage:{prompt_tokens:12,completion_tokens:3,total_tokens:15}}))}
});
await new Promise(r=>model.listen(0,'127.0.0.1',r));
const reserve=net.createServer();await new Promise(r=>reserve.listen(0,'127.0.0.1',r));const port=reserve.address().port;await new Promise(r=>reserve.close(r));
const config=JSON.parse(fs.readFileSync(configPath,'utf8'));
config.logging={file:path.join(root,'gateway-host.log')};
config.gateway={mode:'local',port,bind:'loopback',auth:{mode:'token',token:'isolated-mock-token'}};
config.models={providers:{example:{baseUrl:'http://127.0.0.1:'+model.address().port+'/v1',apiKey:'isolated-mock-key',api:'openai-completions',models:[{id:'example-model',name:'Example model',contextWindow:32768,maxTokens:2048,input:['text'],cost:{input:0,output:0,cacheRead:0,cacheWrite:0}}]}}};
config.agents={defaults:{workspace,model:{primary:'example/example-model'},timeoutSeconds:15},list:[{id:'main',default:true,workspace}]};
fs.writeFileSync(configPath,JSON.stringify(config,null,2)+'\n');
const env={PATH:process.env.PATH,HOME:root,TMPDIR:root,OPENCLAW_STATE_DIR:path.join(root,'.openclaw'),OPENCLAW_CONFIG_PATH:configPath,OPENCLAW_TEST_ROOT:root,KATA_REF:'abcd',KATA_INBOX_USER:'actor/worker'};
let gateway,log='';
async function cli(args,timeout=30000){return new Promise((resolve,reject)=>{const child=spawn(process.execPath,[path.join(runtime,'openclaw.mjs'),...args],{env});child.stdin.end();let out='',err='';child.stdout.on('data',b=>out+=b);child.stderr.on('data',b=>err+=b);const timer=setTimeout(()=>{child.kill('SIGKILL');reject(Error('timeout '+args.join(' ')+' '+err))},timeout);child.on('error',reject);child.on('exit',code=>{clearTimeout(timer);code===0?resolve(out):reject(Error('exit '+code+' '+args.join(' ')+' '+err+' '+out))})})}
function calls(){try{return fs.readFileSync(path.join(root,'calls.jsonl'),'utf8').trim().split('\n').filter(Boolean).map(JSON.parse)}catch{return []}}
try {
 const installed=path.join(root,'.openclaw','extensions','kata-hooks-user'),reviewed=path.join(root,'reviewed-package');
 if(fs.existsSync(installed)){fs.cpSync(installed,reviewed,{recursive:true});fs.rmSync(installed,{recursive:true});config.plugins.load.paths=[reviewed];fs.writeFileSync(configPath,JSON.stringify(config,null,2)+'\n')}
 await cli(['plugins','install','--link',reviewed,'--force','--accept-capabilities']);
 await cli(['plugins','enable','kata-hooks-user']);
 gateway=spawn(process.execPath,[path.join(runtime,'openclaw.mjs'),'gateway','run','--allow-unconfigured','--port',String(port),'--bind','loopback'],{env});gateway.stdout.on('data',b=>{log+=b;fs.appendFileSync(path.join(root,'gateway.log'),b)});gateway.stderr.on('data',b=>{log+=b;fs.appendFileSync(path.join(root,'gateway.log'),b)});
 await new Promise((resolve,reject)=>{const deadline=Date.now()+30000;const timer=setInterval(()=>{if(log.includes('[gateway] ready')){clearInterval(timer);resolve()}else if(gateway.exitCode!==null||Date.now()>deadline){clearInterval(timer);reject(Error('Gateway startup '+log))}},100)});
 console.log('isolated Gateway ready');
 const inspect=JSON.parse(await cli(['plugins','inspect','kata-hooks-user','--runtime','--json']));assert.equal(inspect.plugin.status,'loaded');
 await cli(['agent','--agent','main','--session-key','agent:main:main','--message','hello from native test','--thinking','off','--json']);
 assert.ok(requests.length>=1);const prompt=JSON.stringify(requests.at(-1).messages);assert.match(prompt,/contract:native-contract/);assert.match(prompt,/native-request/);
 assert.equal(calls().filter(c=>c.args[3]==='start').length,1);assert.equal(calls().filter(c=>c.args[3]==='end').length,0);
 await cli(['agent','--agent','main','--session-key','agent:main:main','--message','second ordinary turn','--thinking','off','--json']);assert.equal(calls().filter(c=>c.args[3]==='end').length,0);assert.equal(calls().filter(c=>c.args[3]==='start').length,1);
 for(const reason of ['reset','new']){
  await cli(['gateway','call','sessions.reset','--params',JSON.stringify({key:'agent:main:main',reason}),'--json']);
  await cli(['agent','--agent','main','--session-key','agent:main:main','--message','after '+reason,'--thinking','off','--json']);
 }
 assert.equal(calls().filter(c=>c.args[3]==='start').length,3);assert.equal(calls().filter(c=>c.args[3]==='end').length,2);
 // Native /stop aborts a turn, without terminal session cleanup.
 await cli(['gateway','call','chat.abort','--params',JSON.stringify({sessionKey:'agent:main:main'}),'--json']);assert.equal(calls().filter(c=>c.args[3]==='end').length,2);
 gateway.kill('SIGTERM');await new Promise(r=>gateway.once('exit',r));assert.equal(calls().filter(c=>c.args[3]==='end').length,3);
 console.log('published Gateway model prompt, repeated turns, reset/new RPC, abort and shutdown passed');
}catch(e){console.error(log);throw e}finally{gateway?.kill('SIGKILL');model.close()}
