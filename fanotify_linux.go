//go:build linux

package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const (
	fanCloexec        = 0x00000001
	fanClassNotif     = 0x00000000
	fanReportFID      = 0x00000200
	fanReportDirFID   = 0x00000400
	fanReportName     = 0x00000800
	fanMarkAdd        = 0x00000001
	fanMarkFilesystem = 0x00000100

	fanAccess        = 0x00000001
	fanModify        = 0x00000002
	fanAttrib        = 0x00000004
	fanCloseWrite    = 0x00000008
	fanCloseNoWrite  = 0x00000010
	fanOpen          = 0x00000020
	fanMovedFrom     = 0x00000040
	fanMovedTo       = 0x00000080
	fanCreate        = 0x00000100
	fanDelete        = 0x00000200
	fanDeleteSelf    = 0x00000400
	fanMoveSelf      = 0x00000800
	fanOpenExec      = 0x00001000
	fanQueueOverflow = 0x00004000
	fanEventOnChild  = 0x08000000
	fanOnDir         = 0x40000000
	fanMetaVersion   = 3

	fanInfoTypeFID         = 1
	fanInfoTypeDFIDName    = 2
	fanInfoTypeDFID        = 3
	fanInfoTypeOldDFIDName = 10
	fanInfoTypeNewDFIDName = 12

	linuxATFDCWD = -100
)

type fanotifyEventMetadata struct {
	EventLen    uint32
	Vers        uint8
	Reserved    uint8
	MetadataLen uint16
	Mask        uint64
	FD          int32
	PID         int32
}

type fanotifyEventInfoHeader struct {
	InfoType uint8
	Pad      uint8
	Len      uint16
}

type fanotifyFileHandle struct {
	HandleBytes uint32
	HandleType  int32
	FHandle     []byte
}

type fanotifyEvent struct {
	Mask        uint64
	Mountpoints []string
	InfoType    string
	HandleType  int32
	FileHandle  string
	Name        string
}

type FanotifyEvent = fanotifyEvent

type FanotifyWatcher struct {
	fd         int
	mountpoint string
	selfPID    int32
	events     chan FanotifyEvent
	done       chan struct{}
	closeOnce  sync.Once
}

// NewFanotifyWatcher uses the FID/name reporting mode so directory entry events
// such as create, delete, and rename are visible. This watches the filesystem,
// not only one path subtree, which is acceptable here because any access to the
// same underlying storage should wake the herd.
func NewFanotifyWatcher(mountpoint string) (*FanotifyWatcher, error) {
	fd, err := fanotifyInit(
		fanCloexec|fanClassNotif|fanReportFID|fanReportDirFID|fanReportName,
		syscall.O_RDONLY,
	)
	if err != nil {
		return nil, fmt.Errorf("fanotify_init: %w", explainFanotifyError(err))
	}

	mask := uint64(
		fanAccess |
			fanModify |
			fanAttrib |
			fanOpen |
			fanOpenExec |
			fanCloseWrite |
			fanCloseNoWrite |
			fanMovedFrom |
			fanMovedTo |
			fanCreate |
			fanDelete |
			fanDeleteSelf |
			fanMoveSelf |
			fanEventOnChild |
			fanOnDir,
	)
	if err := fanotifyMark(fd, fanMarkAdd|fanMarkFilesystem, mask, linuxATFDCWD, mountpoint); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("fanotify_mark(%s): %w", mountpoint, explainFanotifyError(err))
	}

	w := &FanotifyWatcher{
		fd:         fd,
		mountpoint: mountpoint,
		selfPID:    int32(os.Getpid()),
		events:     make(chan FanotifyEvent, 16),
		done:       make(chan struct{}),
	}
	go w.readLoop()
	return w, nil
}

func (w *FanotifyWatcher) Events() <-chan FanotifyEvent {
	return w.events
}

func (w *FanotifyWatcher) Close() error {
	var err error
	w.closeOnce.Do(func() {
		close(w.done)
		err = syscall.Close(w.fd)
	})
	return err
}

// readLoop decodes raw fanotify records and forwards only non-self events. The
// self-PID filter avoids the watcher observing its own helper activity and
// turning that into a false wake signal.
func (w *FanotifyWatcher) readLoop() {
	buf := make([]byte, 65536)
	for {
		n, err := syscall.Read(w.fd, buf)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			close(w.events)
			return
		}

		offset := 0
		for offset+int(unsafe.Sizeof(fanotifyEventMetadata{})) <= n {
			meta := (*fanotifyEventMetadata)(unsafe.Pointer(&buf[offset]))
			if meta.EventLen < uint32(unsafe.Sizeof(fanotifyEventMetadata{})) {
				close(w.events)
				return
			}
			if meta.Vers != fanMetaVersion {
				close(w.events)
				return
			}
			if meta.PID == w.selfPID {
				offset += int(meta.EventLen)
				continue
			}

			eventBytes := buf[offset : offset+int(meta.EventLen)]
			event := w.buildEvent(meta, eventBytes)
			if meta.Mask&fanQueueOverflow == 0 {
				select {
				case <-w.done:
					close(w.events)
					return
				case w.events <- event:
				}
			}
			offset += int(meta.EventLen)
		}
	}
}

// buildEvent extracts only the lightweight data we keep for wake/debug
// decisions. Paths are intentionally not reconstructed here because that adds
// extra filesystem work and was observed to create confusing self-generated
// activity.
func (w *FanotifyWatcher) buildEvent(meta *fanotifyEventMetadata, eventBytes []byte) FanotifyEvent {
	event := FanotifyEvent{
		Mask:        meta.Mask,
		Mountpoints: []string{w.mountpoint},
	}
	if meta.FD >= 0 {
		_ = syscall.Close(int(meta.FD))
		return event
	}
	infos := parseFanotifyInfoRecords(eventBytes)
	for _, info := range infos {
		switch info.infoType {
		case fanInfoTypeDFIDName, fanInfoTypeOldDFIDName, fanInfoTypeNewDFIDName:
			event.InfoType = fanotifyInfoTypeName(info.infoType)
			event.HandleType = info.handle.HandleType
			event.FileHandle = hex.EncodeToString(info.handle.FHandle)
			event.Name = strings.TrimSuffix(info.name, "\x00")
			return event
		case fanInfoTypeFID, fanInfoTypeDFID:
			event.InfoType = fanotifyInfoTypeName(info.infoType)
			event.HandleType = info.handle.HandleType
			event.FileHandle = hex.EncodeToString(info.handle.FHandle)
			return event
		}
	}
	return event
}

type parsedFanotifyInfo struct {
	infoType uint8
	handle   fanotifyFileHandle
	name     string
}

// parseFanotifyInfoRecords walks the variable-length info records that follow a
// fanotify metadata header and returns the file-handle/name records we care
// about for debug output.
func parseFanotifyInfoRecords(eventBytes []byte) []parsedFanotifyInfo {
	metaLen := int(unsafe.Sizeof(fanotifyEventMetadata{}))
	if len(eventBytes) < metaLen {
		return nil
	}
	var infos []parsedFanotifyInfo
	offset := metaLen
	for offset+4 <= len(eventBytes) {
		infoType := eventBytes[offset]
		infoLen := int(binary.LittleEndian.Uint16(eventBytes[offset+2 : offset+4]))
		if infoLen < 4 || offset+infoLen > len(eventBytes) {
			break
		}
		payload := eventBytes[offset+4 : offset+infoLen]
		if len(payload) < 12 {
			offset += infoLen
			continue
		}
		handleBytes := int(binary.LittleEndian.Uint32(payload[8:12]))
		if len(payload) < 12+4+handleBytes {
			offset += infoLen
			continue
		}
		handleType := int32(binary.LittleEndian.Uint32(payload[12:16]))
		handleDataStart := 16
		handleDataEnd := handleDataStart + handleBytes
		handle := fanotifyFileHandle{
			HandleBytes: uint32(handleBytes),
			HandleType:  handleType,
			FHandle:     append([]byte(nil), payload[handleDataStart:handleDataEnd]...),
		}
		name := ""
		if handleDataEnd < len(payload) {
			nameBytes := payload[handleDataEnd:]
			if i := bytesIndexByte(nameBytes, 0); i >= 0 {
				nameBytes = nameBytes[:i]
			}
			name = string(nameBytes)
		}
		infos = append(infos, parsedFanotifyInfo{
			infoType: infoType,
			handle:   handle,
			name:     name,
		})
		offset += infoLen
	}
	return infos
}

func bytesIndexByte(buf []byte, value byte) int {
	for i, b := range buf {
		if b == value {
			return i
		}
	}
	return -1
}

func fanotifyInit(flags, eventFlags uint) (int, error) {
	r1, _, errno := syscall.Syscall(syscall.SYS_FANOTIFY_INIT, uintptr(flags), uintptr(eventFlags), 0)
	if errno != 0 {
		return 0, errno
	}
	return int(r1), nil
}

func fanotifyMark(fd int, flags uint, mask uint64, dirfd int, path string) error {
	ptr, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(
		syscall.SYS_FANOTIFY_MARK,
		uintptr(fd),
		uintptr(flags),
		uintptr(mask),
		uintptr(dirfd),
		uintptr(unsafe.Pointer(ptr)),
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func explainFanotifyError(err error) error {
	switch err {
	case syscall.EPERM:
		return fmt.Errorf("%w (fanotify filesystem marks require CAP_SYS_ADMIN or root)", err)
	case syscall.EACCES:
		return fmt.Errorf("%w (insufficient access to mark the filesystem)", err)
	case syscall.EINVAL:
		return fmt.Errorf("%w (fanotify mode or event mask is not supported by this kernel/filesystem)", err)
	default:
		return err
	}
}

var _ = context.Canceled
var _ = os.ErrClosed

func decodeFanotifyMask(mask uint64) []string {
	var events []string
	if mask&fanAccess != 0 {
		events = append(events, "access")
	}
	if mask&fanModify != 0 {
		events = append(events, "modify")
	}
	if mask&fanAttrib != 0 {
		events = append(events, "attrib")
	}
	if mask&fanCloseWrite != 0 {
		events = append(events, "close_write")
	}
	if mask&fanCloseNoWrite != 0 {
		events = append(events, "close_nowrite")
	}
	if mask&fanOpen != 0 {
		events = append(events, "open")
	}
	if mask&fanMovedFrom != 0 {
		events = append(events, "moved_from")
	}
	if mask&fanMovedTo != 0 {
		events = append(events, "moved_to")
	}
	if mask&fanCreate != 0 {
		events = append(events, "create")
	}
	if mask&fanDelete != 0 {
		events = append(events, "delete")
	}
	if mask&fanDeleteSelf != 0 {
		events = append(events, "delete_self")
	}
	if mask&fanMoveSelf != 0 {
		events = append(events, "move_self")
	}
	if mask&fanOpenExec != 0 {
		events = append(events, "open_exec")
	}
	if mask&fanEventOnChild != 0 {
		events = append(events, "event_on_child")
	}
	if mask&fanOnDir != 0 {
		events = append(events, "on_dir")
	}
	return events
}

func fanotifyInfoTypeName(infoType uint8) string {
	switch infoType {
	case fanInfoTypeFID:
		return "fid"
	case fanInfoTypeDFIDName:
		return "dfid_name"
	case fanInfoTypeDFID:
		return "dfid"
	case fanInfoTypeOldDFIDName:
		return "old_dfid_name"
	case fanInfoTypeNewDFIDName:
		return "new_dfid_name"
	default:
		return fmt.Sprintf("info_%d", infoType)
	}
}
