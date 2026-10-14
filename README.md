# linkdataminion

[![build](https://github.com/linkdata/minion/actions/workflows/build.yml/badge.svg)](https://github.com/linkdata/minion/actions/workflows/build.yml)
[![coverage](/../gitcoverage/main/badge.svg)](https://github.com/linkdata/minion/tree/gitcoverage/main)

A small Go service that watches a bot account's GitHub notifications in an explicit repository whitelist for commands. It invokes the authenticated Codex CLI for the work and uses the GitHub API for publication. The checked-in [config.json](config.json) contains sample settings; copy it to the ignored `config.local.json` for a real deployment.

## Goal

Minion should start one AI-assisted action from a fixed command, without free-form instructions in the command. Only collaborators with write or stronger access should be able to request work, and only in repositories owned by explicitly allowed GitHub accounts. GitHub text and repository files are untrusted input; prompt boundaries should reduce injection. AI-assisted work should run in sandboxes. Application logic, rather than AI instructions, should choose and limit GitHub operations, destinations, and publication states.

Current gaps are tracked in [owner allowlisting](https://github.com/linkdata/minion/issues/76), [prompt boundaries](https://github.com/linkdata/minion/issues/77), and [repository instruction discovery](https://github.com/linkdata/minion/issues/78). [Deployment isolation validation](https://github.com/linkdata/minion/issues/7) is also open before public release.

## What it does

Put one command on the first nonblank line of an issue or pull request body or comment. You may add task notes on later lines; the command itself accepts no free-text arguments. These examples use `@linkdataminion`; substitute the configured `bot_login`:

| Command | Result |
| --- | --- |
| `@linkdataminion review` | On a pull request, Codex reviews its exact head against its base. The bot submits a comment review; it never approves. |
| `@linkdataminion audit` | In an issue, audits the default branch; in a PR, audits its exact head. Invokes the configured security-audit skill on the full source tree and posts a Markdown summary in the requesting thread. |
| `@linkdataminion fix` | On an issue, Codex edits a fresh checkout of the default branch. The bot pushes a deterministic branch to its own fork and gives the requester a compare link to open the pull request themselves. |
| `@linkdataminion release v1.2.3` | Codex drafts release notes from local Git history. The bot creates a **draft GitHub release** for the new tag, pinned to the checked-out commit. It never publishes the release. |

Release drafts require an active GitHub tag ruleset covering **all tags** (`~ALL` with no exclusions), restricting both updates and deletions, and granting no bypass. Minion checks all active tag rulesets through GitHub's API before reserving the tag and creating the draft. It refuses when any active tag ruleset has a rule beyond update, deletion, or non-fast-forward protection, even if the ruleset targets a different tag pattern, so the bot cannot bypass a restriction on the requester. The bot token must be able to see the integrity ruleset's bypass list; GitHub hides it from tokens without ruleset write access. An unavailable API, hidden bypass list, or insufficient repository plan causes release requests to be refused. Send a new command after protection is available. Minion reserves the tag at the inspected commit before creating the draft. An administrator can change the ruleset later; verify the tag before publishing a draft.

Fix replies record the expected branch head. Before enabling `fix` for a repository, create a fork under `bot_login` with the same repository name, disable GitHub Actions on that fork, and verify that no external CI, webhooks, or installed apps run its branch pushes with privileged credentials. Minion verifies the fork's parent and visibility, checks that its Actions are disabled, and refuses private fork PR settings that send base secrets or write tokens. The bot credential needs access to the base repository and its private fork PR settings; if the fork or settings cannot be read, `fix` refuses to publish. Private fork PR workflows may need to be enabled on the base repository to run CI after the requester opens the PR; keep secrets and write tokens disabled. Review any base `pull_request_target` workflows before enabling `fix`: that trigger can receive base credentials and must not execute untrusted fork-head code. Minion cannot prove that those workflows or external CI are safe.

The requester must open the pull request from their own account after reviewing the diff, so GitHub can apply any independent-review rule to that requester. The branch remains mutable afterward; protect `minion/fix-*` in the fork from updates by other writers and compare its current head with the recorded commit before merging. This private repository's current plan cannot enforce those protections or required reviews, so a second-person review remains a manual requirement here. Review and retire bot-authored fix PRs and `minion/fix-*` branches in base repositories; Minion publishes fix branches to its fork. A recorded fix with no matching fork branch is refused if its legacy base branch still exists; otherwise Minion can retry the failed fork publication.

Before generating or pushing a fix, Minion reads effective push rulesets on the fork, monitored repository, and fork-network root. It allows active push rules only when GitHub exposes an explicitly empty bypass list, so the bot cannot bypass a restriction another fork would enforce. An unreadable or incomplete ruleset response refuses `fix`. GitHub currently returns HTTP 403 for this private repository's rulesets on its plan, so `fix` remains unavailable even after the bot fork is provisioned until GitHub exposes a trustworthy result. Branch rulesets on a base repository do not apply to its forks; push rulesets and their root bypass permissions do.

Each request must come from an account with effective repository `write`, `maintain`, or `admin` permission. An optional `allowed_actors` list narrows that group. Only the first nonblank line can start a command; quoted, fenced, hidden, or later directives cannot start one. A second full-line Minion directive refuses the request as ambiguous. Notes after an authorized command reach Codex as quoted task guidance within the selected action; they cannot change the bot's application-enforced limits. A review also includes the pull request title and description as untrusted context, even when the pull request author cannot run Minion. Fixes and audits use relevant thread descriptions as untrusted context. The bot checks the whitelist and command author's permission again before every action.

Edited command sources are ignored. Post a new comment to request work after editing text.

For `fix`, Minion reads GitHub's rendered issue text and checks the issue's edit time against the request. If the issue changed at or after the request, it refuses the fix; review the current issue and post a new `fix` comment.

After accepting an authorized command, the bot adds a 👀 reaction to the exact issue, pull request, comment, or review that contains it, before starting Codex. If the reaction fails, it retries the command on the next poll.

Jobs with execution failures run at most three times, except GitHub authentication, rate-limit, server, and network failures, which retry on later polls. Timeouts, invalid audit output, Codex output limits, and oversized fixes stop after one run. Minion posts a fixed failure reply; if GitHub rejects that reply three times, it logs the request URL and moves on. Post a new command after fixing the cause.

`review` is limited to pull requests, and `fix` to issues. The bot replies when those commands are used in the other supported thread type. `audit` and `release` can be requested in an issue or pull request. Existing tags are refused so a draft cannot silently point at a different commit.

Audits use Cloudflare's security-audit skill with the standard profile, independent agents, and its six-phase reporting workflow. Source remains read-only; reports are retained in a new directory under `work_root/audits/` and its path is logged in `journalctl`. Audit runs retain normal Codex session logs so helper agents can fork the parent context. The GitHub reply includes the audited commit and findings summary, not a downloadable attachment. Audits use source inspection and the skill's trusted report validators; repository code, builds, tests, and live services are not executed because this runner does not provide the skill's required isolation and resource limits for target code. Claims needing runtime evidence remain `needs_validation`. Missing tools or time limits must be disclosed as incomplete coverage. The usual `job_timeout_minutes` applies; a timeout leaves partial local artifacts and stops automatic retries.

## Setup

Install Go 1.23 or newer, `gh`, `git`, and `codex` on the host that will run the service. Audits also need Node.js for the skill's report validators and Codex sub-agent support. Copy the sample configuration, set its bot login and repository whitelist, then authenticate as that bot in the same operating-system account or service environment. Add `invitation_owners` only if the bot should accept invitations from those owners:

```sh
cp -n config.json config.local.json
# Edit config.local.json before continuing.
gh auth status
codex login status
go build -buildvcs=false -o bin/minion ./cmd/minion
mkdir -p var
# Provision and mount a dedicated, bounded filesystem at var/work (see below).
./bin/minion -config ./config.local.json -check
```

The `gh` token needs access to [GitHub notifications](https://docs.github.com/en/rest/activity/notifications?apiVersion=2022-11-28) and the whitelisted repositories. GitHub's notifications endpoint requires a classic personal access token or OAuth token; fine-grained tokens and GitHub App tokens do not work there. The bot needs repository **write** access so it can query [collaborator permission](https://docs.github.com/en/rest/collaborators/collaborators?apiVersion=2022-11-28), push fix branches to its fork, and create draft releases. Fixes also need access to the base repository's private fork PR policy and the fork's Actions settings; an unreadable policy causes refusal. GitHub requires the classic token's `repo` scope for those settings; its fine-grained equivalent is repository Administration (read), though fine-grained tokens cannot poll notifications. When push or tag rulesets exist, the token needs ruleset write access for GitHub to include their bypass lists. `-check` verifies notification and repository access, including fork and push-rule safety when `fix` is enabled. Credentials are read from `GH_TOKEN`/`GITHUB_TOKEN` or `gh auth token`; Codex uses its existing login or `CODEX_API_KEY`. Do not put tokens in either configuration file.

Edit `config.local.json` to change the repositories monitored for commands or their enabled actions. Each `repositories` entry needs an `owner/repo` name and its GitHub `id` and `owner_id`. `allowed_actors` can restrict commands to named committers in addition to the GitHub permission check; each listed login also needs its numeric ID in `allowed_actor_ids`. `invitation_owners` lists accounts whose current and future repositories may invite the bot; each listed login needs its numeric ID in `invitation_owner_ids`. Invitations with `write`, `maintain`, or `admin` access automatically add the repository and its IDs to `config.local.json` with `review`, `fix`, and `release` enabled, plus `audit` when security-audit is configured; fixes and releases still produce drafts. Monitoring starts in the same poll without a restart. Existing repository actions and actor restrictions are preserved. `start_at` may be set to an RFC3339 timestamp *before the first run* to process older notifications. Without it, the first run starts at the current time.

A repository added from an invitation has `fix` enabled. Provision its bot-owned fork with Actions disabled before fixes can publish; `-check` reports a missing or unsafe fork.

Existing configs must be updated before starting this version. Confirm that each repository and account is still the one you intended to trust, then look up its immutable GitHub ID with authenticated `gh` commands, for example:

```sh
gh api repos/OWNER/REPO --jq '{id, owner_id: .owner.id}'
gh api users/OWNER --jq .id
gh api users/ACTOR --jq .id
```

Copy those values into `id`, `owner_id`, `invitation_owner_ids`, and `allowed_actor_ids` as applicable. The sample `config.json` contains placeholder IDs. A missing, extra, or nonpositive pin stops startup; Minion does not infer trust from a login that another account may have claimed. Pending retries without IDs cannot run; they are discarded when their notification is seen or their retry becomes due.

The optional `codex` section selects `model`, `reasoning_effort`, and `skills` for every job. `skills` contains paths to `SKILL.md` files (relative to `config.local.json` or absolute). The bot checks each file at startup, grants Codex read access to its directory, and instructs Codex to read it before working. Omit a setting to use Codex's default; an empty `skills` list applies no configured skills. To enable `audit`, add a `security-audit/SKILL.md` entry and include `audit` in the repository's actions. Startup rejects an `audit` action without that skill. Codex jobs ignore the user's `config.toml` and cannot run network commands from their workspace.

Every startup, including `-check`, queries the installed Codex CLI's [model catalogue](https://learn.chatgpt.com/docs/app-server#models) and validates the model and its supported reasoning efforts. Omitted values are resolved from that catalogue and pinned for the life of the service. Skill files must exist, be readable, and contain instructions. Invalid settings, an incompatible Codex protocol, or a failed catalogue lookup stop startup before notification polling or state changes; the log includes the rejected value and supported choices when available. Model discovery has a 30-second timeout and does not start an inference turn. The systemd service retries failed startups every 30 seconds, so a removed model or effort requires correcting `config.local.json` or the Codex installation before work resumes. Restart minion after upgrading Codex to revalidate. The startup log records the selected model, effort, and skill count. Catalogue validation checks compatibility, not future API availability or account quota.

Run it as a service:

```sh
./bin/minion -config ./config.local.json -loop
```

For a user service, copy [`deploy/minion.service`](deploy/minion.service) to `~/.config/systemd/user/minion.service`. Set its absolute checkout path in `WorkingDirectory` and `ExecStart`, and add the installed `gh` and `codex` directories to `PATH` if needed. Then run `systemctl --user daemon-reload` and `systemctl --user enable --now minion.service`. Enable user lingering if it must continue after logout. After rebuilding the binary, restart it:

```sh
systemctl --user restart minion.service
systemctl --user status minion.service
journalctl --user -u minion.service -n 50 --no-pager
```

Or run one pass each minute from cron (use absolute paths, and ensure that cron has access to the same `gh` and `codex` authentication):

```cron
* * * * * /path/to/minion/bin/minion -config /path/to/minion/config.local.json >> /path/to/minion/var/minion.log 2>&1
```

State and job checkouts live under `var/`. Keep `work_root` outside `/tmp`, which the command sandbox hides; Codex receives its own temporary directory beside the checkout. **Before starting Minion, mount a dedicated filesystem at `work_root` whose total capacity is at most `work_root_max_bytes` (8 GiB by default).** Its mount point must be on a different filesystem from the directory containing `state_path`. Startup, `-check`, and each job refuse a missing, unmounted, shared, oversized, or nearly full work root (less than 64 MiB free). This keeps checkout, runtime, and audit writes from filling the state filesystem. A full work filesystem blocks new executions until space is freed; retained audit reports under `work_root/audits` need operator cleanup.

For example, an operator can mount a 2 GiB tmpfs at `/path/to/minion/var/work` with a finite inode count. Create the underlying mount point as root-owned mode `0555`, then mount it with ownership and mode `0700` for the Minion user. Configure the mount to persist across reboots, and verify it with `findmnt /path/to/minion/var/work` and `df -h /path/to/minion/var/work` before starting the service. A tmpfs loses audit reports on reboot; use a dedicated non-thin block filesystem if they must persist. Do not use a sparse loop image backed by the state filesystem: its backing file could still fill that filesystem. The read-only underlying mount point prevents writes from falling through if the mount disappears. If you set a larger `work_root_max_bytes`, keep the actual filesystem capacity within that value.

A file lock prevents overlapping cron runs. The process persists GitHub's minimum `X-Poll-Interval`, so an early cron pass exits without polling. Both the GitHub client and the Codex CLI need outbound network access. Codex's **model-generated shell commands** run in a restricted workspace without network. The Go service owns authenticated GitHub writes: it reacts to accepted commands before Codex runs and publishes allowed results afterward. Invitation handling runs independently.

Every 15 minutes, the service checks [pending repository invitations](https://docs.github.com/en/rest/collaborators/invitations?apiVersion=2022-11-28). It accepts nonexpired invitations addressed to the configured bot when the repository owner matches a pinned `invitation_owners` ID or the repository matches a monitored name and pinned repository and owner IDs. The owner in GitHub's repository record must match its full name. Invitations to monitored repositories still need `write`, `maintain`, or `admin` access; invitations to other allowed-owner repositories may grant any standard role. Read and triage invitations are accepted without enabling commands. Invitations from unlisted owners are declined unless their numeric owner or repository identity is pinned. Malformed, expired, wrong-invitee, and pinned identity mismatch invitations stay untouched. Each check fetches at most ten pages and gives listing and invitation API actions separate 30-second budgets, so a large invitation backlog does not hold notification polling indefinitely. Fetched pages are handled after a later page fails unless GitHub returns HTTP 403 or 429; those responses defer further invitation actions until the next check. New writable repositories are saved atomically to the config before acceptance, so a failed config write leaves the invitation pending for retry. An acceptance failure can leave the entry in the config while acceptance retries. The config file and its directory must be writable; stop the service before editing config to avoid concurrent updates. Invitations accepted before automatic registration was installed need a manual repository entry. This automates the invited account's acceptance step; a repository owner must still send the invitation.

## Live tests

Create the ignored `live-tests.local.json` in the repository root to enable live tests. Without it, they skip:

```json
{
  "owner_login": "example-owner",
  "bot_login": "example-bot",
  "repository_url": "https://github.com/example-owner/example-repo"
}
```

Replace the sample values with two accounts authenticated in `gh` and a repository they can read. Optional `issue_number` selects an owner-authored issue with an owner comment; `pull_request_number` selects an open owner-authored draft pull request with an owner comment; `release_tag` selects an existing tag and draft release. The `linkdata/miniontesting` pull request fixture also needs a bot-authored inline review comment and submitted review with a non-empty body. Optional `bot_ssh_key` is an absolute path to the bot's SSH private key or a path relative to the repository root; it enables SSH identity and repository read checks. Put no token or key contents in this file.

Run `go test -race ./internal/github ./internal/gitwork -run '^(TestLiveSmoke|TestLiveClientReadOnly|TestLiveBotGitAccess|TestLiveCloneAndPullRequest)$' -count=1`. Keep `-count=1` so Go does not reuse a cached result. These tests read GitHub through REST, read-only GraphQL queries, SSH authentication, and Git fetches without changing GitHub state. They verify both accounts' `gh` credentials and, when configured, the bot's SSH key. The Git checkout test uses only `linkdata/miniontesting` and skips for other repositories.

Stop any running minion service before a write run so it cannot accept the test invitation or act on test events. The write run uses both accounts' `gh` credentials and the dedicated `linkdata/miniontesting` repository:

```sh
MINION_LIVE_WRITE=1 go test -race ./internal/github -run '^TestLiveBotWriteLifecycle$' -count=1
```

The write test reuses `linkdataminion`'s write access, accepts a suitable pending invitation, or invites it when neither exists. It keeps the resulting access for later runs. The test posts and edits a bot issue comment, reacts to a temporary owner comment, creates a temporary tag and draft release, and, when `bot_ssh_key` is set, pushes a temporary branch and opens a draft pull request. Set `MINION_LIVE_REVIEW=1` as well to post a comment review on the configured fixture pull request. Cleanup deletes both comments and the reaction, draft release, tag, and temporary branch and closes the temporary pull request. GitHub retains submitted reviews and closed pull request records. Cancel an expired bot invitation in Collaborators settings before another write run.

After bot write access is established, create `linkdataminion/miniontesting` as a fork of `linkdata/miniontesting` and disable GitHub Actions on the fork. Exercise the production Git publishing path with `MINION_LIVE_WRITE=1 go test -race ./internal/gitwork -run '^TestLiveCommitAndPush$' -count=1`. It creates one branch on the bot fork, verifies its commit and bot identity, and deletes the branch after checking its exact SHA.

Exercise the production actions with stub Codex results:

```sh
MINION_LIVE_WRITE=1 go test -race ./internal/action -run '^(TestLiveFixPublishesBranch|TestLiveAuditAction|TestLiveReleaseRefusesUnprotectedTags)$' -count=1
```

The fix test publishes a temporary branch to `linkdataminion/miniontesting` and a reply on the base issue, checks a repeat execution, then removes both after identity checks. The audit test checks both issue and draft pull request heads, validates its report, checks a repeat execution, and removes its replies. The release test verifies that the unprotected test repository refuses to create a tag or draft and acknowledges the request once. A successful production release remains covered offline because the required all-tag protection would prevent this live suite from removing its temporary tag. Replies can leave GitHub notifications after cleanup.

To exercise the production review action, run `MINION_LIVE_WRITE=1 MINION_LIVE_REVIEW=1 go test -race ./internal/action -run '^TestLiveReviewAction$' -count=1`. It reviews the configured owner-authored draft pull request at its exact head, submits a comment review, and checks that a repeat execution does not submit another. GitHub retains the submitted review.

If a write test is interrupted before cleanup, inspect `linkdata/miniontesting` for temporary `minion/live-*` branches, `minion-live-*` tags and drafts, and bot replies carrying its job marker. Inspect `linkdataminion/miniontesting` for temporary `minion/live-gitwork-*` and `minion/fix-*` branches. Remove only artifacts created by that run. Notification delivery and the monitor's invitation handling depend on GitHub account settings and are covered by offline integration tests; the live suite checks the repository notifications API.

The invitation and revocation lifecycle has a separate opt-in test. Run it only when explicitly requested, with `MINION_LIVE_WRITE` unset and `linkdataminion` lacking collaborator access or a pending invitation; it removes the access it grants:

```sh
MINION_LIVE_COLLABORATION=1 go test -race ./internal/github -run '^TestLiveBotCollaborationLifecycle$' -count=1
```

## Usage reports

The service records token usage for each action attempt, including failures and retries. Reports are local and need no GitHub or Codex authentication or network access. They can run while the service is running:

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

Input, cached input, and output tokens come from Codex's usage reports, including helper agents. All actions retain normal Codex session logs to recover this accounting; the usage records contain metadata and counts, not prompts or replies. **Cached input is a subset of input**; total tokens are input plus output, without adding cached input twice. Reports include success, failure, running, and incomplete-usage counts. Missing or partial usage stays marked incomplete: displayed counts are lower bounds, not a claim that an interrupted call was free. A `running` attempt may be left by an interrupted service. Retries are separate attempts and each counts toward usage. Token accounting begins when this feature is installed; it does not reconstruct historical usage. Reports show tokens, not dollar estimates or subscription charges.

Records are stored in a `usage/` directory beside `state_path` (`var/usage/` by default), independently of notification state. Keep this directory to retain usage history. `-config` loads and validates the local configuration but does not run service preflight; `-dir` bypasses configuration entirely and cannot be combined with `-config`. A missing usage directory produces an empty report.

## Why polling

GitHub does not offer a webhook for a user account's personal mentions. The [notifications API](https://docs.github.com/en/rest/activity/notifications?apiVersion=2022-11-28) is the simplest route for a dedicated machine user: a paginated poll for each whitelisted repository, no public server, and no repository webhook administration. This service requests read and unread notifications since a durable cursor for each repository, obeys `X-Poll-Interval`, then inspects content in whitelisted threads. Notification `reason` is thread-level, so the service validates the actual command in each new body/comment and deduplicates by source item plus command, not by notification ID.

Initial issue and pull request descriptions and their comments, pull request reviews and line comments are scanned. Edits to an old issue or pull request description are not treated as new commands, because REST `updated_at` also changes for unrelated thread activity. GitHub notification delivery settings still control what reaches this account. Discussions are unsupported because [their notification subjects can have no URL](https://github.com/orgs/community/discussions/62728), leaving no reliable thread ID. If you later need guaranteed coverage of every edit, discussion support, or lower latency, install repository or [GitHub App webhooks](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks) for the relevant events. A GitHub App posts as its own app bot identity, rather than the configured bot account.

The service stores no access token on disk. `var/state.json` tracks repository cursors, completed command IDs, failed job attempts, pending failed threads, and verified fix branch SHAs for crash recovery. The old account cursor seeds repository cursors on upgrade and when a repository is newly whitelisted. If a repository notification poll fails, only its cursor stays in place; the other repositories and pending retries continue. A repository with more than 2,000 notifications in one poll fails closed at 20 pages, preserving its cursor for intervention. A failed thread is retried from its original scan time with backoff up to 64 minutes while the cursor continues past other notifications. A permanently inaccessible thread remains in the pending list and is logged with its URL; an operator can remove that entry after checking it for valid commands. An older binary cannot read state after repository cursors or failures are recorded; back up state before downgrading. Generated code changes are pushed to branches for requesters to turn into pull requests, and release notes are created only as draft releases.
