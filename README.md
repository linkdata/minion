# linkdataminion

[![build](https://github.com/linkdata/minion/actions/workflows/build.yml/badge.svg)](https://github.com/linkdata/minion/actions/workflows/build.yml)
[![coverage](/../gitcoverage/main/badge.svg)](https://github.com/linkdata/minion/tree/gitcoverage/main)

A small Go service that watches a bot account's GitHub notifications in an explicit repository whitelist for commands. It invokes the authenticated Codex or Claude CLI for the work and uses Git and the GitHub API for publication. The checked-in [config.json](config.json) contains sample settings; copy it to the ignored `config.local.json` for a real deployment.

## Goal

Minion should start one AI-assisted action from a configured command, without free-form instructions in the command. Only collaborators with write or stronger access should be able to request work, and only in repositories explicitly selected by the operator or automatically registered from invitations by pinned owners. GitHub text and repository files are untrusted input; the default anti-prompt-injection skill should help the models keep them separate from operator instructions. AI-assisted work should run in sandboxes. Application logic, rather than AI instructions, should choose and limit GitHub operations, destinations, and publication states.

The security boundary is the operator-controlled configuration and mechanical checks on repository scope, requester permissions, and permitted actions, together with sandbox restrictions, resource limits, and application-controlled GitHub writes. These controls must hold even when the model follows an injected instruction. [Deployment isolation validation](https://github.com/linkdata/minion/issues/7) remains a public-release follow-up.

The embedded `builtin://anti-prompt-injection` skill is enabled by default for both model roles. GitHub titles and descriptions are quoted as untrusted task data in model prompts. Quoting and skill instructions are best-effort defenses against prompt injection; they do not enforce the security boundary. The skill tells each model to use untrusted content as evidence for the selected task, including relevant requirements and prior decisions, without letting instructions inside it change the selected action, authorization rules, permissions, publication destination, or publication state.

Minion does not use a separate prompt-injection classifier: the operator judges its false refusals and operational complexity to outweigh its benefits ([decision #86](https://github.com/linkdata/minion/issues/86)).

## What it does

Put one enabled command on the first nonblank line of an issue or pull request body or comment. You may add task notes on later lines; the command itself accepts only the optional `history` modifier, with no free-text arguments. These defaults use `@linkdataminion`; substitute the configured `bot_login`. The `commands` mapping defines the available names and AI skills; each repository's `actions` list selects which names it accepts.

| Command | Result |
| --- | --- |
| `@linkdataminion review` | On a pull request, the main model uses the ponytail-review skill to review its exact head against its base. The bot submits a comment review; it never approves. |
| `@linkdataminion fix` | On an issue, the main model uses the ponytail skill to edit a pinned source snapshot of the default branch. The bot pushes a deterministic branch to its own fork and gives the requester a compare link to open the pull request themselves. |
| `@linkdataminion audit` | On an issue or pull request, the main model uses the ponytail-audit skill for a read-only source audit and posts its report in the requesting thread. |
| `@linkdataminion security-audit` | On an issue or pull request, the main model uses Cloudflare's security-audit skill. The bot validates its report artifacts and posts the findings summary in the requesting thread. |

Place the optional `history` modifier immediately after the command name, for example `@linkdataminion review history`. The only accepted modifier is the single case-insensitive token `history`; additional tokens are refused. The modifier is part of the request identity. Models receive bounded source snapshots without Git metadata or history by default. The modifier supplies up to 100 commits and 1 MiB of commit metadata, without historical patches, with explicit complete/truncated coverage. Pull requests receive the same comparison context with or without history; see [action boundary](#action-boundary).

Fix replies record the expected branch head. Before enabling `fix` for a repository, create a fork under `bot_login` with the same repository name, disable GitHub Actions on that fork, and verify that no external CI, webhooks, or installed apps run its branch pushes with privileged credentials. Minion verifies the fork's parent and visibility, checks that its Actions are disabled, and refuses private fork PR settings that send base secrets or write tokens. The bot credential needs access to the base repository and its private fork PR settings; if the fork or settings cannot be read, `fix` does not publish; retries follow the [failure policy](#failure-policy). Private fork PR workflows may need to be enabled on the base repository to run CI after the requester opens the PR; keep secrets and write tokens disabled. Review any base `pull_request_target` workflows before enabling `fix`: that trigger can receive base credentials and must not execute untrusted fork-head code. Minion cannot prove that those workflows or external CI are safe.

The requester must open the pull request from their own account after reviewing the diff, so GitHub can apply any independent-review rule to that requester. The branch remains mutable afterward; protect `minion/fix-*` in the fork from updates by other writers and compare its current head with the recorded commit before merging. This private repository's current plan cannot enforce those protections or required reviews, so a second-person review remains a manual requirement here. Minion publishes fix branches to its fork and can retry a failed fork publication.

Before generating or pushing a fix, Minion reads effective push rulesets on the fork, monitored repository, and fork-network root. It allows active push rules only when GitHub exposes an explicitly empty bypass list, so the bot cannot bypass a restriction another fork would enforce. An unreadable or incomplete ruleset response prevents fix publication. An HTTP 403 before model work follows infrastructure retry/backoff without consuming an attempt or guaranteeing a reply; a 403 on the pre-push recheck is a publication failure and consumes an attempt. GitHub currently returns 403 for this private repository's rulesets on its plan, so fixes keep retrying until a trustworthy result is available. Branch rulesets on a base repository do not apply to its forks; push rulesets and their root bypass permissions do.

Each request must come from an account with effective repository `write`, `maintain`, or `admin` permission. An optional `allowed_actors` list narrows that group. Only the first nonblank line can start a command; quoted, fenced, hidden, or later directives cannot start one. A second full-line configured command refuses the request as ambiguous. Notes after an authorized command reach the main model as quoted task guidance within the selected action; they cannot change the bot's application-enforced limits. A review also includes the pull request title and description as untrusted context, even when the pull request author cannot run Minion. Fixes and audits use relevant thread descriptions as untrusted context. The bot checks the whitelist and command author's permission again before every action.

`max_github_context_bytes` limits the combined UTF-8 bytes of trimmed title, description, and requester notes included in an action (32 KiB when omitted or 0; configurable from 1 to 67,108,864 bytes). It counts these fields before prompt quoting, not the complete model prompt or repository files. Text at the limit passes this check; a larger total gets a fixed refusal before either model starts. Untrimmed titles over 1 KiB get the same refusal. Shorten the text and post a new command.

Edited command sources are ignored. Post a new comment to request work after editing text.

For `fix`, Minion reads GitHub's rendered issue text and checks the issue's edit time against the request. If the issue changed at or after the request, it refuses the fix; review the current issue and post a new `fix` comment.

After accepting an authorized command, the bot adds a 👀 reaction to the exact issue, pull request, comment, or review that contains it, before starting the main model. If the reaction fails, it retries the command on the next poll.

The `review` action is limited to pull requests, and `fix` to issues. The bot replies when those commands are used in the other supported thread type. `comment` and `security-audit` actions can be requested in an issue or pull request.

The `security-audit` action requires Codex as main, with Cloudflare's skill, the standard profile, independent agents, and its six-phase reporting workflow. Claude can serve as its critic. Source remains read-only; validated report artifacts return as data and are retained in a new directory under `work_root/audits/`, with its path logged in `journalctl`. Returned audit metadata identifies the target as `owner/repository@commit` rather than an executor-local path. Security-audit runs retain normal Codex session logs so helper agents can fork the parent context. The GitHub reply includes the audited commit and findings summary, not a downloadable attachment. Audit agents are instructed to inspect source and use the skill's report validators, leaving repository code, builds, tests, fixtures and live services unexecuted. The sandbox permits local commands, so the source-only execution rule currently relies on those instructions. Minion checks the validator scripts and Node's permission model before invoking the audit model, then runs the validators under those restrictions: file reads are limited to the installed skill and copied validation files, child processes are denied, and the environment contains only a scratch `HOME` and `PATH`. Node 20–24 does not restrict network access through this model, so the skill source must be trusted. Claims needing runtime evidence remain `needs_validation`. Missing tools or time limits must be disclosed as incomplete coverage. The usual `job_timeout_minutes` applies; a timeout removes the executor workspace. Collected partial audit artifacts are retained under `work_root/audits/incomplete-*` with an `INCOMPLETE.txt` label. Incomplete executor artifacts are not published. Retained audit files are best effort across host crashes; a `complete-*` name denotes validated audit status, not crash durability. Validator errors identify the failed script and exit cause without copying report-derived stderr into the journal.

## Failure policy

A failed request gets at most three execution attempts, with these exceptions:

| Failure | Policy |
| --- | --- |
| Graceful SIGINT/SIGTERM cancellation before publication; unsafe work-root admission; GitHub API HTTP 401/403/408/429/5xx or request transport errors (`*url.Error`) before publication | Retry on later polls without consuming an attempt. Crashes and hard kills consume the recorded attempt. |
| Action timeout; unsupported action/source or oversized source; non-capacity source/context materialization failure; invalid job, prompt, main proposal, critic verdict or result; invalid collection/import or audit report validation; oversized model/fix output; frozen critic input mutation; critic snapshot/history preparation failure after model work; exhausted critic rounds; a fix producing no commit; post-call usage or review-evidence save failure; completed-audit retention failure | No further attempts. |
| Other provider, Git transport, audit-tool prerequisite, commit/push/publication or initial/pre-call usage save failures; initial review-evidence save ENOSPC/EDQUOT | Consume an attempt and may repeat model work. Publication of model results, including recovery of an already pushed fix, consumes an attempt even when shutdown interrupts it. Fixed replies and refusals follow the pre-publication rules. Other initial review-evidence failures allow no further attempts. |
| Final usage save failure after successful publication | Log a warning and retain the last durable counters; the published action stays successful. |

Response-body read/decode failures consume an attempt. Graceful signal refunds require a supervisor that signals only Minion first, as the shipped `KillMode=mixed` service does; signalling children directly can race cancellation reporting.

Minion posts a fixed reply after terminal failure or exhausted attempts. After three failed comment posts, it logs the request URL and moves on. Repository lookup or identity-verification failures retry without consuming that reply-post budget. Post a new command after fixing the cause.

## Setup

Minion requires Linux. Install Go 1.23 or newer, `gh`, Git 2.29 or newer (minimum untested; no startup version check), and the CLI for each enabled model role on the host that will run the service (`codex` by default). Security audits also need Node.js 20 or newer for the [permission model](https://nodejs.org/download/release/v24.20.0/docs/api/permissions.html) used by the skill's report validators, plus a main model with sub-agent support for that skill. Minion selects the supported permission flag at runtime. Keep the configuration and standard native login/configuration paths outside model read grants, including unused providers' stored credentials. Startup checks their resolved locations, including symlink targets; selected public binary and skill subtrees remain usable. Copy the sample configuration, set its bot login and repository whitelist, then authenticate as that bot in the same operating-system account or service environment. Add `invitation_owners` only if the bot should accept invitations from those owners:

```sh
cp -n config.json config.local.json
chmod 600 config.local.json
# Edit config.local.json before continuing.
gh auth status
# If codex.api_key is empty, verify the existing login:
codex login status
go build -buildvcs=false -o bin/minion ./cmd/minion
./bin/minion -config ./config.local.json -install-skills
mkdir -p var
# Provision and mount a dedicated, bounded filesystem at var/work (see below).
./bin/minion -config ./config.local.json -check
```

The `gh` token needs access to [GitHub notifications](https://docs.github.com/en/rest/activity/notifications?apiVersion=2022-11-28) and the whitelisted repositories. GitHub's notifications endpoint requires a classic personal access token or OAuth token; fine-grained tokens and GitHub App tokens do not work there. The bot needs repository **write** access so it can query [collaborator permission](https://docs.github.com/en/rest/collaborators/collaborators?apiVersion=2022-11-28) and push fix branches to its fork. Fixes also need access to the base repository's private fork PR policy and the fork's Actions settings; an unreadable policy causes refusal. GitHub requires the classic token's `repo` scope for those settings; its fine-grained equivalent is repository Administration (read), though fine-grained tokens cannot poll notifications. When push rulesets exist, the token needs ruleset write access for GitHub to include their bypass lists. `-check` verifies notification and repository access, including fork and push-rule safety when `fix` is enabled. GitHub credentials are read from `GH_TOKEN`/`GITHUB_TOKEN` or `gh auth token`. AI provider API keys can be set in `codex.api_key` and `claude.api_key` in the ignored `config.local.json`; see authentication below. Keep real keys out of the checked-in sample and set `chmod 600 config.local.json`.

Edit `config.local.json` to change the repositories monitored for commands or their enabled actions. Each `repositories` entry needs an `owner/repo` name and its GitHub `id` and `owner_id`. `actions` lists the command names allowed in that repository, drawn from the top-level `commands` mapping. `allowed_actors` can restrict commands to named committers in addition to the GitHub permission check; each listed login also needs its numeric ID in `allowed_actor_ids`. `invitation_owners` lists accounts whose current and future repositories may invite the bot; each listed login needs its numeric ID in `invitation_owner_ids`. Invitations with `write`, `maintain`, or `admin` access automatically add the repository and its IDs to `config.local.json` with the configured command names. A command with a missing required skill stays unavailable until it is installed. Monitoring starts in the same poll without a restart. Existing repository actions and actor restrictions are preserved. `start_at` may be set to an RFC3339 timestamp *before the first run* to process older notifications. Without it, the first run starts at the current time.

When a configured command uses the `fix` action, a repository added from an invitation enables it. Provision its bot-owned fork with Actions disabled before fixes can publish; `-check` reports a missing or unsafe fork.

Confirm that each repository and account is still the one you intended to trust, then look up its immutable GitHub ID with authenticated `gh` commands, for example:

```sh
gh api repos/OWNER/REPO --jq '{id, owner_id: .owner.id}'
gh api users/OWNER --jq .id
gh api users/ACTOR --jq .id
```

Copy those values into `id`, `owner_id`, `invitation_owner_ids`, and `allowed_actor_ids` as applicable. The sample `config.json` contains placeholder IDs. A missing, extra, or nonpositive pin stops startup; Minion does not infer trust from a login that another account may have claimed. Pending retries without IDs cannot run; they are discarded when their notification is seen or their retry becomes due.

The top-level `commands` object maps each command name to an application `action` and one or more skill sources in `skills`. Supported actions are `review`, `fix`, `comment`, and `security-audit`. The application action, not the skill, determines whether Minion posts a review or thread comment, pushes a fix branch, or writes a validated security report. Omit `commands` or set it to `null` to use the four mappings in [config.json](config.json); set it to `{}` to disable all commands. Every repository's `actions` list must name configured commands. A mapped command runs only when all its required skills are available to the main provider: downloaded skills need matching Minion install metadata, while the embedded `builtin://anti-prompt-injection` skill needs no installation. Custom GitHub sources use `https://github.com/OWNER/REPO/tree/REF/skills/NAME`, with a single path-segment `REF`; the embedded URI is the only supported `builtin://` source.

The `always_on_skills` list selects optional skills for every run. Omit it or set it to `null` for the default embedded anti-prompt-injection skill and caveman GitHub skill, in that order; set it to `[]` to use none. An external always-on skill is used when installed and skipped otherwise. The main model receives its command skills and installed always-on skills. The critic receives only installed always-on skills, so a command skill cannot turn its read-only critique into another execution of the task.

`-install-skills` copies configured embedded skills and downloads configured GitHub skills into `$HOME/.agents/skills/` for Codex and `$HOME/.claude/skills/` for Claude, then exits. The embedded anti-prompt-injection skill works in Minion jobs without this switch; copying it also makes it available to interactive Codex and Claude. The installer resolves each GitHub ref to its current commit, installs its complete skill directory, and records the fetched source and commit. The default `main` URLs therefore get their latest versions each time. GitHub skill repositories must be publicly readable because the installer fetches them without credentials. It replaces existing same-name skill directories in both locations, including manually installed copies; keep local customizations under other names. Run it explicitly when adding or updating skills; ordinary Minion runs never download them. Review each external source and fetched commit: skill instructions guide the models, and Minion runs the security-audit skill's validator scripts under the service account.

Stop Minion before editing its configuration. Install skills and run `-check` after editing it.

### Main model and adversarial critic

`main` selects `"codex"` (the default) or `"claude"`. `adversarial` is optional; omit it or use `""` to disable the critic. When enabled, it must select the other CLI. The `codex` object accepts `model`, `reasoning_effort`, and `api_key`; `claude` accepts `model`, `effort`, `api_key`, and `allow_all_unix_sockets`. Omitted model settings use the provider's defaults. For example:

```json
{
  "main": "codex",
  "adversarial": "claude",
  "critic_max_rounds": 5,
  "critic_nonblocking_rounds": null,
  "codex": {
    "api_key": ""
  },
  "claude": {
    "api_key": "",
    "allow_all_unix_sockets": true
  }
}
```

Add these fields to the service config. Swap the two provider names to use Claude as main and Codex as critic. Install both selected CLIs, configure API keys or authenticate them interactively, then run `-check`; an unused CLI is not required.

A nonempty `api_key` takes precedence over environment credentials and saved CLI login for startup validation and job execution. Use an [OpenAI API key](https://learn.chatgpt.com/docs/auth#sign-in-with-an-api-key) for Codex and an [Anthropic API key](https://code.claude.com/docs/en/authentication#authentication-precedence) for Claude. With keys empty or omitted, Codex uses its environment credentials or saved login; Claude uses its saved login. Configured keys are not placed in prompts or command arguments, and do not replace the operator's saved login. Minion preserves them when automatically registering a repository in the deployment config.

Claude support requires Linux, Claude Code 2.1.302 or later, `bubblewrap`, and `socat`. Its restricted execution tool set contains only Bash; the `security-audit` action is disabled with Claude as main because that workflow requires independent subagents. Review, fix, and comment actions support either main provider.

Claude runs in [safe mode](https://code.claude.com/docs/en/cli-reference) with automatic instruction discovery, hooks, plugins, MCP, and connected tools disabled. Only configured skills are named in its prompt. Minion configures its [shell sandbox](https://code.claude.com/docs/en/sandboxing) with explicit filesystem grants, read-only metadata and skills, credential-variable denial, separate process/network namespaces, an empty network allowlist, and no unsandboxed fallback. Claude's own API client retains authentication and network access. Minion disables the CLI's print-mode background wait ceiling (`CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0`), so the action deadline controls background work. Both roles are instructed to await delegated work and background checks before returning a final response. Sandboxed Bash is pre-approved with `dontAsk` and an explicit Bash allowance; this does not use bypass-permissions mode or depend on an approval classifier.

`claude.allow_all_unix_sockets` defaults to `true`, including when omitted or `null`. It passes `sandbox.network.allowAllUnixSockets=true` to Claude, bypassing the seccomp helper that fails on affected Linux AppArmor/bubblewrap combinations ([upstream issue](https://github.com/anthropics/sandbox-runtime/issues/429)). This disables Unix-socket filtering: sandboxed commands can access sockets visible through their filesystem grants, potentially reaching local services despite the empty network allowlist. Filesystem and network namespace restrictions still apply. Set it to `false` on unaffected hosts or after an upstream fix to restore filtering. The setting applies whether Claude is the main model or the critic.

For a configured Claude key, Minion supplies a temporary credential helper and private authentication directory outside the sandbox's filesystem grants. The key is absent from Claude's environment and the files are removed after the call. Without a configured key, use `claude auth login`; Minion rejects nonempty `ANTHROPIC_API_KEY` and `CLAUDE_CODE_OAUTH_TOKEN` environment variables because native credential denial can leave them readable in the Bubblewrap supervisor's environment. Authenticated proxy URLs are also rejected. These restrictions apply with either value of `allow_all_unix_sockets`; ordinary proxy URLs without credentials remain supported for Claude's API client.

Claude preflight checks CLI prerequisites, authentication, and the initialization protocol's model catalogue, without inference or executing a sandboxed command. Native sandbox startup errors abort the job. Claude's runtime isolation remains a deployment validation requirement. A successful metadata check alone is not proof of runtime isolation.

With a critic configured, the main model returns a public `answer` and private `refutations`. The first critic reviews the complete candidate against the original task and baseline. After a revision, scoped verification checks earlier findings against the changed files and private counterevidence; accepted verification triggers a separate fresh full review. Full reviews receive no previous findings, answers or refutations. Only an accepted fresh full review permits publication. The main model must implement valid findings at every severity, including nits and simplifications, or supply concrete counterevidence or a task constraint. These are model instructions, not mechanically verified guarantees.

Cross-round memory appends only revised main proposals' `refutations` field (round 2 onward), with its original round and exact private text, rather than answer, critic-feedback or verdict entries. The first proposal must leave refutations empty; preemptive first-round text is discarded. The model is instructed to exclude implemented-finding summaries and approvals from its prose. Each revision and scoped verification also receives the immediately previous answer and critic feedback. They reuse applicable counterevidence; changed code, changed task constraints or material new evidence can invalidate an earlier refutation. Changed code remains open to review. Main-model refutations are claims, not inferred critic acceptance.

The service atomically checkpoints call receipts and the refutation list under `work_root/reviews/<attempt-id>.json` before another call or publication, including failed actions. An initial metadata checkpoint verifies storage after work-root admission. Files use mode `0600` in a private `0700` directory outside model grants. Selected refutation copies reach only their revision/verification call and are removed afterward. The journal logs identity, location and refutation count without private text. Retention failure stops model work and publication; identical final evidence does not rewrite the checkpoint. Records do not mean publication succeeded or resume interrupted actions. Operators manage retained capacity; a volatile work root loses records on reboot. Complete discussion logging remains separate [#98](https://github.com/linkdata/minion/issues/98)/[#99](https://github.com/linkdata/minion/issues/99) work.

Fix critics inspect a frozen candidate; read-only actions use their unchanged source. Audit critics inspect frozen raw reports. Verification and full review of one proposal share immutable inputs and the original baseline, not the previous candidate tree. Minion rejects mutation of the frozen critic inputs and exports their saved bytes; later live workspace changes cannot alter that export. Filesystem access enforcement belongs to the AI CLI sandbox. Audit metadata is projected to supported fields: portable target/companion paths, ownership entries for known artifacts and configured skill source; unknown extensions are dropped. Report prose remains unchanged. Missing/empty artifacts can receive feedback; unsafe, unreadable or oversized inputs stop before review. Model agreement does not replace validation or publication controls.

`critic_max_rounds` counts main proposals, including revisions: default 5 when omitted or 0, range 1–20. N rounds allow at most 3N−1 main/critic calls. `critic_nonblocking_rounds` accepts 0 through that maximum. Omitted or `null` defaults to maximum minus one (4 by default; 0 for a one-round maximum, 1 for a two-round maximum). Explicit 0 requires every finding cleared through the hard limit. At or after a positive threshold, a fresh full review with no blockers permits publication despite remaining nonblocking findings. Before it, every finding must be implemented or evidence-refuted. Blockers are concrete correctness, security or task failures; stylistic nits and simplifications without that impact are nonblocking. Every review still reports all severities. The critic's actual approval and blocker attestations stay separate from the application's acceptance decision. Per-call `has_blockers`/`no_blockers` evidence distinguishes blocking and nonblocking verdicts from an invalid verdict or provider failure (both false). Failures after a native call, such as snapshot verification or usage persistence, retain its parsed verdict. Journal `policy_met` reports only whether a call's verdict meets its round's policy, including verification; it does not mean the candidate was published. See the [failure policy](#failure-policy) for exhausted budgets.

`job_timeout_minutes` covers preparation, model calls and publication, uses 240 minutes when omitted or 0 and accepts 1–1440. Admission, the reaction, cleanup and short fixed refusal/failure-reply budgets can run outside this deadline. The sequential service pauses notification polling and invitation checks during a job, so a stall can occupy that full budget. Separate inactivity and hard-runtime settings remain [#97](https://github.com/linkdata/minion/issues/97) work.

With a critic, both CLIs receive schemas for main proposals and critic verdicts; Minion validates their contents. Decoded main answer/refutations share 58 KiB, raw proposal JSON allows 6×58 KiB+128 bytes for escaping, and critic JSON allows 64 KiB. NULs are stripped from main answers; a blank answer cannot complete a review, comment or audit. Without a critic, the main model keeps its ordinary response format. Every task states the rendered publication budget. Oversized output is refused; a no-critic fix with valid changes can discard its unused summary.

Codex starts in a clean temporary project root beside the supplied source snapshot and reads source through its absolute path. Source `AGENTS.md` files are not loaded as project instructions, and source `.agents/skills` are not offered as skills. The automatic skills catalogue is hidden; only skills selected by Minion's config are named in its prompt. The source has no `.git` metadata. Even writable jobs can only read the source's `.agents`, `.codex`, and `.claude` paths; both CLIs also protect the top-level `.git` from writes if it appears. Startup checks instruction and skill discovery with inert files using the installed Codex CLI, without an inference turn, and stops if those files enter an isolated job prompt. Repository source text remains untrusted context that can influence model output; inspect generated fixes and reports before relying on them.

When Codex is selected for either role, every startup, including `-check`, verifies its instruction boundary, queries the installed Codex CLI's [model catalogue](https://learn.chatgpt.com/docs/app-server#models), and validates the model and its supported reasoning efforts. Omitted values are resolved from that catalogue and pinned for the life of the service. Invalid model settings, an incompatible Codex protocol, a failed instruction check, or a failed catalogue lookup stop startup before notification polling or state changes; the log includes the rejected value and supported choices when available. Missing required skills disable their mapped commands; missing always-on skills are skipped. Model discovery has a 30-second timeout and does not start an inference turn. The systemd service retries failed startups every 30 seconds, so a removed model or effort requires correcting `config.local.json` or the Codex installation before work resumes. Restart Minion after upgrading Codex or installing skills to revalidate. Catalogue validation checks compatibility, not future API availability or account quota.

Run it as a service:

```sh
./bin/minion -config ./config.local.json -loop
```

For a user service, copy [`deploy/minion.service`](deploy/minion.service) to `~/.config/systemd/user/minion.service`. Set its absolute checkout path in `WorkingDirectory` and `ExecStart`, and add the installed `gh` and selected AI CLI directories to `PATH` if needed. Then run `systemctl --user daemon-reload` and `systemctl --user enable --now minion.service`. Enable user lingering if it must continue after logout. After rebuilding the binary, restart it:

```sh
systemctl --user restart minion.service
systemctl --user status minion.service
journalctl --user -u minion.service -n 50 --no-pager
```

Or run one pass each minute from cron (use absolute paths, and ensure that cron has access to the same GitHub and selected AI CLI authentication):

```cron
* * * * * /path/to/minion/bin/minion -config /path/to/minion/config.local.json >> /path/to/minion/var/minion.log 2>&1
```

State, job clones and source snapshots live under `var/`. Each action uses atomically created temporary directories with collision-retrying random names under the host-controlled work root. Keep `work_root` outside `/tmp`, which the command sandbox hides. **Mount a dedicated filesystem at `work_root` with capacity at most `work_root_max_bytes` (8 GiB by default), separate from the `state_path` filesystem.** Startup, `-check` and each job refuse a missing, unmounted, shared, oversized or nearly full root (less than 64 MiB free). Startup/`-check` reject overlap with system, executable or skill grants; the local `Runner` checks selected provider grants per execution. Retained `work_root/reviews/<attempt-id>.json` records and `work_root/audits/complete-*` and `incomplete-*` reports need operator cleanup to free space. The 64 MiB check is not a reservation: a critic-reviewed fix may hold multiple source copies plus Git metadata, tool caches and reports. Allow several hundred MiB per fix. Result-transfer bounds and output prechecks do not cap model-created disk usage or concurrent I/O; executor resource isolation remains [#96](https://github.com/linkdata/minion/issues/96) work.

For example, an operator can mount a 2 GiB tmpfs at `/path/to/minion/var/work` with a finite inode count. Create the underlying mount point as root-owned mode `0555`, then mount it with ownership and mode `0700` for the Minion user. Configure the mount to persist across reboots, and verify it with `findmnt /path/to/minion/var/work` and `df -h /path/to/minion/var/work` before starting the service. A tmpfs loses audit reports on reboot; use a dedicated non-thin block filesystem if they must persist. Do not use a sparse loop image backed by the state filesystem: its backing file could still fill that filesystem. The read-only underlying mount point prevents writes from falling through if the mount disappears. If you set a larger `work_root_max_bytes`, keep the actual filesystem capacity within that value.

A file lock prevents overlapping cron runs. The process persists GitHub's minimum `X-Poll-Interval`, so an early cron pass exits without polling. The GitHub client and both AI CLIs need outbound network access to their APIs. **Model-generated shell commands** run in a restricted workspace with no allowed network destinations; Claude's Unix-socket workaround permits access to visible local sockets as described above. The Go service owns authenticated GitHub writes: it reacts to accepted commands before the models run and publishes allowed results afterward. Invitations use their own pinned authorization policy within the same sequential monitor.

Invitation checks become due every 15 minutes and run when the sequential monitor regains control. The service checks [pending repository invitations](https://docs.github.com/en/rest/collaborators/invitations?apiVersion=2022-11-28). It accepts nonexpired invitations addressed to the configured bot when the repository owner matches a pinned `invitation_owners` ID or the repository matches a monitored name and pinned repository and owner IDs. The owner in GitHub's repository record must match its full name. Invitations to monitored repositories still need `write`, `maintain`, or `admin` access; invitations to other allowed-owner repositories may grant any standard role. Read and triage invitations are accepted without enabling commands. Invitations from unlisted owners are declined unless their numeric owner or repository identity is pinned. Malformed, expired, wrong-invitee, and pinned identity mismatch invitations stay untouched. Each check fetches at most ten pages and gives listing and invitation API actions separate 30-second budgets, so a large invitation backlog does not hold notification polling indefinitely. Fetched pages are handled after a later page fails unless GitHub returns HTTP 403 or 429; those responses defer further invitation actions until the next check. New writable repositories are saved atomically to the config before acceptance, so a failed config write leaves the invitation pending for retry. An acceptance failure can leave the entry in the config while acceptance retries. The config file and its directory must be writable; stop the service before editing config to avoid concurrent updates. Invitations accepted before automatic registration was installed need a manual repository entry. This automates the invited account's acceptance step; a repository owner must still send the invitation.

## Action boundary

The AI CLIs enforce model-command sandboxing. Minion configures their grants, validates supplied inputs and returned results, and controls action lifecycle. The orchestrator prepares source and controls GitHub writes. The local `Runner` consumes a bounded, credential-free `Job` and returns changes, reports, usage and diagnostics in `Result`. These are local Go values; transport, strict decoding, authenticated delivery, remote failure-class serialization and cross-message `Status` ordering remain [#96](https://github.com/linkdata/minion/issues/96) work.

Key contract and execution limits:

| Data | Limit |
| --- | --- |
| Published answer / `REPORT.md` | 58 KiB raw UTF-8 bytes and 58 KiB after line normalization, `> ` prefixes and 12 fence bytes |
| Title; combined title/description/notes | 1 KiB; 64 MiB |
| Identity segments / diagnostic codes | 1–100 ASCII characters from `[A-Za-z0-9._-]`, except `.` and `..` |
| Requester login | Same base spelling, optionally followed by one `[bot]` suffix; numeric identity and repository authorization remain required |
| Model/effort; skills | 128 UTF-8 bytes each, no NUL/CR/LF; 64 required+always-on sources per role, 4096 bytes per source |
| Job runtime; critic budget | Greater than zero, at most 24 hours; at most 20 proposals with a critic, nonblocking threshold 0 through that maximum |
| Diagnostics | 100 per Result and Status contract (Status is defined but not emitted; production/transport is #96 work); 8 KiB per UTF-8 message, no NUL |
| Complete audit | Seven named regular files; 5 MiB per non-summary file; `REPORT.md` uses the published-text limit |
| Transferred head / merge-base regular files | 10,000 per list; 64 MiB of contents across both |
| Changed paths; distinct head/base omissions | 10,000 each |
| Comparison diff; history metadata | 1 MiB each; history at most 100 commits, no patches |
| Complete default/head/merge-base tree | 10,000 blob/gitlink paths, 64 MiB declared blob bytes, 100,000 metadata entries |
| GitHub tree metadata response | 8 MiB |
| Fix result; binary patch | 100 changes, 1 MiB of full changed-file contents including mode-only changes; separate 1 MiB patch limit includes removed/deleted contents and encoding overhead |
| Fix control files / source scans | 64 MiB per `.gitignore`/`.gitattributes` before fix collection; 100,000 files plus distinct ancestor directories per admitted layout, and 100,000 visited entries in fix/frozen-input walks |
| Provider stream | 64 MiB, 100,000 records; 4 MiB per JSONL event |
| Codex rollout accounting | 128 MiB per session; 512 MiB across the session tree |
| Codex rollout discovery / completion | 1,024 recent sessions; 100,000 entries; 64 KiB header; 4 MiB per line; 256 helpers |

PR context always contains changed paths and merge-base versions of changed/deleted files, alongside pinned head source and separate base-tip identity. An unused latest base tip is excluded. Diffs ignore repository attributes; an oversized diff alone is omitted and marked incomplete. Complete-tree limits include omitted links and apply to the entire merge base, even when only selected files transfer; shrinking an over-limit or unsupported base does not bypass them. Clone/fetch history has no object/byte cap; the deadline and dedicated filesystem bound it.

Symlinks/gitlinks are omitted and declared in `source.json`; its single `omitted_scope` applies to the whole snapshot (`head` for issues or `head_and_base` for pull requests). A base-only omission may have regular descendants in head. Fixes cannot modify omissions/descendants or replace their ancestor directories with files. Exact Git spelling is preserved, including case and backslashes; unrepresentable source is refused. Git source admission rejects NTFS/HFS aliases of `.git`. Fixes additionally require backslash-free regular paths and a final layout without Unicode SimpleFold collisions, including omissions. Normalization/reserved names are not folded. Executors require Linux; components allow 255 UTF-8 bytes and relative paths fewer than 4096 bytes. Host filesystem and absolute-root limits still apply, so executor acceptance does not guarantee trusted import or cross-host compatibility.

Models see tracked `.gitignore` files. A private offline index outside model grants collects fixes without remotes, credentials, hooks, filters or source Git config. Tracked edits count; ignored new build outputs do not. Case-insensitive `.git` components stay excluded. Wholly untracked nested repositories must be removed or ignored. Traversed ignore/attributes controls must be regular files; an ignored untracked `.gitignore` is refused if Git loads it but would not export it. Unused controls in ignored directories with tracked files are allowed. Skipped ignored subtrees consume no scan entries beyond their visited parent. Orchestration rechecks the fork and validates imported data before its commit/push; executor Git metadata never transfers.

Codex uses a standalone profile with explicit source/context/skill grants. Admission mirrors the Linux `:minimal` read roots verified in Codex 0.160.0; **Minion does not check the Codex version**. Re-verify grant behavior after upgrades, including readable children beneath masked ancestors. Every Codex model call disables cross-session memories. Native provider directories are resolved once and pinned for each launch; Codex uses the same directory for rollout recovery. Set `CODEX_HOME` or `HOME` so known Codex helpers can be verified.

Native completion is separate from accounting. An oversized provider event cannot be recovered by a later answer. Known Codex helpers require proof that their latest own turn completed; intentional close/interrupt, missing or unreadable logs, inherited-only entries and capped tails that prevent that proof reject completion. A completed attributable helper lifecycle with missing counters only leaves usage incomplete. Cancellation kills CLI process groups; normal exit does not stop or prove the absence of remaining descendants. A two-second pipe-drain bound limits waiting on inherited output pipes after leader exit or cancellation; exceeding it rejects the call, including an otherwise completed answer. It does not prove children exited: same-group or detached descendants can remain alive. Reliable ownership remains [#96](https://github.com/linkdata/minion/issues/96) work.

Ordinary tool output beginning with `bwrap:` or `apply-seccomp:` is not treated as proof of sandbox startup failure. Minion still requests fail-closed native sandbox policy; metadata and error-text checks alone do not establish runtime isolation.

Claude checks bounded stderr for wait-ceiling stops while retaining its last 8 KiB; managed settings may override the launch environment. Non-ambient stopped tasks require a fresh root response and result, and a result with a non-`completed` terminal reason cannot complete a call; a later completed result may. An unusable quota rejection or permanent native memory-pressure stop gets one non-renewable second to flush counters/result before cancellation; an earlier action deadline still wins. Later allowed events cannot erase interrupted work; usable overage and near-limit warnings do not fail a successful call. Reset times are informational.

The journal records role, round, phase, model settings, accounting completeness and diagnostic codes. Follow-ups: admission/capacity [#95](https://github.com/linkdata/minion/issues/95), isolated local/remote executors [#96](https://github.com/linkdata/minion/issues/96), inactivity/max runtime [#97](https://github.com/linkdata/minion/issues/97), discussion capture and limit diagnostics [#98](https://github.com/linkdata/minion/issues/98), restricted records [#99](https://github.com/linkdata/minion/issues/99), usage/billing exports [#100](https://github.com/linkdata/minion/issues/100), dashboard [#101](https://github.com/linkdata/minion/issues/101), and role selection [#102](https://github.com/linkdata/minion/issues/102), under plan [#91](https://github.com/linkdata/minion/issues/91).

## Live tests

Create the ignored `live-tests.local.json` in the repository root to enable live tests. Without it, they skip:

```json
{
  "owner_login": "example-owner",
  "bot_login": "example-bot",
  "repository_url": "https://github.com/example-owner/example-repo"
}
```

Replace the sample values with two accounts authenticated in `gh` and a repository they can read. Optional `issue_number` selects an owner-authored issue with an owner comment; `pull_request_number` selects an open owner-authored draft pull request with an owner comment. The `linkdata/miniontesting` pull request fixture also needs a bot-authored inline review comment and submitted review with a non-empty body. Optional `bot_ssh_key` is an absolute path to the bot's SSH private key or a path relative to the repository root; it enables SSH identity and repository read checks. Put no token or key contents in this file.

Run `go test -race ./internal/github ./internal/gitwork -run '^(TestLiveSmoke|TestLiveClientReadOnly|TestLiveBotGitAccess|TestLiveCloneAndPullRequest)$' -count=1`. Keep `-count=1` so Go does not reuse a cached result. These tests read GitHub through REST, read-only GraphQL queries, SSH authentication, and Git fetches without changing GitHub state. They verify both accounts' `gh` credentials and, when configured, the bot's SSH key. The Git checkout test uses only `linkdata/miniontesting` and skips for other repositories.

Stop any running minion service before a write run so it cannot accept the test invitation or act on test events. The write run uses both accounts' `gh` credentials and the dedicated `linkdata/miniontesting` repository:

```sh
MINION_LIVE_WRITE=1 go test -race ./internal/github -run '^TestLiveBotWriteLifecycle$' -count=1
```

The write test reuses `linkdataminion`'s write access, accepts a suitable pending invitation, or invites it when neither exists. It keeps the resulting access for later runs. The test posts and edits a bot issue comment, reacts to a temporary owner comment, and, when `bot_ssh_key` is set, pushes a temporary branch and opens a draft pull request. Set `MINION_LIVE_REVIEW=1` as well to post a comment review on the configured fixture pull request. Cleanup deletes both comments, the reaction, and the temporary branch and closes the temporary pull request. GitHub retains submitted reviews and closed pull request records. Cancel an expired bot invitation in Collaborators settings before another write run.

After bot write access is established, create `linkdataminion/miniontesting` as a fork of `linkdata/miniontesting` and disable GitHub Actions on the fork. Exercise the production Git publishing path with `MINION_LIVE_WRITE=1 go test -race ./internal/gitwork -run '^TestLiveCommitAndPush$' -count=1`. It creates one branch on the bot fork, verifies its commit and bot identity, and deletes the branch after checking its exact SHA.

Exercise the production actions with stub Codex results:

```sh
MINION_LIVE_WRITE=1 go test -race ./internal/action -run '^(TestLiveFixPublishesBranch|TestLiveSecurityAuditAction)$' -count=1
```

The fix test publishes a temporary branch to `linkdataminion/miniontesting` and a reply on the base issue, checks a repeat execution, then removes both after identity checks. The security-audit test checks both issue and draft pull request heads, validates its report, checks a repeat execution, and removes its replies. Replies can leave GitHub notifications after cleanup.

To exercise the production review action, run `MINION_LIVE_WRITE=1 MINION_LIVE_REVIEW=1 go test -race ./internal/action -run '^TestLiveReviewAction$' -count=1`. It reviews the configured owner-authored draft pull request at its exact head, submits a comment review, and checks that a repeat execution does not submit another. GitHub retains the submitted review.

If a write test is interrupted before cleanup, inspect `linkdata/miniontesting` for temporary `minion/live-*` branches and bot replies carrying its job marker. Inspect `linkdataminion/miniontesting` for temporary `minion/live-gitwork-*` and `minion/fix-*` branches. Remove only artifacts created by that run. Notification delivery and the monitor's invitation handling depend on GitHub account settings and are covered by offline integration tests; the live suite checks the repository notifications API.

The invitation and revocation lifecycle has a separate opt-in test. Run it only when explicitly requested, with `MINION_LIVE_WRITE` unset and `linkdataminion` lacking collaborator access or a pending invitation; it removes the access it grants:

```sh
MINION_LIVE_COLLABORATION=1 go test -race ./internal/github -run '^TestLiveBotCollaborationLifecycle$' -count=1
```

## Usage reports

The service records token usage for each action attempt, including failures and retries. Reports are local and need no GitHub or AI authentication or network access. They can run while the service is running:

```sh
# Group by repository, invoking user, and action (the default).
./bin/minion usage -config ./config.local.json

# October usage for one repository and invoking user.
./bin/minion usage -repo example-owner/example-repo -user example-user -from 2026-10-01 -to 2026-11-01

# Daily totals by repository.
./bin/minion usage -group-by day,repo

# Export individual attempts, with their model, effort, outcome, and source URL.
./bin/minion usage -group-by none -json > usage.json

# Read records directly if the service configuration is unavailable or invalid.
./bin/minion usage -dir ./var/usage -group-by month,user -json
```

Both the service and the usage command default to `config.local.json` when `-config` is omitted. `-repo` and `-user` match case-insensitively. `-from` is inclusive and `-to` is exclusive; both filter the attempt's start time. Dates use midnight UTC, and RFC3339 timestamps are also accepted. `-group-by` accepts a comma-separated combination of `repo`, `user`, `action`, `day`, and `month`; day and month groups use UTC. Use `none` for one row per attempt. JSON includes the applied filters, grouped summaries or individual attempts, and overall totals.

Reports include both providers and all main/critic rounds, with models, efforts and success/failure/running/incomplete states. Codex recovers root/helper session counters; Claude uses the CLI's native per-model `modelUsage` totals without retained sessions. Claude input includes uncached input, cache creation and reads. **Cached input is a subset of input**: total tokens are input+output, not input+cache+output. Query counters are not credits or a billing statement. Missing/damaged/oversized records retain incomplete lower bounds; zero incomplete usage does not mean a free call. Oversized Codex rollout lines are skipped to recover later counters. Recovery uses the live action context, with no fresh budget after cancellation. If already canceled before recovery, retained stream counters can be zero/incomplete despite counters on disk; bounded post-stop recovery remains [#97](https://github.com/linkdata/minion/issues/97) work after [#96](https://github.com/linkdata/minion/issues/96) can prove process quiescence. Application validation failure does not invalidate an already completed call's accounting.

Records contain metadata/counts, not prompts/replies. A `running` attempt can remain after a crash, hard kill, panic or failed final save. Graceful cancellation records `failed`. A pre-call checkpoint records dispatch intent; cancellation there can leave an incomplete call count without proving a charged request. Retries are separate attempts. Accounting starts when installed and does not reconstruct historical usage.

Records are stored in a `usage/` directory beside `state_path` (`var/usage/` by default), independently of notification state. Keep this directory to retain usage history. `-config` loads and validates the local configuration but does not run service preflight; `-dir` bypasses configuration entirely and cannot be combined with `-config`. A missing usage directory produces an empty report.

## Why polling

GitHub does not offer a webhook for a user account's personal mentions. The [notifications API](https://docs.github.com/en/rest/activity/notifications?apiVersion=2022-11-28) is the simplest route for a dedicated machine user: a paginated poll for each whitelisted repository, no public server, and no repository webhook administration. This service requests read and unread notifications since a durable cursor for each repository, obeys `X-Poll-Interval`, then inspects content in whitelisted threads. Notification `reason` is thread-level, so the service validates the actual command in each new body/comment and deduplicates by source item plus command, not by notification ID.

Initial issue and pull request descriptions and their comments, pull request reviews and line comments are scanned. Edits to an old issue or pull request description are not treated as new commands, because REST `updated_at` also changes for unrelated thread activity. GitHub notification delivery settings still control what reaches this account. Discussions are unsupported because [their notification subjects can have no URL](https://github.com/orgs/community/discussions/62728), leaving no reliable thread ID. If you later need guaranteed coverage of every edit, discussion support, or lower latency, install repository or [GitHub App webhooks](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks) for the relevant events. A GitHub App posts as its own app bot identity, rather than the configured bot account.

The service stores no access token on disk. `var/state.json` tracks repository cursors, completed command IDs, failed job attempts, pending failed threads, and verified fix branch SHAs for crash recovery. The account cursor seeds a repository cursor when that repository is newly whitelisted. If a repository notification poll fails, only its cursor stays in place; the other repositories and pending retries continue. A repository with more than 2,000 notifications in one poll fails closed at 20 pages, preserving its cursor for intervention. A failed thread is retried from its original scan time with backoff up to 64 minutes while the cursor continues past other notifications. A permanently inaccessible thread remains in the pending list and is logged with its URL; an operator can remove that entry after checking it for valid commands. Generated code changes are pushed to branches for requesters to turn into pull requests.
