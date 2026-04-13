//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	cmd, err := parseCommand(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	paths := defaultPaths()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runCommand(ctx, paths, logger, cmd); err != nil && !errors.Is(err, context.Canceled) {
		if cmd.Name == "debug" {
			os.Exit(1)
		}
		logger.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

type DaemonConfig struct {
	Mountpoints       []string
	IgnoreMountpoints []string
	Auto              bool
	Verbose           bool
	SleepAfter        time.Duration
	SleepAfterMax     time.Duration
	PollInterval      time.Duration
}

type DebugConfig struct {
	Action            string
	Mountpoints       []string
	Devices           []string
	IgnoreMountpoints []string
}

type Command struct {
	Name          string
	Daemon        DaemonConfig
	Debug         DebugConfig
	SystemInstall SystemInstallConfig
}

type SystemInstallConfig struct{}

func parseCommand(args []string) (Command, error) {
	if len(args) == 0 {
		return Command{}, usageError()
	}

	switch args[0] {
	case "daemon":
		cfg, err := parseDaemonConfig(args[1:])
		if err != nil {
			return Command{}, err
		}
		return Command{Name: "daemon", Daemon: cfg}, nil
	case "debug":
		cfg, err := parseDebugConfig(args[1:])
		if err != nil {
			return Command{}, err
		}
		return Command{Name: "debug", Debug: cfg}, nil
	case "system-install":
		cfg, err := parseSystemInstallConfig(args[1:])
		if err != nil {
			return Command{}, err
		}
		return Command{Name: "system-install", SystemInstall: cfg}, nil
	default:
		return Command{}, usageError()
	}
}

func parseSystemInstallConfig(args []string) (SystemInstallConfig, error) {
	fs := flag.NewFlagSet("system-install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage of system-install:")
		fmt.Fprintln(os.Stderr, "  spinherd system-install")
	}
	if err := fs.Parse(args); err != nil {
		return SystemInstallConfig{}, err
	}
	if fs.NArg() > 0 {
		return SystemInstallConfig{}, fmt.Errorf("system-install does not take positional arguments")
	}
	return SystemInstallConfig{}, nil
}

func parseDaemonConfig(args []string) (DaemonConfig, error) {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage of daemon:")
		fmt.Fprintln(os.Stderr, "  spinherd daemon [--ignore-mnt /mnt/storage0 ...] [--sleep-after 10m] [--sleep-after-max 1h] [--poll-interval 1m] [--verbose]")
		fmt.Fprintln(os.Stderr, "  spinherd daemon --mnt /mnt/storage0 [--mnt /mnt/storage1 ...] [--sleep-after 10m] [--sleep-after-max 1h] [--poll-interval 1m] [--verbose]")
	}

	var cfg DaemonConfig
	cfg.SleepAfter = 10 * time.Minute
	cfg.SleepAfterMax = time.Hour
	cfg.PollInterval = time.Minute
	mounts := stringSliceFlag{}
	ignoreMounts := stringSliceFlag{}
	fs.Var(&mounts, "mnt", "mountpoint root to manage, can be repeated")
	fs.Var(&ignoreMounts, "ignore-mnt", "mountpoint root to exclude from auto mode, can be repeated")
	fs.Var(durationFlag{target: &cfg.SleepAfter}, "sleep-after", "idle delay before spin-down, integer with s, m, or h suffix")
	fs.Var(durationFlag{target: &cfg.SleepAfterMax}, "sleep-after-max", "maximum adaptive idle delay, integer with s, m, or h suffix")
	fs.Var(durationFlag{target: &cfg.PollInterval}, "poll-interval", "diskstats polling interval, integer with s, m, or h suffix")
	fs.BoolVar(&cfg.Verbose, "verbose", false, "log each diskstats poll decision")
	if err := fs.Parse(args); err != nil {
		return DaemonConfig{}, err
	}
	cfg.Mountpoints = mounts
	cfg.IgnoreMountpoints = ignoreMounts
	cfg.Auto = len(cfg.Mountpoints) == 0
	switch {
	case !cfg.Auto && len(cfg.IgnoreMountpoints) > 0:
		return DaemonConfig{}, fmt.Errorf("--ignore-mnt is only supported in auto mode")
	case cfg.SleepAfter <= 0:
		return DaemonConfig{}, fmt.Errorf("--sleep-after must be greater than zero")
	case cfg.SleepAfterMax <= 0:
		return DaemonConfig{}, fmt.Errorf("--sleep-after-max must be greater than zero")
	case cfg.SleepAfterMax < cfg.SleepAfter:
		return DaemonConfig{}, fmt.Errorf("--sleep-after-max must be greater than or equal to --sleep-after")
	case cfg.PollInterval <= 0:
		return DaemonConfig{}, fmt.Errorf("--poll-interval must be greater than zero")
	case cfg.PollInterval > cfg.SleepAfter:
		return DaemonConfig{}, fmt.Errorf("--poll-interval must be less than or equal to --sleep-after")
	}
	return cfg, nil
}

func parseDebugConfig(args []string) (DebugConfig, error) {
	if len(args) == 0 {
		return DebugConfig{}, fmt.Errorf("missing debug action\n%s", usageText())
	}

	action := args[0]
	switch action {
	case "fanotify", "resolve", "daemon", "spinup", "spindown":
	default:
		return DebugConfig{}, fmt.Errorf("unknown debug action %q\n%s", action, usageText())
	}

	fs := flag.NewFlagSet("debug "+action, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of debug %s:\n", action)
		switch action {
		case "daemon":
			fmt.Fprintln(os.Stderr, "  spinherd debug daemon [--ignore-mnt /mnt/storage0 ...]")
		case "resolve":
			fmt.Fprintln(os.Stderr, "  spinherd debug resolve --mnt /mnt/storage0 [--mnt /mnt/storage1 ...]")
		case "fanotify":
			fmt.Fprintln(os.Stderr, "  spinherd debug fanotify --mnt /mnt/storage0 [--mnt /mnt/storage1 ...]")
		case "spindown":
			fmt.Fprintln(os.Stderr, "  spinherd debug spindown --mnt /mnt/storage0 [--mnt /mnt/storage1 ...]")
			fmt.Fprintln(os.Stderr, "  spinherd debug spindown --device /dev/sda [--device /dev/sdb ...]")
		case "spinup":
			fmt.Fprintln(os.Stderr, "  spinherd debug spinup --mnt /mnt/storage0 [--mnt /mnt/storage1 ...]")
			fmt.Fprintln(os.Stderr, "  spinherd debug spinup --device /dev/sda [--device /dev/sdb ...]")
		}
	}

	var cfg DebugConfig
	cfg.Action = action
	mounts := stringSliceFlag{}
	devices := stringSliceFlag{}
	ignoreMounts := stringSliceFlag{}
	fs.Var(&mounts, "mnt", "mountpoint root to inspect, can be repeated")
	if isStartStopDebugAction(action) {
		fs.Var(&devices, "device", "block device path like /dev/sda to control directly, can be repeated")
	}
	if action == "daemon" {
		fs.Var(&ignoreMounts, "ignore-mnt", "mountpoint root to exclude from auto mode, can be repeated")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return DebugConfig{}, err
	}
	cfg.Mountpoints = mounts
	cfg.Devices = devices
	cfg.IgnoreMountpoints = ignoreMounts
	for i, device := range cfg.Devices {
		normalized, err := normalizeDeviceFlag(device)
		if err != nil {
			return DebugConfig{}, err
		}
		cfg.Devices[i] = normalized
	}
	if isStartStopDebugAction(cfg.Action) {
		if len(cfg.Mountpoints) > 0 && len(cfg.Devices) > 0 {
			return DebugConfig{}, fmt.Errorf("--mnt and --device cannot be used together")
		}
		if len(cfg.Mountpoints) == 0 && len(cfg.Devices) == 0 {
			return DebugConfig{}, fmt.Errorf("missing required --mnt or --device")
		}
	} else if len(cfg.Devices) > 0 {
		return DebugConfig{}, fmt.Errorf("--device is only supported for debug spinup and debug spindown")
	}
	if cfg.Action != "daemon" && !isStartStopDebugAction(cfg.Action) && len(cfg.Mountpoints) == 0 {
		return DebugConfig{}, fmt.Errorf("missing required --mnt")
	}
	if cfg.Action == "daemon" && len(cfg.Mountpoints) > 0 {
		return DebugConfig{}, fmt.Errorf("debug daemon does not take --mnt")
	}
	return cfg, nil
}

func runCommand(ctx context.Context, paths Paths, logger *log.Logger, cmd Command) error {
	switch cmd.Name {
	case "daemon":
		app := &App{
			cfg:    cmd.Daemon,
			paths:  paths,
			logger: logger,
		}
		return app.Run(ctx)
	case "debug":
		return runDebug(ctx, paths, logger, cmd.Debug)
	case "system-install":
		return runSystemInstall(ctx, logger, cmd.SystemInstall)
	default:
		return fmt.Errorf("unsupported command %q", cmd.Name)
	}
}

func usageError() error {
	return fmt.Errorf("%s", usageText())
}

func usageText() string {
	return strings.TrimSpace(`
usage:
  spinherd daemon [--ignore-mnt /mnt/spinningrust0 ...] [--sleep-after 10m] [--sleep-after-max 1h] [--poll-interval 1m] [--verbose]
  spinherd daemon --mnt /mnt/spinningrust0 [--mnt /mnt/spinningrust1 ...] [--sleep-after 10m] [--sleep-after-max 1h] [--poll-interval 1m] [--verbose]
  spinherd system-install
  spinherd debug daemon [--ignore-mnt /mnt/spinningrust0 ...]
  spinherd debug resolve --mnt /mnt/spinningrust0 [--mnt /mnt/spinningrust1 ...]
  spinherd debug fanotify --mnt /mnt/spinningrust0 [--mnt /mnt/spinningrust1 ...]
  spinherd debug spindown --mnt /mnt/spinningrust0 [--mnt /mnt/spinningrust1 ...]
  spinherd debug spindown --device /dev/sda [--device /dev/sdb ...]
  spinherd debug spinup --mnt /mnt/spinningrust0 [--mnt /mnt/spinningrust1 ...]
  spinherd debug spinup --device /dev/sda [--device /dev/sdb ...]
`)
}

func isStartStopDebugAction(action string) bool {
	switch action {
	case "spinup", "spindown":
		return true
	default:
		return false
	}
}

func normalizeDeviceFlag(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("device value must not be empty")
	}
	if !strings.HasPrefix(value, "/dev/") {
		return "", fmt.Errorf("invalid device %q, use an explicit block device path like /dev/sda", value)
	}
	value = strings.TrimPrefix(value, "/dev/")
	if value == "" || strings.Contains(value, "/") {
		return "", fmt.Errorf("invalid device %q, use an explicit block device path like /dev/sda", "/dev/"+value)
	}
	return value, nil
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type durationFlag struct {
	target *time.Duration
}

func (f durationFlag) String() string {
	if f.target == nil {
		return ""
	}
	return f.target.String()
}

func (f durationFlag) Set(value string) error {
	duration, err := parseSimpleDuration(value)
	if err != nil {
		return err
	}
	*f.target = duration
	return nil
}

func parseSimpleDuration(value string) (time.Duration, error) {
	if len(value) < 2 {
		return 0, fmt.Errorf("invalid duration %q, use integer with s, m, or h suffix", value)
	}

	unit := value[len(value)-1]
	count, err := strconv.ParseInt(value[:len(value)-1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q, use integer with s, m, or h suffix", value)
	}
	if count < 0 {
		return 0, fmt.Errorf("invalid duration %q, value must not be negative", value)
	}

	switch unit {
	case 's':
		return time.Duration(count) * time.Second, nil
	case 'm':
		return time.Duration(count) * time.Minute, nil
	case 'h':
		return time.Duration(count) * time.Hour, nil
	default:
		return 0, fmt.Errorf("invalid duration %q, use integer with s, m, or h suffix", value)
	}
}
