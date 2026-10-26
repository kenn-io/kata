#!/usr/bin/env node
import {readFileSync,appendFileSync} from 'node:fs';
const args=process.argv.slice(2),state=JSON.parse(readFileSync(process.env.KATA_PI_TEST_STATE,'utf8'));
appendFileSync(process.env.KATA_PI_TEST_CALLS,JSON.stringify({executable:process.argv[1],args,cwd:process.cwd(),server:process.env.KATA_SERVER,author:process.env.KATA_AUTHOR,teammate:process.env.KATA_TEAMMATE,recipient:process.env.KATA_INBOX_USER})+'\n');
if(args[0]==='--json'&&args[3]==='daemon') {
 const address=process.env.KATA_SERVER;
 process.stdout.write(JSON.stringify({network:'tcp',address,request_base_url:address.includes('://')?address:`http://${address}`}));
} else if(args[0]==='--json'&&args[3]==='show') {
 const [project,issue]=args[4].includes('#')?args[4].split('#',2):['spoke-project',args[4]];
 process.stdout.write(JSON.stringify({issue:{project_id:project==='spoke-project'?42:43,uid:`${project}:${issue}`}}));
} else if(args[0]==='agent-contract-hook') {
 if(state.failContract)process.exit(1);
 process.stdout.write(JSON.stringify({hookSpecificOutput:{additionalContext:state.contract}}));
} else if(args[0]==='inbox') {
 if(state.failInbox)process.exit(1);
 process.stdout.write(state.inbox);
} else if(args[0]!=='agent-hook')process.exit(2);
