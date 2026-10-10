# linkdataminion

[![build](https://github.com/linkdata/minion/actions/workflows/build.yml/badge.svg)](https://github.com/linkdata/minion/actions/workflows/build.yml)
[![coverage](/../gitcoverage/main/badge.svg)](https://github.com/linkdata/minion/tree/gitcoverage/main)

A small Go service that watches a bot account's GitHub notifications for commands in an explicit repository whitelist. It invokes the authenticated Codex CLI for the work and uses the GitHub API for publication. The checked-in [config.json](config.json) contains sample settings; copy it to the ignored `config.local.json` for a real deployment.

## What it does

Put one command on the **first nonblank line** of an issue or pull request body or comment. These examples use `@linkdataminion`; substitute the configured `bot_login`:

| Command | Result |
| --- | --- |
| `@linkdataminion review` | On a pull request, Codex reviews its exact head against its base. The bot submits a comment review; it never approves. |
| `@linkdataminion audit` | In an issue, audits the default branch; in a PR, audits its exact head. Invokes the configured security-audit skill on the full source tree and posts a Markdown summary in the requesting thread. |
| `@linkdataminion fix` | On an issue, Codex edits a fresh checkout of the default branch. The bot pushes a deterministic branch and gives the requester a compare link to open the pull request themselves. Optional text may follow `fix` on the same line. |
| `@linkdataminion release v1.2.3` | Codex drafts release notes from local Git history. The bot creates a **draft GitHub release** for the new tag, pinned to the checked-out commit. It never publishes the release. |

Release drafts require an active GitHub tag ruleset covering **all tags** (`~ALL` with no exclusions), restricting both updates and deletions, and granting no bypass. Minion checks all active tag rulesets through GitHub's API before reserving the tag and creating the draft; any active tag creation restriction causes refusal, even if it targets a different tag pattern, so the bot cannot bypass a restriction on the requester. The bot token must be able to see the integrity ruleset's bypass list; GitHub hides it from tokens without ruleset write access. An unavailable API, hidden bypass list, or insufficient repository plan causes release requests to be refused. Send a new command after protection is available. Minion reserves the tag at the inspected commit before creating the draft. An administrator can change the ruleset later; verify the tag before publishing a draft.

Fix replies record the expected branch head. The requester must open the pull request from their own account, so GitHub can apply any independent-review rule to that requester. The branch remains mutable afterward; protect `minion/fix-*` from updates by other writers and compare its current head with the recorded commit before merging. This private repository's current plan cannot enforce those protections or required reviews, so a second-person review remains a manual requirement here. For a bot-authored fix PR from an older version, close that PR, verify the recorded branch head, and have the requester open a new PR from the branch.

Each request must come from an account with effective repository `write`, `maintain`, or `admin` permission. An optional `allowed_actors` list narrows that group. A command in a quotation, code fence, HTML comment, or later body line is ignored. A second full-line directive makes the request ambiguous and is refused. The bot checks the whitelist and author permission again before every action.

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
./bin/minion -config ./config.local.json -check
```

The `gh` token needs access to [GitHub notifications](https://docs.github.com/en/rest/activity/notifications?apiVersion=2022-11-28) and the whitelisted repositories. GitHub's notifications endpoint requires a classic personal access token or OAuth token; fine-grained tokens and GitHub App tokens do not work there. The bot needs repository **write** access so it can query [collaborator permission](https://docs.github.com/en/rest/collaborators/collaborators?apiVersion=2022-11-28), push fix branches, and create draft releases. Release drafts also require ruleset write access so GitHub includes the bypass list in its API response. `-check` verifies notification and repository access. Credentials are read from `GH_TOKEN`/`GITHUB_TOKEN` or `gh auth token`; Codex uses its existing login or `CODEX_API_KEY`. Do not put tokens in either configuration file.

Edit `config.local.json` to change the repositories monitored for commands or their enabled actions. Each `repositories` entry is an `owner/repo` name. `allowed_actors` can restrict commands to named committers in addition to the GitHub permission check. `invitation_owners` lists accounts whose current and future repositories may invite the bot. Invitations with `write`, `maintain`, or `admin` access automatically add the repository to `config.local.json` with `review`, `fix`, and `release` enabled, plus `audit` when security-audit is configured; fixes and releases still produce drafts. Monitoring starts in the same poll without a restart. Existing repository actions and actor restrictions are preserved. `start_at` may be set to an RFC3339 timestamp *before the first run* to process older notifications. Without it, the first run starts at the current time.

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

State and job checkouts live under `var/`. Keep `work_root` outside `/tmp`, which the command sandbox hides; Codex receives its own temporary directory beside the checkout. A file lock prevents overlapping cron runs. The process persists GitHub's minimum `X-Poll-Interval`, so an early cron pass exits without polling. Both the GitHub client and the Codex CLI need outbound network access. Codex's **model-generated shell commands** run in a restricted workspace without network; the Go service performs authenticated GitHub writes after Codex finishes.

Every 15 minutes, the service checks [pending repository invitations](https://docs.github.com/en/rest/collaborators/invitations?apiVersion=2022-11-28). It accepts nonexpired invitations addressed to the configured bot when the repository owner is listed in `invitation_owners` or the repository is explicitly monitored. The owner in GitHub's repository record must match its full name. Invitations to monitored repositories still need `write`, `maintain`, or `admin` access; invitations to other allowed-owner repositories may grant any standard role. Read and triage invitations are accepted without enabling commands. Other invitations stay untouched. New writable repositories are saved atomically to the config before acceptance, so a failed config write leaves the invitation pending for retry. An acceptance failure can leave the entry in the config while acceptance retries. The config file and its directory must be writable; stop the service before editing config to avoid concurrent updates. Invitations accepted before automatic registration was installed need a manual repository entry. This automates the invited account's acceptance step; a repository owner must still send the invitation.

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

GitHub does not offer a webhook for a user account's personal mentions. The [notifications API](https://docs.github.com/en/rest/activity/notifications?apiVersion=2022-11-28) is the simplest route for a dedicated machine user: one paginated poll for recently updated notifications, no public server, and no repository webhook administration. This service requests read and unread notifications since a durable cursor, obeys `X-Poll-Interval`, then inspects content in whitelisted threads. Notification `reason` is thread-level, so the service validates the actual command in each new body/comment and deduplicates by source item plus command, not by notification ID.

Initial issue and pull request descriptions and their comments, pull request reviews and line comments are scanned. Edits to an old issue or pull request description are not treated as new commands, because REST `updated_at` also changes for unrelated thread activity. GitHub notification delivery settings still control what reaches this account. Discussions are unsupported because [their notification subjects can have no URL](https://github.com/orgs/community/discussions/62728), leaving no reliable thread ID. If you later need guaranteed coverage of every edit, discussion support, or lower latency, install repository or [GitHub App webhooks](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks) for the relevant events. A GitHub App posts as its own app bot identity, rather than the configured bot account.

The service stores no access token on disk. `var/state.json` tracks its cursor, completed command IDs, failed job attempts, and verified fix branch SHAs for crash recovery. An older binary cannot read state after failed job attempts are recorded; back up state before downgrading. Generated code changes are pushed to branches for requesters to turn into pull requests, and release notes are created only as draft releases.
