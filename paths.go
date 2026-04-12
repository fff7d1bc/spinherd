//go:build linux

package main

type Paths struct {
	MountInfo   string
	Diskstats   string
	Swaps       string
	SysClassBlk string
	SysDevBlock string
}

func defaultPaths() Paths {
	return Paths{
		MountInfo:   "/proc/self/mountinfo",
		Diskstats:   "/proc/diskstats",
		Swaps:       "/proc/swaps",
		SysClassBlk: "/sys/class/block",
		SysDevBlock: "/sys/dev/block",
	}
}
