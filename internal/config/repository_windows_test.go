//go:build windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/wintest"
	"golang.org/x/sys/windows"
)

func TestAddRepositoryThroughJunctionWindows(t *testing.T) {
	physical := t.TempDir()
	config := filepath.Join(physical, "config.json")
	if err := os.WriteFile(config, []byte(`{"bot_login":"bot","repositories":[{"name":"owner/first","id":1,"owner_id":2,"actions":["review"]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "config-home")
	if err := wintest.CreateJunction(alias, physical); err != nil {
		t.Fatal(err)
	}
	if _, err := AddRepository(filepath.Join(alias, "config.json"), Repository{Name: "owner/second", ID: 3, OwnerID: 2, Actions: []string{"review"}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(config)
	if err != nil || !strings.Contains(string(data), `"name": "owner/second"`) {
		t.Fatalf("config target was not updated: %s, %v", data, err)
	}
}

func TestUpdatePreservesWindowsConfigSecurity(t *testing.T) {
	dir := t.TempDir()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	userSID := user.User.Sid.String()
	parentSD, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + userSID + ")(A;OICI;FR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	parentDACL, _, err := parentSD.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, parentDACL, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + userSID + ")")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	original, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	originalOwner, _, err := original.Owner()
	if err != nil || originalOwner == nil {
		t.Fatalf("read original config owner: %v", err)
	}
	groups, err := windows.GetCurrentProcessToken().GetTokenGroups()
	if err != nil {
		t.Fatal(err)
	}
	alternateOwner := false
	for _, group := range groups.AllGroups() {
		if group.Attributes&windows.SE_GROUP_OWNER == 0 || group.Attributes&windows.SE_GROUP_USE_FOR_DENY_ONLY != 0 ||
			group.Sid.Equals(originalOwner) {
			continue
		}
		if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION, group.Sid, nil, nil, nil); err == nil {
			alternateOwner = true
			break
		}
	}
	if !alternateOwner {
		t.Log("no assignable alternate owner in token; checking current owner")
	}
	before, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	beforeOwner, _, err := before.Owner()
	if err != nil || beforeOwner == nil {
		t.Fatalf("read config owner: %v", err)
	}
	control, _, err := before.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("config DACL was not protected: control=%#x error=%v", control, err)
	}
	temporary, err := newConfigTemp(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = temporary.Close()
		if err := cleanupConfigTemp(temporary.Name()); err != nil {
			t.Errorf("remove temporary config: %v", err)
		}
	})
	tempACL, err := windows.GetNamedSecurityInfo(temporary.Name(), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || tempACL == nil {
		t.Fatalf("read temporary config DACL: %v", err)
	}
	tempOwner, _, err := tempACL.Owner()
	if err != nil || tempOwner == nil || !beforeOwner.Equals(tempOwner) {
		t.Fatalf("temporary config owner differs: original=%s temporary=%v error=%v", beforeOwner, tempOwner, err)
	}
	tempControl, _, err := tempACL.Control()
	beforeSDDL, tempSDDL := before.String(), tempACL.String()
	_, beforeACEs, beforeHasACE := strings.Cut(beforeSDDL, "(")
	_, tempACEs, tempHasACE := strings.Cut(tempSDDL, "(")
	if err != nil || tempControl&windows.SE_DACL_PROTECTED == 0 || !beforeHasACE || !tempHasACE || beforeACEs != tempACEs {
		t.Fatalf("temporary config DACL differs from original before write: original=%q temporary=%q control=%#x error=%v", beforeSDDL, tempSDDL, tempControl, err)
	}
	if _, err := Update(path, func(cfg *Config) error { cfg.BotLogin = "changed"; return nil }); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(filepath.Join(dir, ".config-*.backup"))
	if err != nil || len(backups) != 0 {
		t.Fatalf("successful config update left backup files: %v, %v", backups, err)
	}
	after, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	afterOwner, _, err := after.Owner()
	if err != nil || afterOwner == nil || !beforeOwner.Equals(afterOwner) {
		t.Fatalf("saved config owner differs: original=%s saved=%v error=%v", beforeOwner, afterOwner, err)
	}
	if before.String() == "" || before.String() != after.String() {
		t.Fatal("config DACL changed after save")
	}
}

func TestUpdatePreservesInheritedWindowsConfigACL(t *testing.T) {
	dir := t.TempDir()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	parentSD, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;FR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	parentDACL, _, err := parentSD.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, parentDACL, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := before.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED != 0 || !strings.Contains(before.String(), "ID;") {
		t.Fatalf("config did not inherit parent ACL: control=%#x descriptor=%q error=%v", control, before.String(), err)
	}
	if _, err := Update(path, func(cfg *Config) error { cfg.BotLogin = "changed"; return nil }); err != nil {
		t.Fatal(err)
	}
	after, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if before.String() != after.String() {
		t.Fatalf("saved config changed inherited ACL: before=%q after=%q", before.String(), after.String())
	}
}
