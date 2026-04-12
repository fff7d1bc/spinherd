# spinherd

`spinherd` is a Linux tool for rotational hard disks that should sleep when idle and wake as a herd on the next access.

The main use case is storage that is not needed all the time, for example a home server with SSDs for normal operations and a rotational RAID array that may be touched only a few times a day, or even less often.

`spinherd` resolves mounted filesystems through layered storage stacks down to the physical disks, watches them for idleness, spins them down together, and wakes them together when the storage is accessed again.

The important part is that spin-down and spin-up happen in parallel across the whole herd. That reduces the time from the first access attempt to when data starts flowing again, instead of letting the kernel walk the stack and wake RAID members one by one.

## Build

```
make
make test
make static
```

`make` writes `spinherd` to `build/bin/host/spinherd`. `make static` writes `spinherd-static` to `build/bin/host/spinherd-static`.

## Quick Start

First inspect what `spinherd` would manage:

```
spinherd debug daemon
```

If needed, exclude mountpoints from auto mode:

```
spinherd debug daemon --ignore-mnt /mnt/archive --ignore-mnt /mnt/backup
```

Then run the daemon in an interactive shell with verbose logging before deploying it system-wide:

```
spinherd daemon --verbose
```

Or limit it to specific mountpoints:

```
spinherd daemon --mnt /mnt/storage0 --verbose
```

With `--verbose`, `spinherd` logs each diskstats poll decision so you can see whether the idle timer is advancing or being reset by activity.

The intent is that for most systems the default daemon mode is all that is needed, without configuration changes. `spinherd` tries to make the common case as complete and simple to use as possible.

## How It Works

Default daemon mode scans mounted block-backed filesystems, resolves them to physical disks, keeps only rotational disk sets, excludes storage that shares disks with `/` or swap, and groups mountpoints by identical underlying disks.

Manual mode starts when at least one `--mnt` is given.

The daemon watches disk activity through `/proc/diskstats` and treats completed reads, writes, discards, and flushes as activity. If those counters stay unchanged for `--sleep-after`, it arms fanotify and spins the herd down in parallel.

If the herd wakes up sooner than the current sleep threshold, `spinherd` treats that sleep as too short to be worth it and increases the next sleep threshold by one base `--sleep-after` step, up to `--sleep-after-max`. If a herd stays asleep for at least its current threshold before waking again, the threshold resets back to the base `--sleep-after`.

While a herd is already sleeping, `spinherd` does not keep trying to spin it down again on every timer interval. It waits for access, wakes the herd in parallel, and only then starts a new idle cycle.

`--poll-interval` should usually be left at its default.

## Things To Know

### Disk Own Power Management

Disk firmware power management can interfere with the behavior you want from `spinherd`. Drives with APM or built-in sleep timers may decide to change power state on their own, which can make testing and steady-state behavior less predictable.

If that is happening on your system, consider disabling those features outside `spinherd`, for example through udev `rules.d` or other system-level disk configuration. Managing firmware APM or sleep timers is intentionally out of scope for `spinherd`, so there is no built-in support for changing them.

### Disk Start Stop Handling

The current spin-up and spin-down transport is intentionally based on the behavior used by `hd-idle`.

In practice this means `spinherd` sends SCSI `START STOP UNIT` through `SG_IO` in the style that proved reliable on my hardware. An alternative implementation was tested that matched the behavior used in the `sg_start` tool from `sg3_utils` much more closely, and `sg_start --stop` itself showed that same behavior there. In that setup, disks would bounce back up immediately after stop and the result was not reliable.

The `hd-idle`-style behavior was consistent in runtime testing and kept the disks asleep as expected, so that is the implementation `spinherd` uses now.

### Monitoring For Changes

`spinherd` uses the kernel `fanotify` interface to detect activity that should wake a sleeping herd.

That was chosen over `inotify` to make sure reads and writes through already opened file descriptors are still seen as wake-up triggers. `inotify` is tied to inotify-style path and inode watch scope and is not a good fit for reliably catching that kind of already-open file activity.

## Debugging

The `debug` subcommands exist to ad hoc test parts of `spinherd` at runtime.

Inspect what default daemon mode would do:

```
spinherd debug daemon
spinherd debug daemon --ignore-mnt /mnt/archive
```

Inspect what specific mountpoints resolve to:

```
spinherd debug resolve --mnt /mnt/storage0
spinherd debug resolve --mnt /mnt/archive --mnt /mnt/media
```

Watch runtime filesystem activity that would wake a sleeping herd:

```
spinherd debug fanotify --mnt /mnt/storage0
```

Force spin-down or spin-up:

```
spinherd debug spindown --mnt /mnt/storage0
spinherd debug spinup --mnt /mnt/storage0
spinherd debug spindown --device /dev/sda
spinherd debug spinup --device /dev/sda
```

`debug resolve` and `debug daemon` print pretty JSON. `debug fanotify`, `debug spinup`, and `debug spindown` print JSON lines.

`debug spinup` and `debug spindown` accept either `--mnt` or repeatable `--device`, but not both together. `--device` must be an explicit `/dev/...` block device path.

## Help

```
usage:
  spinherd daemon [--ignore-mnt /mnt/spinningrust0 ...] [--sleep-after 10m] [--sleep-after-max 1h] [--poll-interval 1m] [--verbose]
  spinherd daemon --mnt /mnt/spinningrust0 [--mnt /mnt/spinningrust1 ...] [--sleep-after 10m] [--sleep-after-max 1h] [--poll-interval 1m] [--verbose]
  spinherd debug daemon [--ignore-mnt /mnt/spinningrust0 ...]
  spinherd debug resolve --mnt /mnt/spinningrust0 [--mnt /mnt/spinningrust1 ...]
  spinherd debug fanotify --mnt /mnt/spinningrust0 [--mnt /mnt/spinningrust1 ...]
  spinherd debug spindown --mnt /mnt/spinningrust0 [--mnt /mnt/spinningrust1 ...]
  spinherd debug spindown --device /dev/sda [--device /dev/sdb ...]
  spinherd debug spinup --mnt /mnt/spinningrust0 [--mnt /mnt/spinningrust1 ...]
  spinherd debug spinup --device /dev/sda [--device /dev/sdb ...]
```

## Permissions

In practice, daemon mode usually needs to run as root.

The privileged operations involved are:

- fanotify filesystem marks
- `SG_IO` for SCSI `START STOP UNIT`
- opening the underlying block devices
