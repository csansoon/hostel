---
name: pr-ready
description: Take a Hostel pull request to "ready to merge". Use it every time you create a pull request, and when the owner asks to finish, review, or babysit one. It runs a local subagent review, fixes the findings, follows the reviewers' comments and CI, and tells the owner when the pull request is ready.
---

# Pull request to "ready to merge"

The owner wants this for every pull request that you create (owner instruction,
2026-10-26). Do the steps without asking for approval between them. Fixes go to
the same pull request as new commits; push them at once. Do not merge.

## 1. Local review by a subagent

Start a new subagent (general-purpose) for an independent review of the whole
pull request. Give it:

- the pull request number and branch, and the command for the diff:
  `git diff origin/main...HEAD`;
- `AGENTS.md` for the rules, and the docs that apply (`docs/design.md`, the
  milestone file `docs/m<N>-implementation.md`);
- what to look for: correctness bugs, missing deadlines, leaks of cloud machines
  (cost), restarts in the middle of a step, races, tests that do not test what
  they claim;
- strict limits: read-only, no commits, no comments on GitHub, no Docker, no
  E2E tests, no real provider CLIs (`devbox`, `nsc`: they cost money); it may run
  `mise exec -- go build ./...` and `mise exec -- go vet -tags e2e ./...`;
- the report format: `file:line`, one-sentence defect, a concrete failure
  scenario, confidence; most severe first; under 600 words.

Do not give it your own conclusions about the code.

## 2. Fix the findings

For each finding, check it in the code yourself. Then:

- A real defect: fix it with an E2E test when the E2E tests can show it
  (`AGENTS.md`, testing rules). Check that the test fails on the old code.
- Not a defect: write down why (for the final report).

Commit at logical steps, run `make lint` and the affected E2E tests, and push.

## 3. Comments of reviewers on GitHub

List all review threads and comments, also from bots (for example Codex):

```sh
gh api graphql -f query='query{repository(owner:"csansoon",name:"hostel"){pullRequest(number:N){reviewThreads(first:100){nodes{id isResolved path line comments(first:20){nodes{databaseId author{login} body}}}} comments(first:100){nodes{author{login} body}} reviews(first:50){nodes{author{login} state body}}}}}'
```

For each open thread:

- Fixed: reply with the commit and what changed (and the test), then resolve:
  `gh api repos/csansoon/hostel/pulls/N/comments/<id>/replies -f body=...`
  and the GraphQL mutation `resolveReviewThread(input:{threadId:"..."})`.
- Not fixed: reply with the reason, and leave the thread open for the owner.

New pushes can bring new bot reviews. Check the threads again after each push
and after CI ends, until no new comment comes.

## 4. CI

- Wait for all checks: `gh pr checks N --watch`.
- When a milestone ends, or the code changed after the last gate, start the
  stability gate: remove and add the label `e2e-10` (workflow "E2E gate").
- A failure: read the log, fix the cause (not only the symptom), push, and
  wait again. A failure from the environment (for example a runner problem):
  run it again once, and report it.

## 5. Tell the owner

Only when all of these are true:

- the subagent's findings are fixed or explained;
- every review thread is resolved or has a reply that explains why not;
- all CI checks pass on the last commit (and the gate, when it applies);
- the branch merges without conflicts (`gh pr view N --json mergeable`).

Then report in ASD-STE100 (`AGENTS.md`): the pull request link, the head commit,
what the review found and what you changed, the open threads with the reason,
the CI and gate results, and anything not verified. Do not merge; the owner
merges.
