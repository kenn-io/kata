import { createOmpHost } from "./omp-host.mjs";
const schema = new Proxy(() => schema, { get: () => schema });
export function planHost(
  file,
  { events = {}, cwd = process.cwd(), entries = [], ...options } = {},
) {
  const h = createOmpHost({ cwd, events, ...options });
  h.zod = schema;
  const widgets = [];
  h.ctx.ui.setWidget = (...args) => widgets.push(args);
  h.appendEntry = (customType, data) =>
    entries.push({ type: "custom", customType, data });
  h.getSessionName = () => "Example plan";
  h.ctx.sessionManager.getBranch = () => entries;
  const register = h.registerTool;
  h.registerTool = (t) => register({ ...t, sourceInfo: { path: file } });
  return Object.assign(h, {
    widgets,
    entries,
    call: (params, signal) =>
      h.tools
        .find((t) => t.name === "todo")
        .execute("test", params, signal, undefined, h.ctx),
  });
}
