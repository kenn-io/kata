import { expect, test, runTests, loadCore } from "./plan-test-utils.mjs";
const { kataCommands } = await loadCore();
test("the configured executable variable is recognized without evaluating shell code", () => {
  expect(kataCommands('"$KATA_OMP_EXECUTABLE" list')).toHaveLength(1);
  expect(kataCommands('"${KATA_OMP_EXECUTABLE}" list')).toHaveLength(1);
});
test(
  "store and home overrides route claim commands away from the default target",
  () => {
    for (const assignment of [
      "KATA_HOME=/tmp/alternate-home",
      "KATA_DB=/tmp/alternate.db",
      "KATA_DSN=postgres://example@localhost/alternate",
    ]) {
      const [invocation] = kataCommands(
        `${assignment} kata claim example-workspace#abcd`,
      );
      expect(invocation.routedByEnvironment).toBe(true);
    }
  },
);
await runTests();
