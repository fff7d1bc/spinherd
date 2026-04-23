//go:build linux

package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const (
	sgIO              = 0x2285
	sgDxferNone       = -1
	startStopSenseLen = 255
)

type startStopMode int

const (
	startStopModeDefault startStopMode = iota
	startStopModePowerConditionActive
)

type startStopTransport int

const (
	startStopTransportBlock startStopTransport = iota
	startStopTransportSCSIBlockGeneric
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
	Transport    string
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
	return sendStartStopUnitDetailedMode(devicePath, start, startStopModeDefault)
}

// sendStartStopUnitDetailedMode issues START STOP UNIT over SG_IO and returns
// the transport details that are useful in debug output. The real daemon path
// targets /dev/sg* nodes, while the block-device path is retained only as a
// narrow fallback/debug aid.
func sendStartStopUnitDetailedMode(devicePath string, start bool, mode startStopMode) (startStopResult, error) {
	openFlags, openFlagsLabel := startStopOpenSettings(devicePath)
	file, err := os.OpenFile(devicePath, openFlags, 0)
	if err != nil {
		return startStopResult{}, fmt.Errorf("open %s: %w", devicePath, explainSGIOError(err))
	}
	defer file.Close()

	cmd := startStopCommandWithMode(start, mode)
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
		OpenFlags:    openFlagsLabel,
		Transport:    startStopTransportLabel(devicePath),
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

// startStopCommandWithMode keeps the command shaping in one place so daemon and
// debug paths use the same CDBs. The settled wake path uses the ACTIVE power
// condition rather than the plain start bit.
func startStopCommandWithMode(start bool, mode startStopMode) [6]byte {
	cmd := [6]byte{0x1b, 0x00, 0x00, 0x00, 0x00, 0x00}
	switch mode {
	case startStopModePowerConditionActive:
		if start {
			cmd[4] = 0x10
		}
	default:
		if start {
			cmd[4] = 0x01
		}
	}
	return cmd
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

func startStopTransportLabel(devicePath string) string {
	if strings.HasPrefix(devicePath, "/dev/sg") {
		return "sg"
	}
	return "block"
}

func startStopOpenSettings(devicePath string) (int, string) {
	if strings.HasPrefix(devicePath, "/dev/sg") {
		return os.O_RDWR, "O_RDWR"
	}
	// The block-device path is kept only as an explicit debug fallback. The
	// default daemon/debug transport resolves disks to /dev/sg* and uses that
	// interface instead.
	return os.O_RDONLY, "O_RDONLY"
}

func startStopOpenFlagsLabel(devicePath string) string {
	_, label := startStopOpenSettings(devicePath)
	return label
}
