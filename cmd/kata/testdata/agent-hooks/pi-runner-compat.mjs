// Pi 1.3 takes (trigger, options); Pi 0.99 took (prompt, images, options).
export const emitBeforeAgentStart=(runner,prompt,options)=>runner.emitBeforeAgentStart.length>=3?runner.emitBeforeAgentStart(prompt,undefined,options):runner.emitBeforeAgentStart({messageType:'user',prompt},options);
