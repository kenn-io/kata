#!/usr/bin/env node
import {readFileSync,appendFileSync} from 'node:fs';
const args=process.argv.slice(2),state=JSON.parse(readFileSync(process.env.KATA_PI_TEST_STATE,'utf8'));
appendFileSync(process.env.KATA_PI_TEST_CALLS,JSON.stringify({args,cwd:process.cwd(),server:process.env.KATA_SERVER,author:process.env.KATA_AUTHOR,teammate:process.env.KATA_TEAMMATE,recipient:process.env.KATA_INBOX_USER})+'\n');
if(args[0]==='agent-contract-hook') {
 if(state.failContract)process.exit(1);
 process.stdout.write(JSON.stringify({hookSpecificOutput:{additionalContext:state.contract}}));
} else if(args[0]==='inbox') {
 if(state.failInbox)process.exit(1);
 process.stdout.write(state.inbox);
} else if(args[0]!=='agent-hooks')process.exit(2);
