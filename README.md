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

## System Install

```
spinherd system-install
```

`system-install` copies the currently running binary to `/usr/local/sbin/spinherd`, writes `/etc/systemd/system/spinherd.service`, runs `systemctl daemon-reload`, enables the service for boot, and starts it.

If `spinherd` is already running from `/usr/local/sbin/spinherd`, the copy step is skipped. If it is being run from anywhere else, it is copied over even if the target already exists, so the command also acts as an upgrade path.

## Quick Start

First inspect what `spinherd` would manage.

```
spinherd debug daemon
```

If needed, exclude mountpoints from default daemon mode.

```
spinherd debug daemon --ignore-mnt /mnt/archive --ignore-mnt /mnt/backup
```

Then run the daemon in an interactive shell with verbose logging before deploying it system-wide.

```
spinherd daemon --verbose
```

Or limit it to specific mountpoints.

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

While a herd is already sleeping, `spinherd` does not keep trying to spin it down again on every timer interval. It waits for access, wakes the herd together with a very small per-disk stagger, and only then starts a new idle cycle.

`--poll-interval` should usually be left at its default.

## Things To Know

### Disk Own Power Management

Disk firmware power management can interfere with the behavior you want from `spinherd`. Drives with APM or built-in sleep timers may decide to change power state on their own, which can make testing and steady-state behavior less predictable.

If that is happening on your system, consider disabling those features outside `spinherd`, for example through udev `rules.d` or other system-level disk configuration. Managing firmware APM or sleep timers is intentionally out of scope for `spinherd`, so there is no built-in support for changing them.

### Disk Start Stop Handling

The current spin-up and spin-down transport is intentionally based on the behavior used by `hd-idle`, but the commands are now sent through `/dev/sg*` device nodes rather than normal block devices.

In practice this means `spinherd` resolves each disk to its matching `/dev/sg*` node and sends SCSI `START STOP UNIT` through `SG_IO` there.

An alternative implementation was tested that matched the behavior used in the `sg_start` tool from `sg3_utils` much more closely. On my system, that caused disks to bounce back up immediately after stop. `sg_start --stop` itself showed the same behavior.

That lines up with what `sg_start(8)` already documents. When `--stop` is sent through a normal block device such as `/dev/sdX`, the Linux block layer may notice the disk spinning down and decide to spin it back up again. The `sg_start` documentation recommends using `/dev/sg*` or `/dev/bsg/*` to avoid that class of issue.

The useful detail here is that this was not just about the SCSI command itself. The command was essentially the same, but the transport details around it were different. The `sg_start`-style variant used a more aggressive `/dev/sdX` access pattern, while the `hd-idle`-style variant uses a simpler one. On my hardware, the simpler `hd-idle`-style path works reliably even through `/dev/sdX`, while the `sg_start`-style one does not.

Linux does support `SG_IO` through normal block devices such as `/dev/sdX`, so that was never some accidental or unsupported trick. But after further testing on my SAS setup, issuing the commands through the matching `/dev/sg*` nodes turned out to be the safer path, especially for wake-up after long sleep periods.

So the practical takeaway is that the issue seems to be the `sg_start`-style access pattern on `/dev/sdX`, not merely the fact that `/dev/sdX` exists. `spinherd` now resolves disks to `/dev/sg*` and uses that interface for its real start and stop commands.

### Monitoring For Changes

`spinherd` uses the kernel `fanotify` interface to detect activity that should wake a sleeping herd.

That was chosen over `inotify` to make sure reads and writes through already opened file descriptors are still seen as wake-up triggers. `inotify` is tied to inotify-style path and inode watch scope and is not a good fit for reliably catching that kind of already-open file activity.

There is an important limitation. Mount-level metadata probes such as the `stat` and `statfs` pattern used early by tools like `xfs_fsr` do not currently show up in `spinherd`'s fanotify watcher. In practice that means one disk may begin waking through the normal kernel path before `spinherd` sees a later fanotify-visible event and wakes the rest of the herd.

### Runtime example

Example of running on a system with 6 disks in RAID6, dmcrypt luks and xfs mounted under `/mnt/spinningrust0`. The `daemon` executed without any switches or extra configuration.

```
hagane ~ # spinherd daemon
2026/04/12 23:25:12.390434 watching herd mountpoints=/mnt/spinningrust0 sources=/dev/mapper/enc_spinningrust0 devices=sda,sdb,sdc,sdd,sde,sdf sleep_after=10m0s sleep_after_max=1h0m0s poll_interval=1m0s
2026/04/12 23:43:12.450806 devices idle for 10m0s on mountpoints=/mnt/spinningrust0, arming fanotify
2026/04/12 23:43:12.450922 spinning down devices=sda,sdb,sdc,sdd,sde,sdf
2026/04/12 23:43:12.450939 stop /dev/sg6 (/dev/sdf)
2026/04/12 23:43:12.451047 stop /dev/sg2 (/dev/sdb)
2026/04/12 23:43:12.451214 stop /dev/sg4 (/dev/sdd)
2026/04/12 23:43:12.451289 stop /dev/sg5 (/dev/sde)
2026/04/12 23:43:12.451353 stop /dev/sg3 (/dev/sdc)
2026/04/12 23:43:12.451424 stop /dev/sg1 (/dev/sda)
2026/04/13 07:01:57.479259 sleep cycle lasted 7h18m42.759859459s on mountpoints=/mnt/spinningrust0, resetting sleep-after to 10m0s
2026/04/13 07:01:57.479317 filesystem access detected while idle for mountpoints=/mnt/spinningrust0, waking disks
2026/04/13 07:01:57.479333 start /dev/sg6 (/dev/sdf)
2026/04/13 07:01:57.479333 start /dev/sg2 (/dev/sdb)
2026/04/13 07:01:57.479362 start /dev/sg4 (/dev/sdd)
2026/04/13 07:01:57.479381 start /dev/sg5 (/dev/sde)
2026/04/13 07:01:57.479409 start /dev/sg3 (/dev/sdc)
2026/04/13 07:01:57.479475 start /dev/sg1 (/dev/sda)
```


## Debugging

The `debug` subcommands exist to ad hoc test parts of `spinherd` at runtime.

Inspect what default daemon mode would do.

```
spinherd debug daemon
spinherd debug daemon --ignore-mnt /mnt/archive
```

Inspect what specific mountpoints resolve to.

```
spinherd debug resolve --mnt /mnt/storage0
spinherd debug resolve --mnt /mnt/archive --mnt /mnt/media
```

Print sysfs identity information for the disks that default daemon mode would manage, or for disks under specific mountpoints.

```
spinherd debug disks-info
spinherd debug disks-info --mnt /mnt/storage0
```

Watch runtime filesystem activity that would wake a sleeping herd.

```
spinherd debug fanotify --mnt /mnt/storage0
```

Force spin-down or spin-up.

```
spinherd debug spindown --mnt /mnt/storage0
spinherd debug spinup --mnt /mnt/storage0
spinherd debug spindown --device /dev/sda
spinherd debug spinup --device /dev/sda
```

`debug resolve`, `debug daemon`, and `debug disks-info` print pretty JSON. `debug fanotify`, `debug spinup`, and `debug spindown` print JSON lines.

The regular start and stop debug commands already use the `/dev/sg*` transport internally. When you pass `--device /dev/sdf`, `spinherd` resolves it to the matching `/dev/sg*` node before sending the command.

`debug spinup` and `debug spindown` accept either `--mnt` or repeatable `--device`, but not both together. `--device` must be an explicit `/dev/...` block device path. `debug spinup` follows the same wake path as the daemon, including the `/dev/sg*` transport, the `ACTIVE` wake mode, and the small per-disk stagger. `debug spindown` also uses the `/dev/sg*` transport and the same command path, with a small stagger to keep runtime testing close to the wake-side timing behavior.

## Help

```
usage:
  spinherd daemon [--ignore-mnt /mnt/spinningrust0 ...] [--sleep-after 10m] [--sleep-after-max 1h] [--poll-interval 1m] [--verbose]
  spinherd daemon --mnt /mnt/spinningrust0 [--mnt /mnt/spinningrust1 ...] [--sleep-after 10m] [--sleep-after-max 1h] [--poll-interval 1m] [--verbose]
  spinherd system-install
  spinherd debug daemon [--ignore-mnt /mnt/spinningrust0 ...]
  spinherd debug resolve --mnt /mnt/spinningrust0 [--mnt /mnt/spinningrust1 ...]
  spinherd debug disks-info [--mnt /mnt/spinningrust0 ...]
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
