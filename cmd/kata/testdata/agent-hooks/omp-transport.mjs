import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {writeFileSync} from 'node:fs';
import {pathToFileURL} from 'node:url';
import {execFile} from 'node:child_process';
import {promisify} from 'node:util';
import {createOmpHost} from './omp-host.mjs';
const [extensionPath,cwd,source,binary] = process.argv.slice(2);
const factory = (await import(pathToFileURL(extensionPath))).default;
const exec = promisify(execFile);
let revision=1,metadata={},status='open';const requests=[],writes=[],errors=[];
const server=createServer(async(req,res)=>{
 let raw='';for await(const chunk of req)raw+=chunk;const body=raw?JSON.parse(raw):{};const url=new URL(req.url,'http://daemon.example');requests.push({method:req.method,path:url.pathname,query:Object.fromEntries(url.searchParams),body});res.setHeader('content-type','application/json');
 const respond=value=>res.end(JSON.stringify(value));
 if(url.pathname==='/api/v1/ping')respond({ok:true,service:'kata',version:'test',pid:process.pid});
 else if(url.pathname==='/api/v1/projects/resolve'){assert.equal(body.name,'example-workspace');respond({project:{id:42,name:'example-workspace'}});}
 else if(url.pathname==='/api/v1/projects/42/issues/abc4/metadata'){
  assert.equal(req.headers['if-match'],`"rev-${revision}"`);assert.equal(body.actor,'actor');metadata={...metadata,...body.patch};revision++;writes.push(body.patch);respond({issue:{project_id:42,uid:'01HZZZZZZZZZZZZZZZZZZZZZ01',status,short_id:'abc4',metadata,revision}});
 } else if(url.pathname==='/api/v1/projects/42/issues/abc4')respond({issue:{project_id:42,uid:'01HZZZZZZZZZZZZZZZZZZZZZ01',status,short_id:'abc4',metadata,revision}});
 else if(url.pathname==='/api/v1/projects/42/issues'){
  assert.equal(url.searchParams.get('meta'),'notify.YWN0b3IvdGVhbW1hdGU');assert.equal(url.searchParams.get('status'),'open');
  respond({issues:[{short_id:'abc4',title:'example task',metadata:{'notify.YWN0b3IvdGVhbW1hdGU':{from:'coordinator',message:'fresh request'}}}]});
 } else if(req.method==='POST'&&url.pathname==='/api/v1/ui/telemetry'&&body.event==='agent_active'){res.statusCode=202;respond({status:'queued'});}
 else if(url.pathname==='/health')respond({status:'ok',api_version:1});
 else {errors.push(req.method+' '+url.pathname);res.statusCode=404;respond({error:'unexpected route'});}
});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));process.env.KATA_SERVER=`http://127.0.0.1:${server.address().port}`;process.env.KATA_REF='abc4';process.env.KATA_INBOX_USER='actor/teammate';process.env.KATA_AUTHOR='actor';process.env.KATA_TEAMMATE='teammate';delete process.env.KATA_SESSION_ID;
try {
 const host=createOmpHost({cwd});
 host.execute=async(exe,args,opts)=>{try {const {stdout,stderr}=await exec(exe,args,{cwd:opts.cwd,timeout:opts.timeout});return {code:0,stdout,stderr,killed:false};}catch(e){errors.push(e.stderr||e.message);return {code:e.code||1,stdout:e.stdout||'',stderr:e.stderr||'',killed:!!e.killed};}};
 host.executeIdentity=host.execute;
 factory(host);writeFileSync(source,'source current');
 // Model a live Pi owner using the local TCP daemon's canonical route identity.
 const [daemonResult,issueResult]=await Promise.all([
  exec(binary,['--json','--workspace',cwd,'daemon','locate'],{cwd}),
  exec(binary,['--json','--workspace',cwd,'show','abc4'],{cwd}),
 ]);
 const route=JSON.parse(daemonResult.stdout),resolved=JSON.parse(issueResult.stdout).issue;
 assert.equal(route.network,'tcp');
 process.env.KATA_ATTENTION_HOST=JSON.stringify({pid:process.ppid,identity:JSON.stringify({daemon:route.request_base_url,project:resolved.project_id,issue:resolved.uid})});
 await host.emit('session_start');assert.equal(writes.length,0,'OMP preserves a Pi owner for the same local TCP daemon');
 delete process.env.KATA_ATTENTION_HOST;
 await host.emit('session_start');assert.equal(metadata['work.attention'],'ok');assert.match(metadata['work.attention_session'],/^[0-9a-f]{64}$/);
 const owner=metadata['work.attention_session'];
 const prompt=await host.emit('before_agent_start',{systemPrompt:['authored']});assert(prompt.systemPrompt.join('').includes('source current'));assert(prompt.systemPrompt.join('').includes('fresh request'));
 metadata['work.attention']='stuck';metadata['work.attention_msg']='waiting for a dependency';factory(host);await host.emit('session_start');assert.equal(metadata['work.attention'],'stuck');assert.equal(writes.length,1);
 process.env.KATA_REF='unrelated-ref';await host.emit('session_shutdown');
 // Ending the session retires its owner without overwriting the explicit handoff.
 assert.equal(metadata['work.attention'],'stuck');assert.equal(metadata['work.attention_msg'],'waiting for a dependency');assert.equal(metadata['work.attention_session'],`ended:${owner}`);
 assert.deepEqual(writes,[{'work.attention':'ok','work.attention_session':owner},{'work.attention_session':`ended:${owner}`}]);
 await host.emit('session_shutdown');assert.equal(writes.length,2,'repeated shutdown does not retire the owner twice');
 process.env.KATA_REF='abc4';host.setSession('B');await host.emit('session_switch',{reason:'new'});assert.equal(metadata['work.attention'],'ok');assert.notEqual(metadata['work.attention_session'],owner);
 const nextOwner=metadata['work.attention_session'];assert.match(nextOwner,/^[0-9a-f]{64}$/);assert.deepEqual(writes[2],{'work.attention':'ok','work.attention_session':nextOwner});
 process.env.KATA_REF='unrelated-ref';await host.emit('session_shutdown');assert.equal(metadata['work.attention'],'needs-human');assert.equal(metadata['work.attention_msg'],'session ended without hand-off');assert.equal(metadata['work.attention_session'],`ended:${nextOwner}`);
 assert.equal(writes.length,4);assert.deepEqual(writes[3],{'work.attention_session':`ended:${nextOwner}`,'work.attention':'needs-human','work.attention_msg':'session ended without hand-off'});
 assert.deepEqual(errors,[]);assert(requests.some(request=>request.path==='/api/v1/ping'));assert(requests.some(request=>request.method==='GET'&&request.path==='/api/v1/projects/42/issues/abc4'));console.log('omp real Kata transport passed');
} finally {await new Promise(resolve=>server.close(resolve));}
