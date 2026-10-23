package action

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/workspace"
)

// contentDigest binds the final wire content; the executor's claim does not prove what a critic saw.
func contentDigest(answer string, changes []workspace.Change, artifacts []workspace.File) (string, error) {
	// Result omits empty slices on the wire; hash the shape the parent receives.
	if len(changes) == 0 {
		changes = nil
	}
	if len(artifacts) == 0 {
		artifacts = nil
	}
	data, err := json.Marshal(struct {
		Answer    string             `json:"answer"`
		Changes   []workspace.Change `json:"changes"`
		Artifacts []workspace.File   `json:"artifacts"`
	}{answer, changes, artifacts})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func validContentDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// reviewedContentDigest derives the eventual wire content from the last frozen critic input.
func reviewedContentDigest(job Job, proposed string, snapshot *reviewSnapshot, auditSkill string, secrets ...string) (string, error) {
	answer := proposed
	var changes []workspace.Change
	var artifacts []workspace.File
	switch job.Action {
	case command.Fix:
		changes = snapshot.changes
		if len(changes) != 0 && !publishableAnswer(answer) {
			answer = ""
		}
	case command.SecurityAudit:
		var err error
		artifacts, err = normalizeAuditArtifacts(snapshot.artifacts, job, auditSkill, secrets...)
		if err != nil {
			return "", err
		}
		for _, artifact := range artifacts {
			if artifact.Path == "REPORT.md" {
				answer = string(artifact.Data)
				break
			}
		}
	}
	return contentDigest(answer, changes, artifacts)
}
