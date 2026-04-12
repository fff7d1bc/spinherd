//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	sgIO        = 0x2285
	sgDxferNone = -1
	// This path intentionally mirrors hd-idle's START STOP UNIT transport:
	// O_RDONLY, SG_DXFER_NONE, a 6-byte CDB, a 255-byte sense buffer, and no
	// explicit timeout. In runtime testing on the target hardware, this kept
	// disks spun down as expected, while the sg3_utils/sg_start-like variant
	// (O_RDWR|O_NONBLOCK, 64-byte sense, 120s timeout) caused drives to bounce
	// back up immediately after stop.
	startStopOpenFlags      = os.O_RDONLY
	startStopOpenFlagsLabel = "O_RDONLY"
	startStopSenseLen       = 255
)

type sgIOHdr struct {
	InterfaceID    int32
	DxferDirection int32
	CmdLen         uint8
	MxSbLen        uint8
	IovecCount     uint16
	DxferLen       uint32
	Dxferp         uintptr
	Cmdp           uintptr
	Sbp            uintptr
	Timeout        uint32
	Flags          uint32
	PackID         int32
	UsrPtr         uintptr
	Status         uint8
	MaskedStatus   uint8
	MsgStatus      uint8
	SbLenWr        uint8
	HostStatus     uint16
	DriverStatus   uint16
	Resid          int32
	Duration       uint32
	Info           uint32
}

type startStopResult struct {
	DevicePath   string
	Start        bool
	Command      [6]byte
	OpenFlags    string
	TimeoutMS    uint32
	Status       uint8
	MaskedStatus uint8
	MsgStatus    uint8
	HostStatus   uint16
	DriverStatus uint16
	DurationMS   uint32
	Sense        []byte
}

func sendStartStopUnit(devicePath string, start bool) error {
	_, err := sendStartStopUnitDetailed(devicePath, start)
	return err
}

func sendStartStopUnitDetailed(devicePath string, start bool) (startStopResult, error) {
	file, err := os.OpenFile(devicePath, startStopOpenFlags, 0)
	if err != nil {
		return startStopResult{}, fmt.Errorf("open %s: %w", devicePath, explainSGIOError(err))
	}
	defer file.Close()

	cmd := [6]byte{0x1b, 0x00, 0x00, 0x00, 0x00, 0x00}
	if start {
		cmd[4] = 0x01
	}
	sense := make([]byte, startStopSenseLen)
	hdr := sgIOHdr{
		InterfaceID:    int32('S'),
		DxferDirection: sgDxferNone,
		CmdLen:         uint8(len(cmd)),
		MxSbLen:        uint8(len(sense)),
		Cmdp:           uintptr(unsafe.Pointer(&cmd[0])),
		Sbp:            uintptr(unsafe.Pointer(&sense[0])),
	}

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), uintptr(sgIO), uintptr(unsafe.Pointer(&hdr))); errno != 0 {
		return startStopResult{}, fmt.Errorf("SG_IO %s: %w", devicePath, explainSGIOError(errno))
	}

	result := startStopResult{
		DevicePath:   devicePath,
		Start:        start,
		Command:      cmd,
		OpenFlags:    startStopOpenFlagsLabel,
		TimeoutMS:    hdr.Timeout,
		Status:       hdr.Status,
		MaskedStatus: hdr.MaskedStatus,
		MsgStatus:    hdr.MsgStatus,
		HostStatus:   hdr.HostStatus,
		DriverStatus: hdr.DriverStatus,
		DurationMS:   hdr.Duration,
		Sense:        append([]byte(nil), sense[:hdr.SbLenWr]...),
	}
	if hdr.MaskedStatus != 0 {
		return result, fmt.Errorf(
			"SCSI START STOP UNIT failed for %s: status=%d masked=%d host=%d driver=%d sense=% x",
			devicePath,
			hdr.Status,
			hdr.MaskedStatus,
			hdr.HostStatus,
			hdr.DriverStatus,
			result.Sense,
		)
	}
	return result, nil
}

func explainSGIOError(err error) error {
	switch err {
	case syscall.EPERM:
		return fmt.Errorf("%w (SG_IO usually requires CAP_SYS_RAWIO or root)", err)
	case syscall.EACCES:
		return fmt.Errorf("%w (missing permission to open or control the block device)", err)
	default:
		return err
	}
}
