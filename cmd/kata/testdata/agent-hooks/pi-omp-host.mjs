import assert from 'node:assert/strict';
import {pathToFileURL} from 'node:url';

const [extension] = process.argv.slice(2);
const factory = (await import(pathToFileURL(extension))).default;
const events = [];
factory({zod: {}, on(name) { events.push(name); }, async exec() { assert.fail('OMP host must not run Kata commands'); }});
assert.deepEqual(events, [], 'the Pi adapter leaves an OMP host to its own adapter');
