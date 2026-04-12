//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

type DiskstatsSampler struct {
	Path string
}

type DiskstatsSnapshot map[string][]uint64

type DiskstatsFieldDelta struct {
	Name  string
	Delta int64
}

type DiskstatsDeviceDelta struct {
	Device string
	Fields []DiskstatsFieldDelta
}

var diskstatsFieldNames = []string{
	"reads_completed",
	"reads_merged",
	"sectors_read",
	"time_reading_ms",
	"writes_completed",
	"writes_merged",
	"sectors_written",
	"time_writing_ms",
	"ios_in_progress",
	"time_doing_ios_ms",
	"weighted_time_doing_ios_ms",
	"discards_completed",
	"discards_merged",
	"sectors_discarded",
	"time_discarding_ms",
	"flush_requests_completed",
	"time_flushing_ms",
}

var idleActivityFieldIndexes = map[int]struct{}{
	0:  {}, // reads_completed
	4:  {}, // writes_completed
	11: {}, // discards_completed
	15: {}, // flush_requests_completed
}

func (s DiskstatsSampler) Read(deviceNames []string) (DiskstatsSnapshot, error) {
	wanted := make(map[string]struct{}, len(deviceNames))
	for _, name := range deviceNames {
		wanted[name] = struct{}{}
	}

	file, err := os.Open(s.Path)
	if err != nil {
		return nil, fmt.Errorf("open diskstats: %w", err)
	}
	defer file.Close()

	snapshot := make(DiskstatsSnapshot, len(deviceNames))
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		name, counters, ok, err := parseDiskstatsLine(scanner.Text())
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if _, found := wanted[name]; found {
			snapshot[name] = counters
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read diskstats: %w", err)
	}

	for _, name := range deviceNames {
		if _, found := snapshot[name]; !found {
			return nil, fmt.Errorf("device %s not found in %s", name, s.Path)
		}
	}
	return snapshot, nil
}

func parseDiskstatsLine(line string) (string, []uint64, bool, error) {
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return "", nil, false, nil
	}

	counters := make([]uint64, 0, len(fields)-3)
	for _, field := range fields[3:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return "", nil, false, fmt.Errorf("parse diskstats line %q: %w", line, err)
		}
		counters = append(counters, value)
	}
	return fields[2], counters, true, nil
}

func (d DiskstatsSnapshot) ChangedSince(prev DiskstatsSnapshot) bool {
	return len(d.ChangedDevicesSince(prev)) > 0
}

func (d DiskstatsSnapshot) ChangedDevicesSince(prev DiskstatsSnapshot) []string {
	deltas := d.DeviceDeltasSince(prev)
	var changed []string
	for _, delta := range deltas {
		changed = append(changed, delta.Device)
	}
	return changed
}

func (d DiskstatsSnapshot) DeviceDeltasSince(prev DiskstatsSnapshot) []DiskstatsDeviceDelta {
	var deltas []DiskstatsDeviceDelta
	for name, counters := range d {
		prevCounters, ok := prev[name]
		if !ok || len(prevCounters) != len(counters) {
			fields := make([]DiskstatsFieldDelta, 0, len(counters))
			for i, value := range counters {
				fields = append(fields, DiskstatsFieldDelta{
					Name:  diskstatsFieldName(i),
					Delta: int64(value),
				})
			}
			deltas = append(deltas, DiskstatsDeviceDelta{Device: name, Fields: fields})
			continue
		}

		var fields []DiskstatsFieldDelta
		for i := range counters {
			if _, tracked := idleActivityFieldIndexes[i]; !tracked {
				continue
			}
			if counters[i] == prevCounters[i] {
				continue
			}
			fields = append(fields, DiskstatsFieldDelta{
				Name:  diskstatsFieldName(i),
				Delta: int64(counters[i]) - int64(prevCounters[i]),
			})
		}
		if len(fields) > 0 {
			deltas = append(deltas, DiskstatsDeviceDelta{Device: name, Fields: fields})
		}
	}
	sort.Slice(deltas, func(i, j int) bool {
		return deltas[i].Device < deltas[j].Device
	})
	return deltas
}

func diskstatsFieldName(index int) string {
	if index >= 0 && index < len(diskstatsFieldNames) {
		return diskstatsFieldNames[index]
	}
	return fmt.Sprintf("field_%d", index+1)
}

func formatDiskstatsDeltas(deltas []DiskstatsDeviceDelta) string {
	parts := make([]string, 0, len(deltas))
	for _, delta := range deltas {
		fieldParts := make([]string, 0, len(delta.Fields))
		for _, field := range delta.Fields {
			fieldParts = append(fieldParts, fmt.Sprintf("%s=%+d", field.Name, field.Delta))
		}
		parts = append(parts, fmt.Sprintf("%s[%s]", delta.Device, strings.Join(fieldParts, ",")))
	}
	return strings.Join(parts, "; ")
}
