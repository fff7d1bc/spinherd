//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func resolvePhysicalDevices(paths Paths, majorMinor, source string) ([]string, error) {
	name, err := resolveTopBlockDevice(paths, majorMinor, source)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	if err := walkDeviceParents(paths, name, seen); err != nil {
		return nil, err
	}

	devices := make([]string, 0, len(seen))
	for name := range seen {
		devices = append(devices, name)
	}
	sort.Strings(devices)
	return devices, nil
}

func resolveTopBlockDevice(paths Paths, majorMinor, source string) (string, error) {
	if strings.HasPrefix(source, "/dev/") {
		resolved, err := filepath.EvalSymlinks(source)
		if err == nil {
			return filepath.Base(resolved), nil
		}
		return filepath.Base(source), nil
	}

	major, minor, err := parseMajorMinor(majorMinor)
	if err != nil {
		return "", err
	}
	link := filepath.Join(paths.SysDevBlock, fmt.Sprintf("%d:%d", major, minor))
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", fmt.Errorf("resolve block device for %s: %w", majorMinor, err)
	}
	return filepath.Base(resolved), nil
}

func walkDeviceParents(paths Paths, name string, leaves map[string]struct{}) error {
	devicePath := filepath.Join(paths.SysClassBlk, name)

	slaves, err := os.ReadDir(filepath.Join(devicePath, "slaves"))
	if err == nil && len(slaves) > 0 {
		for _, slave := range slaves {
			if err := walkDeviceParents(paths, slave.Name(), leaves); err != nil {
				return err
			}
		}
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read slaves for %s: %w", name, err)
	}

	if isPartition(devicePath) {
		parent, err := partitionParent(devicePath)
		if err != nil {
			return err
		}
		return walkDeviceParents(paths, parent, leaves)
	}

	if _, err := os.Stat(filepath.Join(devicePath, "device")); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s does not look like a physical block device", name)
		}
		return fmt.Errorf("stat device node for %s: %w", name, err)
	}

	leaves[name] = struct{}{}
	return nil
}

func isPartition(devicePath string) bool {
	_, err := os.Stat(filepath.Join(devicePath, "partition"))
	return err == nil
}

func partitionParent(devicePath string) (string, error) {
	resolved, err := filepath.EvalSymlinks(devicePath)
	if err != nil {
		return "", fmt.Errorf("resolve partition path %s: %w", devicePath, err)
	}
	parent := filepath.Base(filepath.Dir(resolved))
	if parent == "." || parent == string(filepath.Separator) || parent == "" {
		return "", fmt.Errorf("unable to determine parent disk for %s", devicePath)
	}
	return parent, nil
}

// resolveSCSIBlockGenericDevice maps a block disk such as sda to its matching
// /dev/sg* node through sysfs. The daemon uses sg nodes for START STOP UNIT to
// avoid the less predictable block-device path on the tested SAS setup.
func resolveSCSIBlockGenericDevice(paths Paths, name string) (string, error) {
	devicePath := filepath.Join(paths.SysClassBlk, name)
	if isPartition(devicePath) {
		parent, err := partitionParent(devicePath)
		if err != nil {
			return "", err
		}
		name = parent
		devicePath = filepath.Join(paths.SysClassBlk, name)
	}

	entries, err := os.ReadDir(filepath.Join(devicePath, "device", "scsi_generic"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no scsi_generic mapping found for %s", name)
		}
		return "", fmt.Errorf("read scsi_generic mapping for %s: %w", name, err)
	}
	if len(entries) != 1 {
		return "", fmt.Errorf("expected exactly one scsi_generic mapping for %s, got %d", name, len(entries))
	}
	return filepath.Join("/dev", entries[0].Name()), nil
}
