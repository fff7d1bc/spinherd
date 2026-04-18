//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type App struct {
	cfg    DaemonConfig
	paths  Paths
	logger *log.Logger
}

const wakeStartStagger = 10 * time.Millisecond

func (a *App) Run(ctx context.Context) error {
	herds, _, err := planHerds(a.paths, a.cfg.Mountpoints, a.cfg.IgnoreMountpoints, a.cfg.Auto)
	if err != nil {
		return err
	}
	if len(herds) == 0 {
		return fmt.Errorf("no eligible mountpoints found")
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, len(herds))
	var wg sync.WaitGroup

	for _, herd := range herds {
		herd := herd
		a.logger.Printf("watching herd mountpoints=%s sources=%s devices=%s sleep_after=%s sleep_after_max=%s poll_interval=%s",
			strings.Join(herd.Mountpoints(), ","),
			strings.Join(herd.Sources(), ","),
			strings.Join(herd.Devices, ","),
			a.cfg.SleepAfter,
			a.cfg.SleepAfterMax,
			a.cfg.PollInterval,
		)

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.runHerd(runCtx, herd); err != nil && !errors.Is(err, context.Canceled) {
				select {
				case errCh <- err:
				default:
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		cancel()
		<-done
		return ctx.Err()
	case err := <-errCh:
		cancel()
		<-done
		return err
	case <-done:
		return nil
	}
}

func (a *App) runHerd(ctx context.Context, herd Herd) error {
	sampler := DiskstatsSampler{Path: a.paths.Diskstats}
	prev, err := sampler.Read(herd.Devices)
	if err != nil {
		return err
	}

	manager := DiskManager{
		DeviceNames: herd.Devices,
		Paths:       a.paths,
		Logger:      a.logger,
	}

	ticker := time.NewTicker(a.cfg.PollInterval)
	defer ticker.Stop()

	baseSleepAfter := a.cfg.SleepAfter
	currentSleepAfter := baseSleepAfter
	maxSleepAfter := a.cfg.SleepAfterMax
	lastBusy := time.Now()
	sleepStartedAt := time.Time{}
	var watchers *FanotifySet
	state := stateActive
	defer func() {
		closeWatcherSet(watchers)
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-watcherSetEvents(watchers):
			if !ok {
				watchers = nil
				if state != stateActive {
					return fmt.Errorf("fanotify watcher stopped unexpectedly")
				}
				continue
			}
			if state == stateActive {
				continue
			}
			if state == stateSleeping {
				currentSleepAfter = nextSleepAfter(currentSleepAfter, baseSleepAfter, maxSleepAfter, time.Since(sleepStartedAt), herd, a.logger)
			}
			a.logger.Printf("filesystem access detected while idle for mountpoints=%s, waking disks", strings.Join(herd.Mountpoints(), ","))
			closeWatcherSet(watchers)
			watchers = nil
			if err := manager.StartAll(); err != nil {
				return err
			}
			prev, err = sampler.Read(herd.Devices)
			if err != nil {
				return err
			}
			lastBusy = time.Now()
			sleepStartedAt = time.Time{}
			state = stateActive
		case err, ok := <-watcherSetErrors(watchers):
			if ok && err != nil {
				return err
			}
			watchers = nil
			if state != stateActive {
				return fmt.Errorf("fanotify watcher stopped unexpectedly")
			}
		case <-ticker.C:
			if state != stateActive {
				continue
			}
			current, err := sampler.Read(herd.Devices)
			if err != nil {
				return err
			}
			changedDeltas := current.DeviceDeltasSince(prev)
			if len(changedDeltas) > 0 {
				if a.cfg.Verbose {
					a.logger.Printf("diskstats changed for mountpoints=%s details=%s, resetting idle timer",
						strings.Join(herd.Mountpoints(), ","),
						formatDiskstatsDeltas(changedDeltas),
					)
				}
				lastBusy = time.Now()
				prev = current
				continue
			}
			prev = current
			idleFor := time.Since(lastBusy)
			if idleFor < currentSleepAfter {
				if a.cfg.Verbose {
					a.logger.Printf("diskstats unchanged for mountpoints=%s idle_for=%s remaining_until_sleep=%s current_sleep_after=%s",
						strings.Join(herd.Mountpoints(), ","),
						idleFor.Truncate(time.Second),
						(currentSleepAfter - idleFor).Truncate(time.Second),
						currentSleepAfter,
					)
				}
				continue
			}

			a.logger.Printf("devices idle for %s on mountpoints=%s, arming fanotify", currentSleepAfter, strings.Join(herd.Mountpoints(), ","))
			watchers, err = NewFanotifySet(herd.Mountpoints())
			if err != nil {
				return err
			}
			state = stateArming

			current, err = sampler.Read(herd.Devices)
			if err != nil {
				return err
			}
			if current.ChangedSince(prev) {
				a.logger.Printf("disk activity resumed before spin-down on mountpoints=%s, keeping disks awake", strings.Join(herd.Mountpoints(), ","))
				prev = current
				lastBusy = time.Now()
				closeWatcherSet(watchers)
				watchers = nil
				state = stateActive
				continue
			}
			prev = current

			a.logger.Printf("spinning down devices=%s", strings.Join(herd.Devices, ","))
			if err := manager.StopAll(); err != nil {
				return err
			}
			sleepStartedAt = time.Now()
			state = stateSleeping

			select {
			case _, ok := <-watcherSetEvents(watchers):
				if !ok {
					watchers = nil
					return fmt.Errorf("fanotify watcher stopped unexpectedly")
				}
				currentSleepAfter = nextSleepAfter(currentSleepAfter, baseSleepAfter, maxSleepAfter, time.Since(sleepStartedAt), herd, a.logger)
				a.logger.Printf("filesystem access arrived during spin-down for mountpoints=%s, waking disks immediately", strings.Join(herd.Mountpoints(), ","))
				closeWatcherSet(watchers)
				watchers = nil
				if err := manager.StartAll(); err != nil {
					return err
				}
				prev, err = sampler.Read(herd.Devices)
				if err != nil {
					return err
				}
				lastBusy = time.Now()
				sleepStartedAt = time.Time{}
				state = stateActive
			case err, ok := <-watcherSetErrors(watchers):
				if ok && err != nil {
					return err
				}
				watchers = nil
				return fmt.Errorf("fanotify watcher stopped unexpectedly")
			default:
			}
		}
	}
}

func nextSleepAfter(current, base, max, sleptFor time.Duration, herd Herd, logger *log.Logger) time.Duration {
	if max <= base {
		return base
	}
	if sleptFor >= current {
		if logger != nil {
			logger.Printf("sleep cycle lasted %s on mountpoints=%s, resetting sleep-after to %s", sleptFor, strings.Join(herd.Mountpoints(), ","), base)
		}
		return base
	}

	next := current + base
	if next > max {
		next = max
	}
	if next != current && logger != nil {
		logger.Printf("sleep cycle lasted only %s on mountpoints=%s, increasing sleep-after to %s (max %s)", sleptFor, strings.Join(herd.Mountpoints(), ","), next, max)
	}
	return next
}

type Runtime struct {
	Mount   MountInfo
	Devices []string
}

type Herd struct {
	Mounts  []MountInfo
	Devices []string
}

func (h Herd) Mountpoints() []string {
	values := make([]string, 0, len(h.Mounts))
	for _, mount := range h.Mounts {
		values = append(values, mount.Mountpoint)
	}
	sort.Strings(values)
	return values
}

func (h Herd) Sources() []string {
	set := make(map[string]struct{})
	for _, mount := range h.Mounts {
		set[mount.Source] = struct{}{}
	}
	values := make([]string, 0, len(set))
	for source := range set {
		values = append(values, source)
	}
	sort.Strings(values)
	return values
}

func resolveRuntime(paths Paths, mountpoint string) (Runtime, error) {
	mount, err := resolveMount(paths, mountpoint)
	if err != nil {
		return Runtime{}, err
	}
	return runtimeFromMount(paths, mount)
}

func runtimeFromMount(paths Paths, mount MountInfo) (Runtime, error) {
	devices, err := resolvePhysicalDevices(paths, mount.MajorMinor, mount.Source)
	if err != nil {
		return Runtime{}, err
	}
	if len(devices) == 0 {
		return Runtime{}, fmt.Errorf("no physical block devices found for %s", mount.Source)
	}
	return Runtime{
		Mount:   mount,
		Devices: devices,
	}, nil
}

func resolveRuntimes(paths Paths, mountpoints []string) ([]Runtime, error) {
	runtimes := make([]Runtime, 0, len(mountpoints))
	seen := make(map[string]struct{})
	for _, mountpoint := range mountpoints {
		runtime, err := resolveRuntime(paths, mountpoint)
		if err != nil {
			return nil, err
		}
		if _, found := seen[runtime.Mount.Mountpoint]; found {
			continue
		}
		seen[runtime.Mount.Mountpoint] = struct{}{}
		runtimes = append(runtimes, runtime)
	}
	return runtimes, nil
}

func groupHerds(runtimes []Runtime) []Herd {
	byDevices := make(map[string]*Herd)
	for _, runtime := range runtimes {
		key := strings.Join(runtime.Devices, ",")
		herd, found := byDevices[key]
		if !found {
			devices := append([]string(nil), runtime.Devices...)
			herd = &Herd{Devices: devices}
			byDevices[key] = herd
		}
		herd.Mounts = append(herd.Mounts, runtime.Mount)
	}

	keys := make([]string, 0, len(byDevices))
	for key := range byDevices {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	herds := make([]Herd, 0, len(keys))
	for _, key := range keys {
		herd := *byDevices[key]
		sort.Slice(herd.Mounts, func(i, j int) bool {
			return herd.Mounts[i].Mountpoint < herd.Mounts[j].Mountpoint
		})
		herds = append(herds, herd)
	}
	return herds
}

type herdState string

const (
	stateActive   herdState = "active"
	stateArming   herdState = "arming"
	stateSleeping herdState = "sleeping"
)

type FanotifySet struct {
	watchers []*FanotifyWatcher
	events   chan FanotifyEvent
	errs     chan error
	done     chan struct{}
}

type fanotifyWatchSpec struct {
	mountpoints []string
}

func NewFanotifySet(mountpoints []string) (*FanotifySet, error) {
	set := &FanotifySet{
		events: make(chan FanotifyEvent, 16),
		errs:   make(chan error, len(mountpoints)),
		done:   make(chan struct{}),
	}

	specs := make(map[uint64]*fanotifyWatchSpec, len(mountpoints))
	for _, mountpoint := range mountpoints {
		fsKey, err := filesystemKey(mountpoint)
		if err != nil {
			closeWatcherSet(set)
			return nil, fmt.Errorf("stat mountpoint %s: %w", mountpoint, err)
		}
		spec, found := specs[fsKey]
		if !found {
			spec = &fanotifyWatchSpec{}
			specs[fsKey] = spec
		}
		spec.mountpoints = append(spec.mountpoints, mountpoint)
	}

	for _, spec := range specs {
		sort.Strings(spec.mountpoints)
		watcher, err := NewFanotifyWatcher(spec.mountpoints[0])
		if err != nil {
			closeWatcherSet(set)
			return nil, err
		}
		set.watchers = append(set.watchers, watcher)
		go set.forward(watcher, spec.mountpoints)
	}

	return set, nil
}

func (s *FanotifySet) forward(watcher *FanotifyWatcher, mountpoints []string) {
	for {
		select {
		case <-s.done:
			return
		case event, ok := <-watcher.Events():
			if !ok {
				select {
				case <-s.done:
					return
				case s.errs <- fmt.Errorf("fanotify watcher for %s stopped unexpectedly", strings.Join(mountpoints, ",")):
					return
				}
			}
			select {
			case <-s.done:
				return
			case s.events <- FanotifyEvent{
				Mask:        event.Mask,
				Mountpoints: append([]string(nil), mountpoints...),
				InfoType:    event.InfoType,
				HandleType:  event.HandleType,
				FileHandle:  event.FileHandle,
				Name:        event.Name,
			}:
			default:
			}
		}
	}
}

func watcherSetEvents(set *FanotifySet) <-chan FanotifyEvent {
	if set == nil {
		return nil
	}
	return set.events
}

func watcherSetErrors(set *FanotifySet) <-chan error {
	if set == nil {
		return nil
	}
	return set.errs
}

func closeWatcherSet(set *FanotifySet) {
	if set == nil {
		return
	}
	select {
	case <-set.done:
	default:
		close(set.done)
	}
	for _, watcher := range set.watchers {
		_ = watcher.Close()
	}
}

func filesystemKey(path string) (uint64, error) {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Dev), nil
}

type DiskManager struct {
	DeviceNames []string
	Paths       Paths
	Logger      *log.Logger
}

func (m DiskManager) StartAll() error {
	return m.runParallel(true, startStopModePowerConditionActive, wakeStartStagger)
}

func (m DiskManager) StopAll() error {
	return m.runParallel(false, startStopModeDefault, 0)
}

func (m DiskManager) runParallel(start bool, mode startStopMode, stagger time.Duration) error {
	errs := make(chan error, len(m.DeviceNames))
	for i, name := range m.DeviceNames {
		i := i
		name := name
		go func() {
			if stagger > 0 {
				time.Sleep(time.Duration(i) * stagger)
			}
			path, err := resolveSCSIBlockGenericDevice(m.Paths, name)
			if err != nil {
				errs <- err
				return
			}
			if m.Logger != nil {
				if start {
					m.Logger.Printf("start %s (/dev/%s)", path, name)
				} else {
					m.Logger.Printf("stop %s (/dev/%s)", path, name)
				}
			}
			_, err = sendStartStopUnitDetailedMode(path, start, mode)
			errs <- err
		}()
	}

	var joined []error
	for range m.DeviceNames {
		if err := <-errs; err != nil {
			joined = append(joined, err)
		}
	}
	if len(joined) == 0 {
		return nil
	}
	return errors.Join(joined...)
}
