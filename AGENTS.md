# Project instructions

- Use strict critic acceptance when developing or reviewing Minion itself: implement or refute every actionable finding, including nits and simplifications, with concrete evidence or an applicable user constraint before approval or merge. Nonblocking findings do not waive this requirement. Set `critic_stopping.nonblocking_after_round` to `0` in any deployment used for Minion work; this service-wide setting also applies to its other watched repositories. Other deployments may keep the configurable default.
- Run the opt-in invite-and-revoke live test (`MINION_LIVE_COLLABORATION=1`, `TestLiveBotCollaborationLifecycle`) only when the user explicitly asks for that test. Do not include it in routine test runs or broad live-test verification.
