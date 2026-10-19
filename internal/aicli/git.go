package aicli

// ModelGitEnvironment disables global/system Git config, system attributes,
// implicit bare repositories, replacement objects, lazy fetching and optional metadata writes.
// Repository-local config, hooks, excludes, and attributes still apply.
// Callers must first remove inherited GIT_* entries.
func ModelGitEnvironment() []string {
	return []string{
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.bareRepository", "GIT_CONFIG_VALUE_0=explicit",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0",
	}
}
