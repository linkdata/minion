package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"
)

//go:embed bundled
var bundledSkills embed.FS

// Commits identify the upstream base; embedded copies include Minion-specific edits.
// The copies are reviewed and updated with Minion, not at install time.
var bundledCommits = map[string]string{
	"https://github.com/DietrichGebert/ponytail/tree/main/skills/ponytail":               "96608e078b2d5ea607c5c3b8b5a54d1c1fba6846",
	"https://github.com/DietrichGebert/ponytail/tree/main/skills/ponytail-review":        "96608e078b2d5ea607c5c3b8b5a54d1c1fba6846",
	"https://github.com/DietrichGebert/ponytail/tree/main/skills/ponytail-audit":         "96608e078b2d5ea607c5c3b8b5a54d1c1fba6846",
	"https://github.com/JuliusBrussee/caveman/tree/main/skills/caveman":                  "1e91f1306d602d4ca23618701b1185575ae1869b",
	"https://github.com/cloudflare/security-audit-skill/tree/main/skills/security-audit": "c1c8a8c1471069fb0e188eeaff69b8e8db6564a8",
}

func bundledFiles(src source) (files []skillFile, commit string, err error) {
	commit = bundledCommits[src.url]
	if commit == "" {
		return nil, "", nil
	}
	root := "bundled/" + src.name
	err = fs.WalkDir(bundledSkills, root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("bundled skill %q contains a non-file entry", src.name)
		}
		data, err := bundledSkills.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, skillFile{name: strings.TrimPrefix(path, root+"/"), data: data, mode: info.Mode()})
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return files, commit, nil
}
