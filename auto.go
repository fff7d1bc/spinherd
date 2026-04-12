//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type PlanReport struct {
	Mode        string
	Herds       []Herd
	Excluded    []ExcludedMount
	RootDevices []string
	SwapDevices []string
	SwapEntries []string
}

type ExcludedMount struct {
	Mountpoint string
	Source     string
	Devices    []string
	Reasons    []string
}

func planHerds(paths Paths, mountpoints, ignoredMountpoints []string, auto bool) ([]Herd, PlanReport, error) {
	if auto {
		report, err := planAuto(paths, ignoredMountpoints)
		if err != nil {
			return nil, PlanReport{}, err
		}
		return report.Herds, report, nil
	}

	runtimes, err := resolveRuntimes(paths, mountpoints)
	if err != nil {
		return nil, PlanReport{}, err
	}
	herds := groupHerds(runtimes)
	return herds, PlanReport{Mode: "manual", Herds: herds}, nil
}

func planAuto(paths Paths, ignoredMountpoints []string) (PlanReport, error) {
	entries, err := readMountInfo(paths.MountInfo)
	if err != nil {
		return PlanReport{}, err
	}
	entries = effectiveMounts(entries)
	ignoredSet, err := normalizeMountpointSet(ignoredMountpoints)
	if err != nil {
		return PlanReport{}, err
	}

	rootRuntime, err := resolveRuntime(paths, "/")
	if err != nil {
		return PlanReport{}, fmt.Errorf("resolve root filesystem: %w", err)
	}
	rootSet := makeStringSet(rootRuntime.Devices)

	swapDevices, swapEntries, err := resolveSwapDevices(paths, entries)
	if err != nil {
		return PlanReport{}, err
	}
	swapSet := makeStringSet(swapDevices)

	var runtimes []Runtime
	var excluded []ExcludedMount

	for _, entry := range entries {
		if _, ignored := ignoredSet[entry.Mountpoint]; ignored {
			excluded = append(excluded, ExcludedMount{
				Mountpoint: entry.Mountpoint,
				Source:     entry.Source,
				Reasons:    []string{"ignored by user"},
			})
			continue
		}

		if !strings.HasPrefix(entry.Source, "/dev/") {
			excluded = append(excluded, ExcludedMount{
				Mountpoint: entry.Mountpoint,
				Source:     entry.Source,
				Reasons:    []string{"source is not block-backed"},
			})
			continue
		}

		runtime, err := runtimeFromMount(paths, entry)
		if err != nil {
			excluded = append(excluded, ExcludedMount{
				Mountpoint: entry.Mountpoint,
				Source:     entry.Source,
				Reasons:    []string{fmt.Sprintf("resolution failed: %v", err)},
			})
			continue
		}

		var reasons []string
		if intersects(runtime.Devices, rootSet) {
			reasons = append(reasons, "shares devices with root filesystem")
		}
		if intersects(runtime.Devices, swapSet) {
			reasons = append(reasons, "shares devices with swap")
		}

		rot, nonRot, err := classifyRotational(paths, runtime.Devices)
		if err != nil {
			return PlanReport{}, err
		}
		switch {
		case len(rot) == 0:
			reasons = append(reasons, "no rotational disks")
		case len(nonRot) > 0:
			reasons = append(reasons, "mixed rotational and non-rotational disks")
		}

		if len(reasons) > 0 {
			excluded = append(excluded, ExcludedMount{
				Mountpoint: runtime.Mount.Mountpoint,
				Source:     runtime.Mount.Source,
				Devices:    runtime.Devices,
				Reasons:    reasons,
			})
			continue
		}

		runtimes = append(runtimes, runtime)
	}

	herds := groupHerds(runtimes)
	sort.Slice(excluded, func(i, j int) bool {
		return excluded[i].Mountpoint < excluded[j].Mountpoint
	})

	return PlanReport{
		Mode:        "auto",
		Herds:       herds,
		Excluded:    excluded,
		RootDevices: append([]string(nil), rootRuntime.Devices...),
		SwapDevices: swapDevices,
		SwapEntries: swapEntries,
	}, nil
}

func resolveSwapDevices(paths Paths, entries []MountInfo) ([]string, []string, error) {
	file, err := os.Open(paths.Swaps)
	if err != nil {
		return nil, nil, fmt.Errorf("open swaps: %w", err)
	}
	defer file.Close()

	var devices []string
	var entriesOut []string
	seen := make(map[string]struct{})

	scanner := bufio.NewScanner(file)
	first := true
	for scanner.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}

		name := fields[0]
		entryDevices, err := resolveSwapEntryDevices(paths, entries, name)
		if err != nil {
			return nil, nil, err
		}
		if len(entryDevices) == 0 {
			continue
		}
		entriesOut = append(entriesOut, name)
		for _, device := range entryDevices {
			if _, found := seen[device]; found {
				continue
			}
			seen[device] = struct{}{}
			devices = append(devices, device)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("read swaps: %w", err)
	}

	sort.Strings(devices)
	sort.Strings(entriesOut)
	return devices, entriesOut, nil
}

func resolveSwapEntryDevices(paths Paths, mounts []MountInfo, name string) ([]string, error) {
	if strings.HasPrefix(name, "/dev/") {
		return resolvePhysicalDevices(paths, "", name)
	}

	mount, ok := findContainingMount(mounts, name)
	if !ok {
		return nil, fmt.Errorf("swap file %s is not on a known mount", name)
	}
	runtime, err := runtimeFromMount(paths, mount)
	if err != nil {
		return nil, fmt.Errorf("resolve swap file backing mount %s: %w", name, err)
	}
	return runtime.Devices, nil
}

func findContainingMount(mounts []MountInfo, path string) (MountInfo, bool) {
	cleaned := filepath.Clean(path)
	var best MountInfo
	bestLen := -1

	for i := len(mounts) - 1; i >= 0; i-- {
		mount := mounts[i]
		mountpoint := mount.Mountpoint
		if cleaned != mountpoint && !strings.HasPrefix(cleaned, mountpoint+"/") {
			continue
		}
		if len(mountpoint) > bestLen {
			best = mount
			bestLen = len(mountpoint)
		}
	}

	return best, bestLen >= 0
}

func classifyRotational(paths Paths, devices []string) ([]string, []string, error) {
	var rotational []string
	var nonRotational []string
	for _, device := range devices {
		value, err := os.ReadFile(filepath.Join(paths.SysClassBlk, device, "queue", "rotational"))
		if err != nil {
			return nil, nil, fmt.Errorf("read rotational flag for %s: %w", device, err)
		}
		switch strings.TrimSpace(string(value)) {
		case "1":
			rotational = append(rotational, device)
		case "0":
			nonRotational = append(nonRotational, device)
		default:
			return nil, nil, fmt.Errorf("unexpected rotational flag for %s: %q", device, strings.TrimSpace(string(value)))
		}
	}
	return rotational, nonRotational, nil
}

func makeStringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func intersects(values []string, set map[string]struct{}) bool {
	for _, value := range values {
		if _, found := set[value]; found {
			return true
		}
	}
	return false
}

func normalizeMountpointSet(values []string) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		cleaned, err := filepath.Abs(value)
		if err != nil {
			return nil, fmt.Errorf("resolve mountpoint %s: %w", value, err)
		}
		set[cleaned] = struct{}{}
	}
	return set, nil
}
