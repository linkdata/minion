package main

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestAdminConfigurationPagesShowPoliciesAndEscapeData(t *testing.T) {
	cfg := config.Config{
		Main: "codex", Codex: config.CodexConfig{APIKey: "secret-model-key"},
		Admin: &config.AdminConfig{Administrators: []config.AdminAccount{{Username: "operator", PasswordHash: "secret-password-hash"}}},
		Repositories: []config.Repository{
			{Name: "owner/open", ID: 1, OwnerID: 10, Actions: []string{"review"}},
			{Name: "owner/closed", ID: 2, OwnerID: 10, DenyAllActors: true, Actions: []string{"review"}},
			{Name: "owner/limited", ID: 3, OwnerID: 10, AllowedActors: []string{"alice"}, AllowedActorIDs: map[string]int64{"alice": 42}, Actions: []string{"review"}},
			{Name: "owner/legacy-open", ID: 4, OwnerID: 10, AllowedActors: []string{}, Actions: []string{"review"}},
		},
		Commands:       map[string]config.CommandMapping{"review": {Action: "review", Skills: []string{"https://github.com/owner/project/tree/main/skills/review"}}},
		AlwaysOnSkills: []string{"builtin://anti-prompt-injection"},
	}
	view := configurationView(adminConfigSnapshot{Config: cfg, Error: "<script>alert(1)</script>", Pending: true})
	if len(view.Users) != 4 || !view.Users[0].AnyWriter || view.Users[1].AnyWriter || len(view.Users[1].Users) != 0 || view.Users[2].Users[0].ID != 42 || !view.Users[3].AnyWriter {
		t.Fatalf("requester policies = %+v", view.Users)
	}
	if view.Repositories[0].CanRemove != true || view.CommandSkills[0].Skills[0].CanRemove || !view.AlwaysOnSkills[0].CanRemove {
		t.Fatalf("removal controls = %+v", view)
	}
	page := struct {
		Dot                adminConfigurationView
		HeadHTML, TailHTML template.HTML
	}{Dot: view}
	for _, tc := range []struct {
		name  string
		wants []string
	}{
		{"admin users", []string{"Any writer", "Nobody", "@alice", `name="repository" value="owner/limited"`, "/admin/users/add", "/admin/users/remove", "/admin/users/allow-any", "/admin/users/deny-all", "Allow nobody"}},
		{"admin repositories", []string{"owner/open", "Repository ID 1", `value="review" checked`, "/admin/repositories/add", "/admin/repositories/remove", "/admin/repositories/actions", "The new repository starts with no allowed requesters"}},
		{"admin skills", []string{"anti-prompt-injection", "review command", `value="command:review"`, "/admin/skills/add", "/admin/skills/remove"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			if err := adminConfigurationHTML.ExecuteTemplate(&body, tc.name, page); err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.wants {
				if !strings.Contains(body.String(), want) {
					t.Fatalf("%s missing %q: %s", tc.name, want, body.String())
				}
			}
			if tc.name == "admin users" && strings.Count(body.String(), `action="/admin/users/deny-all"`) != 2 {
				t.Fatalf("deny-all button must appear only for the two any-writer repositories: %s", body.String())
			}
			for _, secret := range []string{"secret-model-key", "secret-password-hash", "<script>alert(1)</script>"} {
				if strings.Contains(body.String(), secret) {
					t.Fatalf("%s exposed %q", tc.name, secret)
				}
			}
			if !strings.Contains(body.String(), "&lt;script&gt;alert(1)&lt;/script&gt;") {
				t.Fatalf("%s did not escape error text", tc.name)
			}
		})
	}
	page.Dot = configurationView(adminConfigSnapshot{Config: cfg, Remote: true})
	var remote bytes.Buffer
	if err := adminConfigurationHTML.ExecuteTemplate(&remote, "admin skills", page); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(remote.String(), "Remote worker restart required") || strings.Contains(remote.String(), "/admin/skills/add") || strings.Contains(remote.String(), "/admin/skills/remove") {
		t.Fatalf("remote worker skills page offered live edits: %s", remote.String())
	}
}
