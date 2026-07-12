# Vercel Docs Deployment Context Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restrict Vercel docs deployments to required docs inputs, remove redundant and local-only upload content, and align the live project settings with the self-contained docs build.

**Architecture:** A repository-root `.vercelignore` will define the upload boundary while preserving the root-linked CLI workflow and pre-hydrated screenshots. The existing parser-backed Vercel checker will evaluate representative paths against the ordered ignore policy so future changes cannot silently widen the deployment context. The live Vercel project settings will be updated separately through the authenticated API and read back for verification.

**Tech Stack:** Vercel CLI/configuration, Python 3 standard library, Bash/Make docs checks.

---

### Task 1: Implement and document the bounded deployment policy

**Files:**
- Create: `.vercelignore`
- Modify: `docs/scripts/check_vercel_redirects.py`
- Modify: `docs/development/deploying-docs.md`
- Delete: `docs/superpowers/specs/2026-07-12-vercel-docs-deployment-context-design.md`
- Commit: `docs/superpowers/plans/2026-07-12-vercel-docs-deployment-context.md`

- [ ] **Step 1: Add an ordered Vercel-ignore policy evaluator and representative paths**

Add `fnmatch` to the checker imports, define `VERCELIGNORE = ROOT.parent / ".vercelignore"`, and add functions that load non-empty, non-comment rules and evaluate them in order. Directory rules apply to the named path and descendants; glob rules use `fnmatch.fnmatchcase`; negated rules re-include matching paths.

```python
def load_vercelignore() -> list[str]:
    try:
        lines = VERCELIGNORE.read_text(encoding="utf-8").splitlines()
    except FileNotFoundError:
        fail("missing repository-root .vercelignore")
    return [line.strip() for line in lines if line.strip() and not line.lstrip().startswith("#")]


def ignore_rule_matches(rule: str, path: str) -> bool:
    pattern = rule.lstrip("/").rstrip("/")
    if pattern == "*":
        return True
    if any(character in pattern for character in "*?["):
        return fnmatch.fnmatchcase(path, pattern)
    return path == pattern or path.startswith(f"{pattern}/")


def deployment_includes(rules: list[str], path: str) -> bool:
    ignored = False
    for raw_rule in rules:
        negated = raw_rule.startswith("!")
        rule = raw_rule[1:] if negated else raw_rule
        if ignore_rule_matches(rule, path):
            ignored = not negated
    return not ignored
```

In `main()`, require these paths to remain included:

```python
required_paths = (
    "docs/index.md",
    "docs/vercel.json",
    "docs/assets/screenshots/tui/hero.svg",
)
```

Require these representative paths to be excluded:

```python
excluded_paths = (
    "cmd/kata/main.go",
    ".superpowers/sdd/progress.md",
    "docs/site/index.html",
    "docs/superpowers/plans/example.md",
    "docs/.venv/bin/zensical",
    "docs/.cache/build-entry",
    "docs/.env.local",
    "docs/.env.preview.local",
    "docs/.vercel/project.json",
    "docs/__pycache__/module.pyc",
    "docs/scripts/__pycache__/checker.pyc",
    "docs/.zensical-build.example.toml",
    "docs/zensical-public-docs.example/index.md",
)
```

Fail with a path-specific message when any representative path has the wrong classification.

- [ ] **Step 2: Run the checker and verify RED**

Run: `python3 docs/scripts/check_vercel_redirects.py`

Expected: exit 1 with `FAIL: missing repository-root .vercelignore`.

- [ ] **Step 3: Add the docs-only `.vercelignore` allowlist**

Create this ordered policy:

```gitignore
# The Vercel project is linked at the repository root but builds from docs/.
/*
!docs

# Build and machine-local state under the allowed docs tree.
docs/site
docs/.venv
docs/.cache
docs/.env*.local
docs/.vercel
docs/__pycache__
docs/**/__pycache__/**
docs/.zensical-build.*
docs/zensical-public-docs.*
docs/superpowers
```

Do not exclude `docs/assets/screenshots`: the Vercel source bundle has no Git metadata, so the build requires the deployment helper's pre-hydrated assets.

- [ ] **Step 4: Run the checker and verify GREEN**

Run: `python3 docs/scripts/check_vercel_redirects.py`

Expected: exit 0 and `vercel redirect checks passed`.

- [ ] **Step 5: Document the upload boundary and remove the transient spec**

Add a short repository-configuration paragraph to `docs/development/deploying-docs.md` explaining that the root-linked CLI deployment uses the root `.vercelignore`, only the `docs/` project and hydrated screenshots are uploaded, and generated/local state is excluded. State that the Vercel project must keep “Include source files outside of the Root Directory” disabled because the build is self-contained.

Delete `docs/superpowers/specs/2026-07-12-vercel-docs-deployment-context-design.md` as requested before publication.

- [ ] **Step 6: Run the complete docs contract check**

Run: `make docs-check`

Expected: exit 0 after both isolated and local docs builds complete.

- [ ] **Step 7: Review and commit the implementation**

Review `git status --short`, `git diff HEAD`, and recent repository commit style. Follow the mandatory commit skill, stage only the files listed for this task, and create a rationale-first commit without amending the existing design checkpoint.

### Task 2: Correct and verify the live Vercel project settings

**Files:**
- No repository files.

- [ ] **Step 1: Read linked identifiers without printing them**

Run from the original linked repository checkout, not the isolated worktree. It
contains the existing local Vercel link:

```bash
project_id=$(jq -r '.projectId' .vercel/project.json)
org_id=$(jq -r '.orgId' .vercel/project.json)
```

- [ ] **Step 2: Disable outside-root source and clear the stale ignored-build command**

Use the authenticated Vercel CLI to send a JSON PATCH without printing either identifier:

```bash
printf '%s' '{"sourceFilesOutsideRootDirectory":false,"commandForIgnoringBuildStep":null}' \
  | vercel api "/v9/projects/$project_id" --scope "$org_id" \
      -X PATCH --input - --silent
```

If the API rejects `null`, retry only the command-clearing field with an empty string while retaining `sourceFilesOutsideRootDirectory: false`:

```bash
printf '%s' '{"sourceFilesOutsideRootDirectory":false,"commandForIgnoringBuildStep":""}' \
  | vercel api "/v9/projects/$project_id" --scope "$org_id" \
      -X PATCH --input - --silent
```

- [ ] **Step 3: Read back the settings**

Run:

```bash
vercel api "/v9/projects/$project_id" --scope "$org_id" --raw 2>/dev/null \
  | jq '{rootDirectory, sourceFilesOutsideRootDirectory, commandForIgnoringBuildStep}'
```

Expected: `rootDirectory` is `docs`, `sourceFilesOutsideRootDirectory` is `false`, and `commandForIgnoringBuildStep` is `null` or an empty string.

### Task 3: Final verification and publication

**Files:**
- Review all committed branch changes.

- [ ] **Step 1: Run repository verification against the committed branch**

Run:

```bash
make docs-check
go test ./...
git diff --check origin/main...HEAD
```

Expected: every command exits 0.

- [ ] **Step 2: Confirm the final tree omits the design spec**

Run: `git diff --name-status origin/main...HEAD`

Expected: `.vercelignore`, the checker, deployment docs, and this implementation plan are present; the design spec is absent from the final tree.

- [ ] **Step 3: Run the public-data scrub**

Scan the complete diff, unpushed commits and messages, implementation plan, and proposed PR title/body with the configured private-terms denylist and structural checks. Require zero hits.

- [ ] **Step 4: Push and open the PR**

Follow the mandatory commit-push-pr workflow. Use a rationale-first PR body with no testing or verification section. Close kata issue `zntp` only after the final commit and verification are complete, attaching the commit SHA.
