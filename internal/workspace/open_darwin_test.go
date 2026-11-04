//go:build darwin

package workspace

func bumpTestChangeTime(info *fileInfoWithStat) {
	info.stat.Ctimespec.Sec++
}
