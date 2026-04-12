//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type MountInfo struct {
	Mountpoint string
	Source     string
	MajorMinor string
}

func resolveMount(paths Paths, mountpoint string) (MountInfo, error) {
	cleaned, err := filepath.Abs(mountpoint)
	if err != nil {
		return MountInfo{}, fmt.Errorf("resolve mountpoint: %w", err)
	}

	entries, err := readMountInfo(paths.MountInfo)
	if err != nil {
		return MountInfo{}, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry.Mountpoint == cleaned {
			return entry, nil
		}
	}
	return MountInfo{}, fmt.Errorf("%s is not a mountpoint root", cleaned)
}

func readMountInfo(path string) ([]MountInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open mountinfo: %w", err)
	}
	defer file.Close()

	var entries []MountInfo
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		entry, ok, err := parseMountInfoLine(scanner.Text())
		if err != nil {
			return nil, err
		}
		if ok {
			entries = append(entries, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read mountinfo: %w", err)
	}
	return entries, nil
}

func parseMountInfoLine(line string) (MountInfo, bool, error) {
	parts := strings.Split(line, " - ")
	if len(parts) != 2 {
		return MountInfo{}, false, fmt.Errorf("invalid mountinfo line: %q", line)
	}

	left := strings.Fields(parts[0])
	right := strings.Fields(parts[1])
	if len(left) < 5 || len(right) < 2 {
		return MountInfo{}, false, fmt.Errorf("invalid mountinfo fields: %q", line)
	}

	return MountInfo{
		MajorMinor: left[2],
		Mountpoint: unescapeMountField(left[4]),
		Source:     right[1],
	}, true, nil
}

func unescapeMountField(value string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(value)
}

func parseMajorMinor(value string) (uint64, uint64, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid major:minor value %q", value)
	}
	major, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("parse major %q: %w", value, err)
	}
	minor, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("parse minor %q: %w", value, err)
	}
	return major, minor, nil
}

func effectiveMounts(entries []MountInfo) []MountInfo {
	seen := make(map[string]struct{}, len(entries))
	effective := make([]MountInfo, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if _, found := seen[entry.Mountpoint]; found {
			continue
		}
		seen[entry.Mountpoint] = struct{}{}
		effective = append(effective, entry)
	}
	for i, j := 0, len(effective)-1; i < j; i, j = i+1, j-1 {
		effective[i], effective[j] = effective[j], effective[i]
	}
	return effective
}
