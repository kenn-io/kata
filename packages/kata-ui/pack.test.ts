import { execFileSync } from "node:child_process";
import { mkdtempSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, expect, test } from "vitest";

const temporaryDirectories: string[] = [];

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("published tarball contains only the shared UI runtime", () => {
  const destination = mkdtempSync(join(tmpdir(), "kata-ui-pack-"));
  temporaryDirectories.push(destination);
  execFileSync("bun", ["pm", "pack", "--destination", destination], {
    cwd: import.meta.dirname,
    stdio: "pipe",
  });
  const archives = readdirSync(destination).filter((name) => name.endsWith(".tgz"));
  expect(archives).toHaveLength(1);

  const entries = execFileSync("tar", ["-tf", join(destination, archives[0])], {
    encoding: "utf8",
  })
    .trim()
    .split("\n")
    .filter(Boolean)
    .sort();

  expect(entries).toContain("package/package.json");
  expect(entries).toContain("package/README.md");
  expect(entries).toContain("package/src/index.ts");
  expect(entries).toContain("package/src/IssueDetail.svelte");
  expect(entries.every((entry) => entry.startsWith("package/"))).toBe(true);
  expect(entries.some((entry) => entry.endsWith(".test.ts"))).toBe(false);
  for (const excluded of ["node_modules/", "web/", ".env", ".cache/", "schema.d.ts"]) {
    expect(entries.some((entry) => entry.includes(excluded))).toBe(false);
  }
});
