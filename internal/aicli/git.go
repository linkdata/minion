package aicli

// ModelGitEnvironment disables global/system Git config, system attributes,
// replacement objects, lazy fetching and optional metadata writes.
// Callers must first remove inherited GIT_* entries.
func ModelGitEnvironment() []string {
	return []string{
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1",
		"GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0",
	}
}
