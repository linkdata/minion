package codexcli

// appendChildIDs keeps repeated waits from consuming the unique-helper bound.
func appendChildIDs(children *[]string, seen map[string]bool, ids ...string) bool {
	complete := true
	for _, id := range ids {
		if seen[id] {
			continue
		}
		if len(*children) == maxCodexChildren {
			complete = false
			continue
		}
		seen[id] = true
		*children = append(*children, id)
	}
	return complete
}
