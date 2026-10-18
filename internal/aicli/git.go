package aicli

// ModelGitEnvironment disables global/system Git configuration and optional metadata writes.
func ModelGitEnvironment() []string {
	return []string{
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1",
		"GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0",
	}
}
