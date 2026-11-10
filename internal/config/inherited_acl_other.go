//go:build !darwin

package config

import "os"

func RemoveInheritedConfigACL(*os.File) error { return nil }
