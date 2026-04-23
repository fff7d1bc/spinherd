//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

func runDebug(ctx context.Context, paths Paths, _ *log.Logger, cfg DebugConfig) error {
	switch cfg.Action {
	case "daemon":
		_, report, err := planHerds(paths, nil, cfg.IgnoreMountpoints, true)
		if err != nil {
			return err
		}
		return writeJSON(buildAutoDebugOutput(report))
	}

	var herds []Herd
	if len(cfg.Devices) > 0 {
		herds = []Herd{{Devices: uniqueSortedStrings(cfg.Devices)}}
	} else {
		var resolveErr error
		herds, _, resolveErr = planHerds(paths, cfg.Mountpoints, nil, false)
		if resolveErr != nil {
			return resolveErr
		}
	}

	switch cfg.Action {
	case "resolve":
		return writeJSON(buildResolveDebugOutput(herds))
	case "spinup", "spindown":
		for _, herd := range herds {
			if err := writeJSONLine(debugEvent{
				Timestamp:   time.Now().Format(time.RFC3339Nano),
				Action:      cfg.Action,
				Type:        "herd",
				Mountpoints: herd.Mountpoints(),
				Sources:     herd.Sources(),
				Devices:     herd.Devices,
			}); err != nil {
				return err
			}
			if err := writeJSONLine(debugEvent{
				Timestamp:   time.Now().Format(time.RFC3339Nano),
				Action:      cfg.Action,
				Type:        "progress",
				Status:      "in_progress",
				Mountpoints: herd.Mountpoints(),
				Devices:     herd.Devices,
			}); err != nil {
				return err
			}
			if err := debugStartStopAll(paths, cfg.Action, herd); err != nil {
				_ = writeJSONLine(debugEvent{
					Timestamp:   time.Now().Format(time.RFC3339Nano),
					Action:      cfg.Action,
					Type:        "error",
					Mountpoints: herd.Mountpoints(),
					Devices:     herd.Devices,
					Error:       err.Error(),
				})
				return err
			}
			if err := writeJSONLine(debugEvent{
				Timestamp:   time.Now().Format(time.RFC3339Nano),
				Action:      cfg.Action,
				Type:        "result",
				Mountpoints: herd.Mountpoints(),
				Devices:     herd.Devices,
				Status:      "ok",
			}); err != nil {
				return err
			}
		}
		return nil
	case "fanotify":
		var mountpoints []string
		var sources []string
		var devices []string
		for _, herd := range herds {
			mountpoints = append(mountpoints, herd.Mountpoints()...)
			sources = append(sources, herd.Sources()...)
			devices = append(devices, herd.Devices...)
		}
		if err := writeJSONLine(fanotifyWatchingEvent{
			Timestamp:   time.Now().Format(time.RFC3339Nano),
			Action:      "fanotify",
			Type:        "watching",
			Status:      "waiting",
			Mountpoints: mountpoints,
			Sources:     uniqueSortedStrings(sources),
			Devices:     uniqueSortedStrings(devices),
		}); err != nil {
			return err
		}
		if err := debugFanotify(ctx, mountpoints); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("unsupported debug action %q", cfg.Action)
	}
}

// debugStartStopAll deliberately mirrors the daemon's settled transport and
// timing so runtime experiments are testing the same behavior rather than a
// special-case debug-only path.
func debugStartStopAll(paths Paths, action string, herd Herd) error {
	start, mode, transport := startStopBehavior(action)
	errs := make(chan error, len(herd.Devices))
	var wg sync.WaitGroup
	var writeMu sync.Mutex
	stagger := wakeStartStagger

	emit := func(value any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeJSONLine(value)
	}

	for i, device := range herd.Devices {
		i := i
		device := device
		devicePath, err := startStopDevicePath(paths, device, transport)
		if err != nil {
			return err
		}

		if err := emit(debugDiskEvent{
			Timestamp:   time.Now().Format(time.RFC3339Nano),
			Action:      action,
			Type:        "disk",
			Status:      "starting",
			Mountpoints: herd.Mountpoints(),
			Device:      device,
			DevicePath:  devicePath,
			Transport:   startStopTransportLabel(devicePath),
			OpenFlags:   startStopOpenFlagsLabel(devicePath),
			TimeoutMS:   0,
			Command:     startStopCommandMode(start, mode),
		}); err != nil {
			return err
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			if stagger > 0 {
				time.Sleep(time.Duration(i) * stagger)
			}
			result, err := sendStartStopUnitDetailedMode(devicePath, start, mode)
			if err != nil {
				_ = emit(debugDiskEvent{
					Timestamp:    time.Now().Format(time.RFC3339Nano),
					Action:       action,
					Type:         "disk",
					Status:       "error",
					Mountpoints:  herd.Mountpoints(),
					Device:       device,
					DevicePath:   devicePath,
					Transport:    result.Transport,
					OpenFlags:    result.OpenFlags,
					TimeoutMS:    result.TimeoutMS,
					Command:      result.Command[:],
					SCSIStatus:   result.Status,
					MaskedStatus: result.MaskedStatus,
					MsgStatus:    result.MsgStatus,
					HostStatus:   result.HostStatus,
					DriverStatus: result.DriverStatus,
					DurationMS:   result.DurationMS,
					Sense:        result.Sense,
					Error:        err.Error(),
				})
				errs <- err
				return
			}

			if writeErr := emit(debugDiskEvent{
				Timestamp:    time.Now().Format(time.RFC3339Nano),
				Action:       action,
				Type:         "disk",
				Status:       "ok",
				Mountpoints:  herd.Mountpoints(),
				Device:       device,
				DevicePath:   devicePath,
				Transport:    result.Transport,
				OpenFlags:    result.OpenFlags,
				TimeoutMS:    result.TimeoutMS,
				Command:      result.Command[:],
				SCSIStatus:   result.Status,
				MaskedStatus: result.MaskedStatus,
				MsgStatus:    result.MsgStatus,
				HostStatus:   result.HostStatus,
				DriverStatus: result.DriverStatus,
				DurationMS:   result.DurationMS,
				Sense:        result.Sense,
			}); writeErr != nil {
				errs <- writeErr
				return
			}
		}()
	}

	wg.Wait()
	close(errs)

	var joined []error
	for err := range errs {
		if err != nil {
			joined = append(joined, err)
		}
	}
	if len(joined) == 0 {
		return nil
	}
	return joined[0]
}

func startStopCommand(start bool) []byte {
	cmd := startStopCommandWithMode(start, startStopModeDefault)
	return cmd[:]
}

func startStopCommandMode(start bool, mode startStopMode) []byte {
	cmd := startStopCommandWithMode(start, mode)
	return cmd[:]
}

func startStopBehavior(action string) (bool, startStopMode, startStopTransport) {
	switch action {
	case "spinup":
		return true, startStopModePowerConditionActive, startStopTransportSCSIBlockGeneric
	default:
		return false, startStopModeDefault, startStopTransportSCSIBlockGeneric
	}
}

// startStopDevicePath keeps debug input user-friendly. Callers can still think
// in terms of /dev/sdX, while the real command path is resolved to /dev/sg*.
func startStopDevicePath(paths Paths, device string, transport startStopTransport) (string, error) {
	switch transport {
	case startStopTransportSCSIBlockGeneric:
		return resolveSCSIBlockGenericDevice(paths, device)
	default:
		return filepath.Join("/dev", device), nil
	}
}

func debugFanotify(ctx context.Context, mountpoints []string) error {
	watchers, err := NewFanotifySet(mountpoints)
	if err != nil {
		_ = writeJSONLine(debugEvent{
			Timestamp:   time.Now().Format(time.RFC3339Nano),
			Action:      "fanotify",
			Type:        "error",
			Mountpoints: mountpoints,
			Error:       err.Error(),
		})
		return err
	}
	defer closeWatcherSet(watchers)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-watcherSetEvents(watchers):
			if !ok {
				return fmt.Errorf("fanotify watcher stopped unexpectedly")
			}
			if err := writeJSONLine(fanotifyDebugEvent{
				Timestamp:      time.Now().Format(time.RFC3339Nano),
				Action:         "fanotify",
				Type:           "event",
				Status:         "ok",
				Mountpoint:     singleMountpoint(event.Mountpoints),
				Mountpoints:    multiMountpoints(event.Mountpoints),
				FanotifyMask:   event.Mask,
				FanotifyEvents: decodeFanotifyMask(event.Mask),
				InfoType:       event.InfoType,
				HandleType:     event.HandleType,
				FileHandle:     event.FileHandle,
				Name:           event.Name,
			}); err != nil {
				return err
			}
		case err := <-watcherSetErrors(watchers):
			if err != nil {
				_ = writeJSONLine(debugEvent{
					Timestamp:   time.Now().Format(time.RFC3339Nano),
					Action:      "fanotify",
					Type:        "error",
					Mountpoints: mountpoints,
					Error:       err.Error(),
				})
				return err
			}
		}
	}
}

type debugEvent struct {
	Timestamp      string   `json:"timestamp,omitempty"`
	Action         string   `json:"action"`
	Type           string   `json:"type"`
	Status         string   `json:"status,omitempty"`
	Mountpoints    []string `json:"mountpoints,omitempty"`
	Sources        []string `json:"sources,omitempty"`
	Devices        []string `json:"devices,omitempty"`
	FanotifyMask   uint64   `json:"fanotify_mask,omitempty"`
	FanotifyEvents []string `json:"fanotify_events,omitempty"`
	RelativePath   string   `json:"relative_path,omitempty"`
	Error          string   `json:"error,omitempty"`
}

type fanotifyDebugEvent struct {
	Timestamp      string   `json:"timestamp"`
	Action         string   `json:"action"`
	Type           string   `json:"type"`
	Status         string   `json:"status"`
	Mountpoint     string   `json:"mountpoint,omitempty"`
	Mountpoints    []string `json:"mountpoints,omitempty"`
	FanotifyMask   uint64   `json:"fanotify_mask"`
	FanotifyEvents []string `json:"fanotify_events,omitempty"`
	InfoType       string   `json:"info_type,omitempty"`
	HandleType     int32    `json:"handle_type,omitempty"`
	FileHandle     string   `json:"file_handle,omitempty"`
	Name           string   `json:"name,omitempty"`
}

type fanotifyWatchingEvent struct {
	Timestamp   string   `json:"timestamp"`
	Action      string   `json:"action"`
	Type        string   `json:"type"`
	Status      string   `json:"status"`
	Mountpoints []string `json:"mountpoints,omitempty"`
	Sources     []string `json:"sources,omitempty"`
	Devices     []string `json:"devices,omitempty"`
}

type debugDiskEvent struct {
	Timestamp    string   `json:"timestamp"`
	Action       string   `json:"action"`
	Type         string   `json:"type"`
	Status       string   `json:"status"`
	Mountpoints  []string `json:"mountpoints,omitempty"`
	Device       string   `json:"device"`
	DevicePath   string   `json:"device_path"`
	Transport    string   `json:"transport,omitempty"`
	OpenFlags    string   `json:"open_flags,omitempty"`
	TimeoutMS    uint32   `json:"timeout_ms,omitempty"`
	Command      []byte   `json:"command,omitempty"`
	SCSIStatus   uint8    `json:"scsi_status,omitempty"`
	MaskedStatus uint8    `json:"masked_status,omitempty"`
	MsgStatus    uint8    `json:"msg_status,omitempty"`
	HostStatus   uint16   `json:"host_status,omitempty"`
	DriverStatus uint16   `json:"driver_status,omitempty"`
	DurationMS   uint32   `json:"duration_ms,omitempty"`
	Sense        []byte   `json:"sense,omitempty"`
	Error        string   `json:"error,omitempty"`
}

type debugHerdOutput struct {
	Mountpoints []string `json:"mountpoints"`
	Sources     []string `json:"sources"`
	Devices     []string `json:"devices"`
	DevicePaths []string `json:"device_paths,omitempty"`
}

type debugResolveOutput struct {
	Action    string            `json:"action"`
	HerdCount int               `json:"herd_count"`
	Herds     []debugHerdOutput `json:"herds"`
}

type debugExcludedOutput struct {
	Mountpoint string   `json:"mountpoint"`
	Source     string   `json:"source"`
	Devices    []string `json:"devices"`
	Reasons    []string `json:"reasons"`
}

type debugAutoOutput struct {
	Action        string                `json:"action"`
	Mode          string                `json:"mode"`
	RootDevices   []string              `json:"root_devices"`
	SwapEntries   []string              `json:"swap_entries"`
	SwapDevices   []string              `json:"swap_devices"`
	HerdCount     int                   `json:"herd_count"`
	Herds         []debugHerdOutput     `json:"herds"`
	Excluded      []debugExcludedOutput `json:"excluded"`
	ExcludedCount int                   `json:"excluded_count"`
}

func buildResolveDebugOutput(herds []Herd) debugResolveOutput {
	return debugResolveOutput{
		Action:    "resolve",
		HerdCount: len(herds),
		Herds:     convertHerds(herds, true),
	}
}

func buildAutoDebugOutput(report PlanReport) debugAutoOutput {
	excluded := make([]debugExcludedOutput, 0, len(report.Excluded))
	for _, item := range report.Excluded {
		excluded = append(excluded, debugExcludedOutput{
			Mountpoint: item.Mountpoint,
			Source:     item.Source,
			Devices:    item.Devices,
			Reasons:    item.Reasons,
		})
	}

	return debugAutoOutput{
		Action:        "daemon",
		Mode:          report.Mode,
		RootDevices:   report.RootDevices,
		SwapEntries:   report.SwapEntries,
		SwapDevices:   report.SwapDevices,
		HerdCount:     len(report.Herds),
		Herds:         convertHerds(report.Herds, false),
		Excluded:      excluded,
		ExcludedCount: len(excluded),
	}
}

func convertHerds(herds []Herd, withDevicePaths bool) []debugHerdOutput {
	out := make([]debugHerdOutput, 0, len(herds))
	for _, herd := range herds {
		item := debugHerdOutput{
			Mountpoints: herd.Mountpoints(),
			Sources:     herd.Sources(),
			Devices:     herd.Devices,
		}
		if withDevicePaths {
			for _, device := range herd.Devices {
				item.DevicePaths = append(item.DevicePaths, "/dev/"+device)
			}
		}
		out = append(out, item)
	}
	return out
}

func writeJSON(value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal debug output: %w", err)
	}
	data = append(data, '\n')
	_, err = fmt.Print(string(data))
	return err
}

func writeJSONLine(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal debug output: %w", err)
	}
	data = append(data, '\n')
	_, err = fmt.Print(string(data))
	return err
}

func uniqueSortedStrings(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func singleMountpoint(values []string) string {
	if len(values) == 1 {
		return values[0]
	}
	return ""
}

func multiMountpoints(values []string) []string {
	if len(values) > 1 {
		return values
	}
	return nil
}
