---
name: roast
description: Adversarial cross-model review in which a finder proposes defects and an independent verifier checks each claim against the code. Use after a coherent, non-trivial code or prose change, before declaring the work complete, or when the operator asks for a Roast review.
---

# Roast

Roast reviews local Git changes with two independently configured models. The
finder looks aggressively for defects; the verifier re-reads the cited code and
confirms or refutes each candidate before it reaches the final report.

## Setup and trust boundary

Roast sends the selected diff, repository context, prompts, and tool results to
the inference endpoints in the user's Roast configuration. It may also upload a
complete run bundle when uploads are explicitly enabled. These destinations are
user-controlled and may be third-party services. Do not assume they are trusted
for private source code: inspect the configuration or run `roast doctor`, and
follow the operator's data-handling policy.

Configure profiles in the user config file shown by `roast doctor`, following
the profile example in the Configure section of the Roast README
(https://github.com/bluesky-social/roast#configure). Use
`roast config init --from <url>` only with a trusted configuration URL that the
operator supplies. A profile selects an endpoint, model, wire protocol
(`responses`, `chat`, or `anthropic`), credential environment variable, and
output limit. The finder and verifier may use different profiles and protocols.

`roast skill install` installs this skill into the shared Agent Skills location
and any detected Claude Code or Polytoken installation. `roast skill status`
reports whether each installed copy matches the binary. On the first interactive
review in a repository, Roast asks once whether to install its native pre-commit
gate and records the answer in local Git config. Non-interactive runs never
install it without recorded consent. The gate is managed explicitly with
`roast gate enable|status|disable`.

## When to review

Review coherent, non-trivial changes such as features, refactors, concurrency,
protocol changes, and important bug fixes. Skip formatting-only changes and
minor documentation corrections. Review before committing with `--staged`, or
use `--revision <rev>` to review one specific committed change, or `--base <ref>`
for the existing cumulative review since a base.

When the operator names a revision, pass it verbatim as `--revision <rev>`;
`--task` is descriptive text, not revision selection. Roast automatically uses
jj revsets inside colocated Git-backed jj workspaces and Git refs elsewhere.
For a request to review the current jj change, use `--revision @`. Do not
substitute the latest described
change, Git HEAD, or another revision. If resolution fails or is ambiguous,
report that and ask for clarification rather than running a different review.
Both diff and file context are pinned to the selected revision. Merge commits
are compared with their first parent; initial commits with the empty tree.

## Invocations

```sh
roast review --task "what changed and why"
roast review --staged --task "what changed and why"
roast review --revision <rev> --task "what changed and why"
roast review --quick --task "draft review"
roast review --interactive --task "review with repository navigation"
roast review --scope repo --task "survey this repository"
roast runs
roast runs show <run-id>
roast doctor
```

`--quick` skips independent verification and is a draft result. Repository
survey mode is prioritized sampling, not exhaustive coverage. A `NO_DIFF`
result means no review ran; it is not a clean review.

Roast writes JSON to stdout and a Markdown report to stderr by default. Exit 0
means the command completed without a finding at the requested gate threshold;
exit 1 means a finding met that threshold; exit 2 is invalid usage or config;
exit 3 is authentication; exit 4 is an operational or resource failure; and
exit 5 is cancellation.

Every non-empty review records a durable local run. `roast runs status` reports
upload state and `roast runs sync` retries eligible pending uploads. Replay and
evaluation tooling are intentionally not part of the `roast` command.

## Handling findings

Check every finding against the cited code. State whether it is genuine,
refuted, or a deliberate tradeoff and explain what will be changed. A verifier
refutation is expected behavior, not a failure. Do not silently discard a
confirmed finding.
