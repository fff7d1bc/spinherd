//go:build linux

package main

import (
	"encoding/binary"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestParseMountInfoLine(t *testing.T) {
	line := `83 36 9:0 / /mnt/spinningrust0 rw,relatime - xfs /dev/mapper/cryptrust rw,seclabel,attr2,inode64`
	entry, ok, err := parseMountInfoLine(line)
	if err != nil {
		t.Fatalf("parseMountInfoLine returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected parsed entry")
	}
	if entry.Mountpoint != "/mnt/spinningrust0" {
		t.Fatalf("unexpected mountpoint: %q", entry.Mountpoint)
	}
	if entry.Source != "/dev/mapper/cryptrust" {
		t.Fatalf("unexpected source: %q", entry.Source)
	}
	if entry.MajorMinor != "9:0" {
		t.Fatalf("unexpected major:minor: %q", entry.MajorMinor)
	}
}

func TestParseMountInfoLineUnescapesMountpoint(t *testing.T) {
	line := `83 36 9:0 / /mnt/with\040space rw,relatime - xfs /dev/mapper/cryptrust rw`
	entry, ok, err := parseMountInfoLine(line)
	if err != nil {
		t.Fatalf("parseMountInfoLine returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected parsed entry")
	}
	if entry.Mountpoint != "/mnt/with space" {
		t.Fatalf("unexpected mountpoint: %q", entry.Mountpoint)
	}
}

func TestParseSimpleDuration(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"30s", 30 * time.Second},
		{"10m", 10 * time.Minute},
		{"2h", 2 * time.Hour},
		{"0s", 0},
	}
	for _, tc := range tests {
		got, err := parseSimpleDuration(tc.input)
		if err != nil {
			t.Fatalf("parseSimpleDuration(%q) returned error: %v", tc.input, err)
		}
		if got != tc.want {
			t.Fatalf("parseSimpleDuration(%q) = %v want %v", tc.input, got, tc.want)
		}
	}
}

func TestParseSimpleDurationRejectsInvalidValues(t *testing.T) {
	inputs := []string{"", "5", "1d", "500ms", "1h30m", "-1m", "abc"}
	for _, input := range inputs {
		if _, err := parseSimpleDuration(input); err == nil {
			t.Fatalf("expected parseSimpleDuration(%q) to fail", input)
		}
	}
}

func TestParseDiskstatsLine(t *testing.T) {
	name, counters, ok, err := parseDiskstatsLine("   8       0 sda 157698 8844 10138736 116480 65232 41884 9263394 252985 0 88956 369539")
	if err != nil {
		t.Fatalf("parseDiskstatsLine returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected parsed line")
	}
	if name != "sda" {
		t.Fatalf("unexpected device name: %q", name)
	}
	if len(counters) != 11 {
		t.Fatalf("unexpected counter count: %d", len(counters))
	}
}

func TestDiskstatsSamplerReadAndChangedSince(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "diskstats")
	mustWriteFile(t, path, []byte(strings.Join([]string{
		"   8       0 sda 1 2 3 4 5 6 7 8 9 10 11",
		"   8      16 sdb 1 2 3 4 5 6 7 8 9 10 11",
	}, "\n")))

	sampler := DiskstatsSampler{Path: path}
	first, err := sampler.Read([]string{"sda", "sdb"})
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	second, err := sampler.Read([]string{"sda", "sdb"})
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if second.ChangedSince(first) {
		t.Fatal("expected identical snapshots to be unchanged")
	}

	mustWriteFile(t, path, []byte(strings.Join([]string{
		"   8       0 sda 2 2 3 4 5 6 7 8 9 10 11",
		"   8      16 sdb 1 2 3 4 5 6 7 8 9 10 11",
	}, "\n")))
	third, err := sampler.Read([]string{"sda", "sdb"})
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if !third.ChangedSince(second) {
		t.Fatal("expected changed snapshot to be detected")
	}
}

func TestResolveMount(t *testing.T) {
	root := t.TempDir()
	paths := Paths{MountInfo: filepath.Join(root, "mountinfo")}
	mustWriteFile(t, paths.MountInfo, []byte(strings.Join([]string{
		`24 22 8:1 / / rw,relatime - ext4 /dev/sda1 rw`,
		`25 24 9:0 / /mnt/data rw,relatime - xfs /dev/mapper/data rw`,
	}, "\n")))

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	defer func() {
		_ = os.Chdir(wd)
	}()
	if err := os.Chdir("/"); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	mount, err := resolveMount(paths, "mnt/data")
	if err != nil {
		t.Fatalf("resolveMount returned error: %v", err)
	}
	if mount.Mountpoint != "/mnt/data" {
		t.Fatalf("unexpected mountpoint: %q", mount.Mountpoint)
	}
}

func TestResolveMountRejectsNonMountpoint(t *testing.T) {
	root := t.TempDir()
	paths := Paths{MountInfo: filepath.Join(root, "mountinfo")}
	mustWriteFile(t, paths.MountInfo, []byte(`24 22 8:1 / / rw,relatime - ext4 /dev/sda1 rw`))

	_, err := resolveMount(paths, "/not-mounted")
	if err == nil {
		t.Fatal("expected resolveMount to fail")
	}
}

func TestResolveMountPrefersLastMatchingEntry(t *testing.T) {
	root := t.TempDir()
	paths := Paths{MountInfo: filepath.Join(root, "mountinfo")}
	mustWriteFile(t, paths.MountInfo, []byte(strings.Join([]string{
		`24 22 8:1 / /mnt/data rw,relatime - ext4 /dev/old rw`,
		`25 24 9:0 / /mnt/data rw,relatime - xfs /dev/new rw`,
	}, "\n")))

	mount, err := resolveMount(paths, "/mnt/data")
	if err != nil {
		t.Fatalf("resolveMount returned error: %v", err)
	}
	if mount.Source != "/dev/new" {
		t.Fatalf("unexpected mount source: %q", mount.Source)
	}
}

func TestResolvePhysicalDevices(t *testing.T) {
	root := t.TempDir()
	paths := Paths{
		SysClassBlk: filepath.Join(root, "sys/class/block"),
		SysDevBlock: filepath.Join(root, "sys/dev/block"),
	}

	mustMkdirAll(t, filepath.Join(paths.SysClassBlk, "dm-0/slaves"))
	mustMkdirAll(t, filepath.Join(paths.SysClassBlk, "md0/slaves"))
	mustMkdirAll(t, filepath.Join(paths.SysClassBlk, "sda/device"))
	mustSymlink(t, filepath.Join(paths.SysClassBlk, "md0"), filepath.Join(paths.SysClassBlk, "dm-0/slaves/md0"))
	mustSymlink(t, filepath.Join(paths.SysClassBlk, "sdb"), filepath.Join(paths.SysClassBlk, "md0/slaves/sdb"))
	mustMkdirAll(t, filepath.Join(root, "devices/pci0/block/sda/sda1"))
	mustMkdirAll(t, filepath.Join(root, "devices/pci0/block/sdb/device"))
	mustWriteFile(t, filepath.Join(root, "devices/pci0/block/sda/sda1/partition"), []byte("1"))
	mustSymlink(t, filepath.Join(root, "devices/pci0/block/sda/sda1"), filepath.Join(paths.SysClassBlk, "sda1"))
	mustSymlink(t, filepath.Join(root, "devices/pci0/block/sdb"), filepath.Join(paths.SysClassBlk, "sdb"))
	mustSymlink(t, filepath.Join(paths.SysClassBlk, "sda1"), filepath.Join(paths.SysClassBlk, "md0/slaves/sda1"))

	devices, err := resolvePhysicalDevices(paths, "", "/dev/dm-0")
	if err != nil {
		t.Fatalf("resolvePhysicalDevices returned error: %v", err)
	}

	expected := []string{"sda", "sdb"}
	if !reflect.DeepEqual(devices, expected) {
		t.Fatalf("unexpected devices: got %v want %v", devices, expected)
	}
}

func TestResolveSCSIBlockGenericDevice(t *testing.T) {
	root := t.TempDir()
	paths := Paths{
		SysClassBlk: filepath.Join(root, "sys/class/block"),
	}
	mustMkdirAll(t, filepath.Join(paths.SysClassBlk, "sda/device/scsi_generic/sg1"))

	got, err := resolveSCSIBlockGenericDevice(paths, "sda")
	if err != nil {
		t.Fatalf("resolveSCSIBlockGenericDevice returned error: %v", err)
	}
	if got != "/dev/sg1" {
		t.Fatalf("unexpected sg path: %q", got)
	}
}

func TestParseCommandDaemon(t *testing.T) {
	cmd, err := parseCommand([]string{"daemon", "--mnt", "/mnt/data", "--mnt", "/mnt/archive", "--sleep-after", "15m", "--sleep-after-max", "1h", "--poll-interval", "3m"})
	if err != nil {
		t.Fatalf("parseCommand returned error: %v", err)
	}
	if cmd.Name != "daemon" {
		t.Fatalf("unexpected command name: %q", cmd.Name)
	}
	expectedMounts := []string{"/mnt/data", "/mnt/archive"}
	if !reflect.DeepEqual(cmd.Daemon.Mountpoints, expectedMounts) {
		t.Fatalf("unexpected mountpoints: got %v want %v", cmd.Daemon.Mountpoints, expectedMounts)
	}
	if cmd.Daemon.Auto {
		t.Fatal("expected explicit --mnt daemon mode to disable auto mode")
	}
	if cmd.Daemon.SleepAfter != 15*time.Minute {
		t.Fatalf("unexpected sleep-after: %v", cmd.Daemon.SleepAfter)
	}
	if cmd.Daemon.SleepAfterMax != time.Hour {
		t.Fatalf("unexpected sleep-after-max: %v", cmd.Daemon.SleepAfterMax)
	}
	if cmd.Daemon.PollInterval != 3*time.Minute {
		t.Fatalf("unexpected poll-interval: %v", cmd.Daemon.PollInterval)
	}
}

func TestParseCommandSystemInstall(t *testing.T) {
	cmd, err := parseCommand([]string{"system-install"})
	if err != nil {
		t.Fatalf("parseCommand returned error: %v", err)
	}
	if cmd.Name != "system-install" {
		t.Fatalf("unexpected command name: %q", cmd.Name)
	}
}

func TestParseCommandSystemInstallRejectsPositionalArgs(t *testing.T) {
	if _, err := parseCommand([]string{"system-install", "extra"}); err == nil {
		t.Fatal("expected system-install to reject positional arguments")
	}
}

func TestParseCommandDaemonAutoWithIgnoreMounts(t *testing.T) {
	cmd, err := parseCommand([]string{"daemon", "--ignore-mnt", "/mnt/a", "--ignore-mnt", "/mnt/b"})
	if err != nil {
		t.Fatalf("parseCommand returned error: %v", err)
	}
	if !cmd.Daemon.Auto {
		t.Fatal("expected auto mode without --mnt")
	}
	expectedIgnored := []string{"/mnt/a", "/mnt/b"}
	if !reflect.DeepEqual(cmd.Daemon.IgnoreMountpoints, expectedIgnored) {
		t.Fatalf("unexpected ignored mountpoints: got %v want %v", cmd.Daemon.IgnoreMountpoints, expectedIgnored)
	}
}

func TestParseCommandDaemonVerbose(t *testing.T) {
	cmd, err := parseCommand([]string{"daemon", "--verbose"})
	if err != nil {
		t.Fatalf("parseCommand returned error: %v", err)
	}
	if !cmd.Daemon.Verbose {
		t.Fatal("expected verbose mode to be enabled")
	}
}

func TestParseCommandWithoutSubcommandFails(t *testing.T) {
	if _, err := parseCommand([]string{"--mnt", "/mnt/data"}); err == nil {
		t.Fatal("expected error when subcommand is missing")
	}
}

func TestParseCommandDebugFanotify(t *testing.T) {
	cmd, err := parseCommand([]string{"debug", "fanotify", "--mnt", "/mnt/data", "--mnt", "/mnt/archive"})
	if err != nil {
		t.Fatalf("parseCommand returned error: %v", err)
	}
	if cmd.Name != "debug" {
		t.Fatalf("unexpected command name: %q", cmd.Name)
	}
	if cmd.Debug.Action != "fanotify" {
		t.Fatalf("unexpected action: %q", cmd.Debug.Action)
	}
	expectedMounts := []string{"/mnt/data", "/mnt/archive"}
	if !reflect.DeepEqual(cmd.Debug.Mountpoints, expectedMounts) {
		t.Fatalf("unexpected mountpoints: got %v want %v", cmd.Debug.Mountpoints, expectedMounts)
	}
}

func TestParseCommandDebugResolve(t *testing.T) {
	cmd, err := parseCommand([]string{"debug", "resolve", "--mnt", "/mnt/data"})
	if err != nil {
		t.Fatalf("parseCommand returned error: %v", err)
	}
	if cmd.Name != "debug" {
		t.Fatalf("unexpected command name: %q", cmd.Name)
	}
	if cmd.Debug.Action != "resolve" {
		t.Fatalf("unexpected action: %q", cmd.Debug.Action)
	}
	expectedMounts := []string{"/mnt/data"}
	if !reflect.DeepEqual(cmd.Debug.Mountpoints, expectedMounts) {
		t.Fatalf("unexpected mountpoints: got %v want %v", cmd.Debug.Mountpoints, expectedMounts)
	}
}

func TestParseCommandDebugSpinupDevice(t *testing.T) {
	cmd, err := parseCommand([]string{"debug", "spinup", "--device", "/dev/sda", "--device", "/dev/sdb"})
	if err != nil {
		t.Fatalf("parseCommand returned error: %v", err)
	}
	if cmd.Name != "debug" {
		t.Fatalf("unexpected command name: %q", cmd.Name)
	}
	if cmd.Debug.Action != "spinup" {
		t.Fatalf("unexpected action: %q", cmd.Debug.Action)
	}
	if !reflect.DeepEqual(cmd.Debug.Devices, []string{"sda", "sdb"}) {
		t.Fatalf("unexpected devices: %v", cmd.Debug.Devices)
	}
}

func TestParseCommandDebugSpinupRejectsShortDeviceName(t *testing.T) {
	if _, err := parseCommand([]string{"debug", "spinup", "--device", "sda"}); err == nil {
		t.Fatal("expected error when --device is not an explicit /dev path")
	}
}

func TestSystemdServiceText(t *testing.T) {
	got := systemdServiceText("/usr/local/sbin/spinherd")
	wantParts := []string{
		"Description=spinherd storage spindown daemon",
		"After=local-fs.target",
		"ExecStart=/usr/local/sbin/spinherd daemon",
		"Restart=always",
		"WantedBy=multi-user.target",
	}
	for _, part := range wantParts {
		if !strings.Contains(got, part) {
			t.Fatalf("service text missing %q:\n%s", part, got)
		}
	}
}

func TestStartStopCommandWithMode(t *testing.T) {
	if got := startStopCommandWithMode(true, startStopModeDefault); !reflect.DeepEqual(got[:], []byte{0x1b, 0x00, 0x00, 0x00, 0x01, 0x00}) {
		t.Fatalf("unexpected default start command: %v", got)
	}
	if got := startStopCommandWithMode(true, startStopModePowerConditionActive); !reflect.DeepEqual(got[:], []byte{0x1b, 0x00, 0x00, 0x00, 0x10, 0x00}) {
		t.Fatalf("unexpected active power condition command: %v", got)
	}
	if got := startStopCommandWithMode(false, startStopModeDefault); !reflect.DeepEqual(got[:], []byte{0x1b, 0x00, 0x00, 0x00, 0x00, 0x00}) {
		t.Fatalf("unexpected default stop command: %v", got)
	}
}

func TestStartStopOpenSettings(t *testing.T) {
	if flags, label := startStopOpenSettings("/dev/sg1"); flags != os.O_RDWR || label != "O_RDWR" {
		t.Fatalf("unexpected sg open settings: flags=%d label=%q", flags, label)
	}
	if flags, label := startStopOpenSettings("/dev/sda"); flags != os.O_RDONLY || label != "O_RDONLY" {
		t.Fatalf("unexpected block open settings: flags=%d label=%q", flags, label)
	}
}

func TestStartStopBehaviorDefaultsToSG(t *testing.T) {
	if start, _, transport := startStopBehavior("spinup"); !start || transport != startStopTransportSCSIBlockGeneric {
		t.Fatalf("unexpected spinup behavior: start=%v transport=%v", start, transport)
	}
	if start, _, transport := startStopBehavior("spindown"); start || transport != startStopTransportSCSIBlockGeneric {
		t.Fatalf("unexpected spindown behavior: start=%v transport=%v", start, transport)
	}
}

func TestStartStopBehaviorUsesActiveWakeForSpinup(t *testing.T) {
	start, mode, transport := startStopBehavior("spinup")
	if !start {
		t.Fatal("expected spinup behavior to start disks")
	}
	if mode != startStopModePowerConditionActive {
		t.Fatalf("unexpected spinup mode: %v", mode)
	}
	if transport != startStopTransportSCSIBlockGeneric {
		t.Fatalf("unexpected spinup transport: %v", transport)
	}
}

func TestParseCommandDebugLegacySGActionsFail(t *testing.T) {
	actions := []string{"spinup-sg", "spindown-sg", "spinup-immed", "spinup-active", "spinup-immed-sg", "spinup-active-sg"}
	for _, action := range actions {
		if _, err := parseCommand([]string{"debug", action, "--device", "/dev/sda"}); err == nil {
			t.Fatalf("expected legacy action %q to fail", action)
		}
	}
}

func TestParseCommandDebugSpindownRejectsMixedMountAndDevice(t *testing.T) {
	if _, err := parseCommand([]string{"debug", "spindown", "--mnt", "/mnt/data", "--device", "/dev/sda"}); err == nil {
		t.Fatal("expected error when --mnt and --device are mixed")
	}
}

func TestParseCommandDebugResolveRejectsDevice(t *testing.T) {
	if _, err := parseCommand([]string{"debug", "resolve", "--device", "/dev/sda"}); err == nil {
		t.Fatal("expected error when --device is used with debug resolve")
	}
}

func TestParseCommandDebugUnknownAction(t *testing.T) {
	if _, err := parseCommand([]string{"debug", "unknown", "--mnt", "/mnt/data"}); err == nil {
		t.Fatal("expected error for unknown debug action")
	}
}

func TestParseCommandLegacyMountpointFlagFails(t *testing.T) {
	if _, err := parseCommand([]string{"daemon", "--mountpoint", "/mnt/data"}); err == nil {
		t.Fatal("expected error for legacy --mountpoint flag")
	}
}

func TestParseCommandDaemonImplicitAuto(t *testing.T) {
	cmd, err := parseCommand([]string{"daemon", "--sleep-after", "15m", "--sleep-after-max", "1h", "--poll-interval", "3m"})
	if err != nil {
		t.Fatalf("parseCommand returned error: %v", err)
	}
	if cmd.Name != "daemon" || !cmd.Daemon.Auto {
		t.Fatalf("unexpected daemon auto command: %+v", cmd)
	}
	if cmd.Daemon.SleepAfterMax != time.Hour {
		t.Fatalf("unexpected sleep-after-max: %v", cmd.Daemon.SleepAfterMax)
	}
}

func TestParseCommandDaemonLegacyAutoFlagFails(t *testing.T) {
	if _, err := parseCommand([]string{"daemon", "--auto"}); err == nil {
		t.Fatal("expected error for removed --auto flag")
	}
}

func TestParseCommandDaemonIgnoreMountsWithManualModeFails(t *testing.T) {
	if _, err := parseCommand([]string{"daemon", "--mnt", "/mnt/data", "--ignore-mnt", "/mnt/skip"}); err == nil {
		t.Fatal("expected error when --ignore-mnt is used with --mnt")
	}
}

func TestParseCommandDaemonSleepAfterMaxMustNotBeSmaller(t *testing.T) {
	if _, err := parseCommand([]string{"daemon", "--sleep-after", "10m", "--sleep-after-max", "5m"}); err == nil {
		t.Fatal("expected error when --sleep-after-max is smaller than --sleep-after")
	}
}

func TestParseCommandDaemonRejectsUnsupportedDurationSuffixes(t *testing.T) {
	if _, err := parseCommand([]string{"daemon", "--sleep-after", "500ms"}); err == nil {
		t.Fatal("expected error for unsupported duration suffix")
	}
	if _, err := parseCommand([]string{"daemon", "--poll-interval", "1h30m"}); err == nil {
		t.Fatal("expected error for compound duration")
	}
}

func TestParseCommandDaemonDefaultsSleepAfterMaxToOneHour(t *testing.T) {
	cmd, err := parseCommand([]string{"daemon", "--sleep-after", "20m"})
	if err != nil {
		t.Fatalf("parseCommand returned error: %v", err)
	}
	if cmd.Daemon.SleepAfterMax != time.Hour {
		t.Fatalf("unexpected default sleep-after-max: %v", cmd.Daemon.SleepAfterMax)
	}
}

func TestParseCommandDaemonRejectsPollIntervalAboveSleepAfter(t *testing.T) {
	if _, err := parseCommand([]string{"daemon", "--sleep-after", "10m", "--poll-interval", "11m"}); err == nil {
		t.Fatal("expected error for poll interval above sleep-after")
	}
}

func TestParseCommandDebugDaemon(t *testing.T) {
	cmd, err := parseCommand([]string{"debug", "daemon"})
	if err != nil {
		t.Fatalf("parseCommand returned error: %v", err)
	}
	if cmd.Name != "debug" || cmd.Debug.Action != "daemon" {
		t.Fatalf("unexpected debug daemon command: %+v", cmd)
	}
}

func TestParseCommandDebugDaemonWithIgnoreMounts(t *testing.T) {
	cmd, err := parseCommand([]string{"debug", "daemon", "--ignore-mnt", "/mnt/a", "--ignore-mnt", "/mnt/b"})
	if err != nil {
		t.Fatalf("parseCommand returned error: %v", err)
	}
	if !reflect.DeepEqual(cmd.Debug.IgnoreMountpoints, []string{"/mnt/a", "/mnt/b"}) {
		t.Fatalf("unexpected ignored mountpoints: %v", cmd.Debug.IgnoreMountpoints)
	}
}

func TestParseCommandDebugLegacyAutoActionFails(t *testing.T) {
	if _, err := parseCommand([]string{"debug", "auto"}); err == nil {
		t.Fatal("expected error for removed debug auto action")
	}
}

func TestDiskstatsDeviceDeltasSince(t *testing.T) {
	prev := DiskstatsSnapshot{
		"sda": {10, 0, 100, 20, 5, 0, 40, 8, 0, 30, 50},
	}
	current := DiskstatsSnapshot{
		"sda": {11, 0, 108, 24, 5, 0, 40, 8, 0, 34, 54},
	}

	deltas := current.DeviceDeltasSince(prev)
	if len(deltas) != 1 {
		t.Fatalf("unexpected delta count: %d", len(deltas))
	}
	if deltas[0].Device != "sda" {
		t.Fatalf("unexpected delta device: %s", deltas[0].Device)
	}

	got := formatDiskstatsDeltas(deltas)
	want := "sda[reads_completed=+1]"
	if got != want {
		t.Fatalf("unexpected formatted deltas: got %q want %q", got, want)
	}
}

func TestGroupHerds(t *testing.T) {
	herds := groupHerds([]Runtime{
		{Mount: MountInfo{Mountpoint: "/mnt/a", Source: "/dev/dm-0"}, Devices: []string{"sda", "sdb"}},
		{Mount: MountInfo{Mountpoint: "/mnt/b", Source: "/dev/dm-1"}, Devices: []string{"sda", "sdb"}},
		{Mount: MountInfo{Mountpoint: "/mnt/c", Source: "/dev/dm-2"}, Devices: []string{"sdc", "sdd"}},
	})
	if len(herds) != 2 {
		t.Fatalf("unexpected herd count: %d", len(herds))
	}
	if got := herds[0].Mountpoints(); !reflect.DeepEqual(got, []string{"/mnt/a", "/mnt/b"}) {
		t.Fatalf("unexpected grouped mountpoints: %v", got)
	}
	if got := herds[1].Mountpoints(); !reflect.DeepEqual(got, []string{"/mnt/c"}) {
		t.Fatalf("unexpected grouped mountpoints: %v", got)
	}
}

func TestFindContainingMountPrefersLongestMatch(t *testing.T) {
	mount, ok := findContainingMount([]MountInfo{
		{Mountpoint: "/mnt"},
		{Mountpoint: "/mnt/data"},
		{Mountpoint: "/mnt/data/deeper"},
	}, "/mnt/data/deeper/file.bin")
	if !ok {
		t.Fatal("expected containing mount")
	}
	if mount.Mountpoint != "/mnt/data/deeper" {
		t.Fatalf("unexpected mountpoint: %q", mount.Mountpoint)
	}
}

func TestFindContainingMountPrefersLastEntryForSameMountpoint(t *testing.T) {
	mount, ok := findContainingMount([]MountInfo{
		{Mountpoint: "/mnt/data", Source: "/dev/old"},
		{Mountpoint: "/mnt/data", Source: "/dev/new"},
	}, "/mnt/data/file.bin")
	if !ok {
		t.Fatal("expected containing mount")
	}
	if mount.Source != "/dev/new" {
		t.Fatalf("unexpected mount source: %q", mount.Source)
	}
}

func TestNormalizeMountpointSet(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	defer func() {
		_ = os.Chdir(wd)
	}()

	root := t.TempDir()
	if err := os.Chdir(root); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "mnt/data"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	set, err := normalizeMountpointSet([]string{"mnt/data", filepath.Join(root, "mnt/data")})
	if err != nil {
		t.Fatalf("normalizeMountpointSet returned error: %v", err)
	}
	if len(set) != 1 {
		t.Fatalf("unexpected set size: %d", len(set))
	}
}

func TestUniqueSortedStrings(t *testing.T) {
	got := uniqueSortedStrings([]string{"b", "a", "b", "c", "a"})
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected unique sorted values: got %v want %v", got, want)
	}
}

func TestParseFanotifyInfoRecordsDFIDName(t *testing.T) {
	metaLen := int(unsafe.Sizeof(fanotifyEventMetadata{}))
	buf := make([]byte, metaLen)

	info := make([]byte, 4+20+4+4+4)
	info[0] = fanInfoTypeDFIDName
	binary.LittleEndian.PutUint16(info[2:4], uint16(len(info)))
	binary.LittleEndian.PutUint32(info[12:16], 4) // handle bytes
	binary.LittleEndian.PutUint32(info[16:20], uint32(129))
	copy(info[20:24], []byte{0xde, 0xad, 0xbe, 0xef})
	copy(info[24:], []byte("foo\x00"))

	records := parseFanotifyInfoRecords(append(buf, info...))
	if len(records) != 1 {
		t.Fatalf("unexpected record count: %d", len(records))
	}
	if records[0].infoType != fanInfoTypeDFIDName {
		t.Fatalf("unexpected info type: %d", records[0].infoType)
	}
	if records[0].handle.HandleType != 129 {
		t.Fatalf("unexpected handle type: %d", records[0].handle.HandleType)
	}
	if !reflect.DeepEqual(records[0].handle.FHandle, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("unexpected handle data: %v", records[0].handle.FHandle)
	}
	if records[0].name != "foo" {
		t.Fatalf("unexpected name: %q", records[0].name)
	}
}

func TestDecodeFanotifyMaskIncludesFIDEvents(t *testing.T) {
	mask := uint64(fanDelete | fanDeleteSelf | fanAttrib | fanEventOnChild | fanOnDir)
	got := decodeFanotifyMask(mask)
	want := []string{"attrib", "delete", "delete_self", "event_on_child", "on_dir"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected mask decode: got %v want %v", got, want)
	}
}

func TestSingleAndMultiMountpoints(t *testing.T) {
	if got := singleMountpoint([]string{"/mnt/data"}); got != "/mnt/data" {
		t.Fatalf("unexpected single mountpoint: %q", got)
	}
	if got := singleMountpoint([]string{"/mnt/a", "/mnt/b"}); got != "" {
		t.Fatalf("expected empty single mountpoint, got %q", got)
	}
	if got := multiMountpoints([]string{"/mnt/a", "/mnt/b"}); !reflect.DeepEqual(got, []string{"/mnt/a", "/mnt/b"}) {
		t.Fatalf("unexpected multi mountpoints: %v", got)
	}
	if got := multiMountpoints([]string{"/mnt/data"}); got != nil {
		t.Fatalf("expected nil multi mountpoints, got %v", got)
	}
}

func TestConvertHerdsWithDevicePaths(t *testing.T) {
	herds := []Herd{{
		Mounts:  []MountInfo{{Mountpoint: "/mnt/data", Source: "/dev/dm-0"}},
		Devices: []string{"sda", "sdb"},
	}}
	got := convertHerds(herds, true)
	if len(got) != 1 {
		t.Fatalf("unexpected herd count: %d", len(got))
	}
	wantPaths := []string{"/dev/sda", "/dev/sdb"}
	if !reflect.DeepEqual(got[0].DevicePaths, wantPaths) {
		t.Fatalf("unexpected device paths: got %v want %v", got[0].DevicePaths, wantPaths)
	}
}

func TestDiskManagerRunParallelNilLoggerDoesNotPanic(t *testing.T) {
	manager := DiskManager{DeviceNames: []string{}, Logger: nil}
	if err := manager.runParallel(true, startStopModeDefault, 0); err != nil {
		t.Fatalf("runParallel returned error: %v", err)
	}

	manager = DiskManager{DeviceNames: []string{}, Logger: log.New(os.Stderr, "", 0)}
	if err := manager.runParallel(false, startStopModeDefault, 0); err != nil {
		t.Fatalf("runParallel with logger returned error: %v", err)
	}
}

func TestNextSleepAfter(t *testing.T) {
	herd := Herd{Mounts: []MountInfo{{Mountpoint: "/mnt/data"}}}

	got := nextSleepAfter(10*time.Minute, 10*time.Minute, time.Hour, 5*time.Minute, herd, nil)
	if got != 20*time.Minute {
		t.Fatalf("unexpected increased base sleep-after: %v", got)
	}

	got = nextSleepAfter(20*time.Minute, 10*time.Minute, time.Hour, 5*time.Minute, herd, nil)
	if got != 30*time.Minute {
		t.Fatalf("unexpected increased sleep-after: %v", got)
	}

	got = nextSleepAfter(55*time.Minute, 10*time.Minute, time.Hour, 5*time.Minute, herd, nil)
	if got != time.Hour {
		t.Fatalf("unexpected capped sleep-after: %v", got)
	}

	got = nextSleepAfter(30*time.Minute, 10*time.Minute, time.Hour, 45*time.Minute, herd, nil)
	if got != 10*time.Minute {
		t.Fatalf("unexpected reset sleep-after: %v", got)
	}
}

func TestNextSleepAfterKeepsBaseWhenMaxNotHigher(t *testing.T) {
	herd := Herd{Mounts: []MountInfo{{Mountpoint: "/mnt/data"}}}
	got := nextSleepAfter(20*time.Minute, 10*time.Minute, 10*time.Minute, time.Minute, herd, nil)
	if got != 10*time.Minute {
		t.Fatalf("unexpected sleep-after: %v", got)
	}
}

func TestPlanAuto(t *testing.T) {
	paths := setupAutoPlanningFixture(t)

	report, err := planAuto(paths, []string{"/mnt/ignored"})
	if err != nil {
		t.Fatalf("planAuto returned error: %v", err)
	}

	if report.Mode != "auto" {
		t.Fatalf("unexpected mode: %q", report.Mode)
	}
	if !reflect.DeepEqual(report.RootDevices, []string{"sda"}) {
		t.Fatalf("unexpected root devices: %v", report.RootDevices)
	}
	if !reflect.DeepEqual(report.SwapDevices, []string{"sde"}) {
		t.Fatalf("unexpected swap devices: %v", report.SwapDevices)
	}
	if !reflect.DeepEqual(report.SwapEntries, []string{"/mnt/withswap/swapfile"}) {
		t.Fatalf("unexpected swap entries: %v", report.SwapEntries)
	}

	if len(report.Herds) != 1 {
		t.Fatalf("unexpected herd count: %d", len(report.Herds))
	}
	if got := report.Herds[0].Mountpoints(); !reflect.DeepEqual(got, []string{"/mnt/data", "/mnt/data2"}) {
		t.Fatalf("unexpected included herd mountpoints: %v", got)
	}
	if !reflect.DeepEqual(report.Herds[0].Devices, []string{"sdb", "sdc"}) {
		t.Fatalf("unexpected included herd devices: %v", report.Herds[0].Devices)
	}

	excluded := excludedReasonsByMountpoint(report.Excluded)
	assertReasonsContain(t, excluded, "/mnt/ignored", "ignored by user")
	assertReasonsContain(t, excluded, "/mnt/withswap", "shares devices with swap")
	assertReasonsContain(t, excluded, "/mnt/rootmirror", "shares devices with root filesystem")
	assertReasonsContain(t, excluded, "/mnt/mixed", "mixed rotational and non-rotational disks")
	assertReasonsContain(t, excluded, "/mnt/nonrot", "no rotational disks")
	assertReasonsContain(t, excluded, "/mnt/tmpfs", "source is not block-backed")
	assertReasonsContainPrefix(t, excluded, "/mnt/failing", "resolution failed:")
}

func TestPlanAutoPrefersEffectiveOvermountEntry(t *testing.T) {
	paths := setupAutoPlanningFixture(t)
	baseMounts, err := os.ReadFile(paths.MountInfo)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	mustWriteFile(t, paths.MountInfo, []byte(strings.TrimSpace(string(baseMounts))+"\n"+strings.Join([]string{
		`10 1 253:8 / /mnt/overmounted rw,relatime - xfs /dev/dm-8 rw`,
		`11 1 253:7 / /mnt/overmounted rw,relatime - xfs /dev/dm-7 rw`,
	}, "\n")))

	mustBlockDevice(t, paths, "sdg", "1")
	mustBlockDevice(t, paths, "sdh", "0")
	mustStackedDevice(t, paths, "dm-7", "sdg")
	mustStackedDevice(t, paths, "dm-8", "sdh")

	report, err := planAuto(paths, nil)
	if err != nil {
		t.Fatalf("planAuto returned error: %v", err)
	}

	found := false
	for _, herd := range report.Herds {
		if reflect.DeepEqual(herd.Mountpoints(), []string{"/mnt/overmounted"}) && reflect.DeepEqual(herd.Devices, []string{"sdg"}) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected overmounted herd using topmost entry, got %+v", report.Herds)
	}
}

func TestResolveSwapEntryDevicesPrefersEffectiveOvermountEntry(t *testing.T) {
	root := t.TempDir()
	paths := Paths{
		SysClassBlk: filepath.Join(root, "sys/class/block"),
		SysDevBlock: filepath.Join(root, "sys/dev/block"),
	}
	mustBlockDevice(t, paths, "sdg", "1")
	mustBlockDevice(t, paths, "sdh", "1")
	mustStackedDevice(t, paths, "dm-7", "sdg")
	mustStackedDevice(t, paths, "dm-8", "sdh")

	devices, err := resolveSwapEntryDevices(paths, []MountInfo{
		{Mountpoint: "/mnt/swapfs", Source: "/dev/dm-7", MajorMinor: "253:7"},
		{Mountpoint: "/mnt/swapfs", Source: "/dev/dm-8", MajorMinor: "253:8"},
	}, "/mnt/swapfs/swapfile")
	if err != nil {
		t.Fatalf("resolveSwapEntryDevices returned error: %v", err)
	}
	if !reflect.DeepEqual(devices, []string{"sdh"}) {
		t.Fatalf("unexpected devices: %v", devices)
	}
}

func TestPlanHerdsManualMode(t *testing.T) {
	paths := setupAutoPlanningFixture(t)
	herds, report, err := planHerds(paths, []string{"/mnt/data", "/mnt/data2"}, []string{"/mnt/ignored"}, false)
	if err != nil {
		t.Fatalf("planHerds returned error: %v", err)
	}
	if report.Mode != "manual" {
		t.Fatalf("unexpected report mode: %q", report.Mode)
	}
	if len(herds) != 1 {
		t.Fatalf("unexpected herd count: %d", len(herds))
	}
	if got := herds[0].Mountpoints(); !reflect.DeepEqual(got, []string{"/mnt/data", "/mnt/data2"}) {
		t.Fatalf("unexpected mountpoints: %v", got)
	}
}

func TestResolveSwapDevicesBlockAndFile(t *testing.T) {
	root := t.TempDir()
	paths := Paths{
		MountInfo:   filepath.Join(root, "mountinfo"),
		Swaps:       filepath.Join(root, "swaps"),
		SysClassBlk: filepath.Join(root, "sys/class/block"),
		SysDevBlock: filepath.Join(root, "sys/dev/block"),
	}

	mustWriteFile(t, paths.MountInfo, []byte(strings.Join([]string{
		`1 0 8:0 / / rw,relatime - ext4 /dev/sda rw`,
		`2 1 253:1 / /mnt/swapfs rw,relatime - xfs /dev/dm-10 rw`,
	}, "\n")))
	mustWriteFile(t, paths.Swaps, []byte(strings.Join([]string{
		"Filename\tType\tSize\tUsed\tPriority",
		"/dev/sdb partition 1 0 -2",
		"/mnt/swapfs/swapfile file 1 0 -3",
	}, "\n")))

	mustMkdirAll(t, filepath.Join(paths.SysClassBlk, "sda/device"))
	mustMkdirAll(t, filepath.Join(paths.SysClassBlk, "sdb/device"))
	mustMkdirAll(t, filepath.Join(paths.SysClassBlk, "dm-10/slaves"))
	mustSymlink(t, filepath.Join(paths.SysClassBlk, "sdc"), filepath.Join(paths.SysClassBlk, "dm-10/slaves/sdc"))
	mustMkdirAll(t, filepath.Join(paths.SysClassBlk, "sdc/device"))

	entries, err := readMountInfo(paths.MountInfo)
	if err != nil {
		t.Fatalf("readMountInfo returned error: %v", err)
	}
	devices, names, err := resolveSwapDevices(paths, entries)
	if err != nil {
		t.Fatalf("resolveSwapDevices returned error: %v", err)
	}
	if !reflect.DeepEqual(devices, []string{"sdb", "sdc"}) {
		t.Fatalf("unexpected swap devices: %v", devices)
	}
	if !reflect.DeepEqual(names, []string{"/dev/sdb", "/mnt/swapfs/swapfile"}) {
		t.Fatalf("unexpected swap entries: %v", names)
	}
}

func TestResolveSwapEntryDevicesMissingSwapFileMount(t *testing.T) {
	_, err := resolveSwapEntryDevices(Paths{}, nil, "/no/mount/swapfile")
	if err == nil {
		t.Fatal("expected resolveSwapEntryDevices to fail")
	}
}

func TestClassifyRotationalUnexpectedValue(t *testing.T) {
	root := t.TempDir()
	paths := Paths{SysClassBlk: filepath.Join(root, "sys/class/block")}
	mustMkdirAll(t, filepath.Join(paths.SysClassBlk, "sda/queue"))
	mustWriteFile(t, filepath.Join(paths.SysClassBlk, "sda/queue/rotational"), []byte("2\n"))
	if _, _, err := classifyRotational(paths, []string{"sda"}); err == nil {
		t.Fatal("expected classifyRotational to fail")
	}
}

func setupAutoPlanningFixture(t *testing.T) Paths {
	t.Helper()

	root := t.TempDir()
	paths := Paths{
		MountInfo:   filepath.Join(root, "mountinfo"),
		Swaps:       filepath.Join(root, "swaps"),
		SysClassBlk: filepath.Join(root, "sys/class/block"),
		SysDevBlock: filepath.Join(root, "sys/dev/block"),
	}

	mustWriteFile(t, paths.MountInfo, []byte(strings.Join([]string{
		`1 0 8:0 / / rw,relatime - ext4 /dev/sda rw`,
		`2 1 253:0 / /mnt/data rw,relatime - xfs /dev/dm-0 rw`,
		`3 1 253:5 / /mnt/data2 rw,relatime - xfs /dev/dm-5 rw`,
		`4 1 253:1 / /mnt/mixed rw,relatime - xfs /dev/dm-1 rw`,
		`5 1 253:2 / /mnt/nonrot rw,relatime - xfs /dev/dm-2 rw`,
		`6 1 253:3 / /mnt/ignored rw,relatime - xfs /dev/dm-3 rw`,
		`7 1 253:4 / /mnt/withswap rw,relatime - xfs /dev/dm-4 rw`,
		`8 1 8:0 / /mnt/rootmirror rw,relatime - ext4 /dev/sda rw`,
		`9 1 253:9 / /mnt/failing rw,relatime - xfs /dev/dm-9 rw`,
		`10 1 0:45 / /mnt/tmpfs rw,nosuid,nodev - tmpfs tmpfs rw`,
	}, "\n")))
	mustWriteFile(t, paths.Swaps, []byte(strings.Join([]string{
		"Filename\tType\tSize\tUsed\tPriority",
		"/mnt/withswap/swapfile file 1 0 -2",
	}, "\n")))

	mustBlockDevice(t, paths, "sda", "0")
	mustBlockDevice(t, paths, "sdb", "1")
	mustBlockDevice(t, paths, "sdc", "1")
	mustBlockDevice(t, paths, "sdd", "1")
	mustBlockDevice(t, paths, "nvme1n1", "0")
	mustBlockDevice(t, paths, "nvme2n1", "0")
	mustBlockDevice(t, paths, "sde", "1")
	mustBlockDevice(t, paths, "sdf", "1")

	mustStackedDevice(t, paths, "dm-0", "sdb", "sdc")
	mustStackedDevice(t, paths, "dm-5", "sdb", "sdc")
	mustStackedDevice(t, paths, "dm-1", "sdd", "nvme1n1")
	mustStackedDevice(t, paths, "dm-2", "nvme2n1")
	mustStackedDevice(t, paths, "dm-3", "sdf")
	mustStackedDevice(t, paths, "dm-4", "sde")
	mustMkdirAll(t, filepath.Join(paths.SysClassBlk, "dm-9/slaves"))

	return paths
}

func excludedReasonsByMountpoint(excluded []ExcludedMount) map[string][]string {
	out := make(map[string][]string, len(excluded))
	for _, item := range excluded {
		out[item.Mountpoint] = item.Reasons
	}
	return out
}

func assertReasonsContain(t *testing.T, reasonsByMount map[string][]string, mountpoint, reason string) {
	t.Helper()
	reasons, ok := reasonsByMount[mountpoint]
	if !ok {
		t.Fatalf("missing excluded mountpoint %s", mountpoint)
	}
	for _, item := range reasons {
		if item == reason {
			return
		}
	}
	t.Fatalf("missing reason %q for %s in %v", reason, mountpoint, reasons)
}

func assertReasonsContainPrefix(t *testing.T, reasonsByMount map[string][]string, mountpoint, prefix string) {
	t.Helper()
	reasons, ok := reasonsByMount[mountpoint]
	if !ok {
		t.Fatalf("missing excluded mountpoint %s", mountpoint)
	}
	for _, item := range reasons {
		if strings.HasPrefix(item, prefix) {
			return
		}
	}
	t.Fatalf("missing reason prefix %q for %s in %v", prefix, mountpoint, reasons)
}

func mustBlockDevice(t *testing.T, paths Paths, name, rotational string) {
	t.Helper()
	mustMkdirAll(t, filepath.Join(paths.SysClassBlk, name, "device"))
	mustMkdirAll(t, filepath.Join(paths.SysClassBlk, name, "queue"))
	mustWriteFile(t, filepath.Join(paths.SysClassBlk, name, "queue", "rotational"), []byte(rotational+"\n"))
}

func mustStackedDevice(t *testing.T, paths Paths, name string, slaves ...string) {
	t.Helper()
	mustMkdirAll(t, filepath.Join(paths.SysClassBlk, name, "slaves"))
	for _, slave := range slaves {
		mustSymlink(t, filepath.Join(paths.SysClassBlk, slave), filepath.Join(paths.SysClassBlk, name, "slaves", slave))
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustSymlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for symlink %s: %v", path, err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink %s -> %s: %v", path, target, err)
	}
}
