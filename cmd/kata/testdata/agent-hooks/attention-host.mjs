import assert from 'node:assert/strict';
import {createInterface} from 'node:readline';
import {pathToFileURL} from 'node:url';

const {kataAttentionHostSkip, kataAttentionHostClaim, kataAttentionHostRelease} =
  await import(pathToFileURL(process.argv[2]));
for await (const line of createInterface({input: process.stdin})) {
  const input = JSON.parse(line);
  Object.defineProperty(process, 'pid', {value: input.ownPid, configurable: true});
  if (input.operation === 'claim-release') {
    process.env.KATA_REF = 'preserved-ref';
    kataAttentionHostClaim(input.identity);
    const claimed = JSON.parse(process.env.KATA_ATTENTION_HOST);
    const replacements = {
      own: process.env.KATA_ATTENTION_HOST,
      pid: JSON.stringify({...claimed, pid: input.ownPid + 1}),
      identity: JSON.stringify({...claimed, identity: input.identity + '-other'}),
      reordered: JSON.stringify({identity: input.identity, pid: input.ownPid}),
      extended: JSON.stringify({...claimed, extra: true}), malformed: '{',
    };
    if (input.replacement === 'absent') delete process.env.KATA_ATTENTION_HOST;
    else process.env.KATA_ATTENTION_HOST = replacements[input.replacement];
    const before = process.env.KATA_ATTENTION_HOST;
    kataAttentionHostRelease(input.identity);
    console.log(JSON.stringify({...claimed, released: process.env.KATA_ATTENTION_HOST === undefined,
      unchanged: before === process.env.KATA_ATTENTION_HOST, kataRef: process.env.KATA_REF}));
    continue;
  }
  if (input.marker === undefined) delete process.env.KATA_ATTENTION_HOST;
  else process.env.KATA_ATTENTION_HOST = input.malformed ?
    ['{', 'null', '[]', '{}', 'true', '"text"', '',
      ...[0, -1, 0.5, 'pid', null, true, {}, 2147483648, Number.MAX_VALUE]
        .map(pid => JSON.stringify({...input.marker, pid})),
      ...[null, true, 1, {}, []].map(identity => JSON.stringify({...input.marker, identity}))][input.marker.pid % 21] :
    JSON.stringify(input.marker);
  process.kill = (pid, signal) => {
    if (!input.malformed) assert.equal(pid, input.marker.pid);
    assert.equal(signal, 0);
    if (pid > 2147483647) throw new RangeError('pid outside Node process.kill range');
    if (!input.alive || input.denied) {
      throw Object.assign(new Error('liveness'), {code: input.alive ? 'EPERM' : input.denied ? 'EIO' : 'ESRCH'});
    }
    return true;
  };
  console.log(JSON.stringify({skip: kataAttentionHostSkip(input.identity)}));
}
