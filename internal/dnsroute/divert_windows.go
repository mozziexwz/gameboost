//go:build windows

package dnsroute

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	flagDrop     = 1 << 1
	flagOutbound = 1 << 17
	flagLoopback = 1 << 18
)

// Network-layer WINDIVERT_ADDRESS from the bundled 2.2.2-A header.
type address struct {
	timestamp int64
	flags     uint32
	reserved  uint32
	network   [64]byte
}

type driver struct {
	library                                               *windows.DLL
	open, receive, send, checksum, parse, shutdown, close *windows.Proc
}

func loadDriver(path string) (*driver, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.LoadLibraryEx(abs, 0, windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		return nil, err
	}
	dll := &windows.DLL{Name: abs, Handle: h}
	d := &driver{library: dll}
	for _, item := range []struct {
		proc **windows.Proc
		name string
	}{
		{&d.open, "Open"}, {&d.receive, "Recv"}, {&d.send, "Send"},
		{&d.checksum, "HelperCalcChecksums"}, {&d.parse, "HelperParsePacket"},
		{&d.shutdown, "Shutdown"}, {&d.close, "Close"},
	} {
		*item.proc, err = dll.FindProc("WinDivert" + item.name)
		if err != nil {
			_ = dll.Release()
			return nil, err
		}
	}
	return d, nil
}

func (d *driver) capture(filter string, priority, flags uintptr) (uintptr, error) {
	s, err := windows.BytePtrFromString(filter)
	if err != nil {
		return 0, err
	}
	for attempt := 0; ; attempt++ {
		h, _, callErr := d.open.Call(uintptr(unsafe.Pointer(s)), 0, priority, flags)
		if h != uintptr(windows.InvalidHandle) {
			return h, nil
		}
		// WFP can still be deleting callouts from the preceding driver
		// instance even after the Service Control Manager reports STOPPED.
		if !errors.Is(callErr, windows.ERROR_OBJECT_ALREADY_EXISTS) || attempt >= 5 {
			return 0, fmt.Errorf("打开 WinDivert 过滤器失败：%w", callErr)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func (d *driver) recv(h uintptr, buf []byte, addr *address) (int, error) {
	var length uint32
	ok, _, err := d.receive.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		uintptr(unsafe.Pointer(&length)), uintptr(unsafe.Pointer(addr)))
	if ok == 0 {
		return 0, err
	}
	return int(length), nil
}

func (d *driver) headers(p []byte) (tcp, udp int, payload []byte) {
	var t, u, body *byte
	var length uint32
	base := unsafe.Pointer(&p[0])
	ok, _, _ := d.parse.Call(uintptr(base), uintptr(len(p)), 0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&t)), uintptr(unsafe.Pointer(&u)),
		uintptr(unsafe.Pointer(&body)), uintptr(unsafe.Pointer(&length)), 0, 0)
	if ok != 0 {
		if t != nil {
			tcp = int(uintptr(unsafe.Pointer(t)) - uintptr(base))
		}
		if u != nil {
			udp = int(uintptr(unsafe.Pointer(u)) - uintptr(base))
		}
		if body != nil && length > 0 {
			start := int(uintptr(unsafe.Pointer(body)) - uintptr(base))
			payload = p[start : start+int(length)]
		}
	}
	runtime.KeepAlive(p)
	return
}

func (d *driver) inject(h uintptr, packet []byte, addr *address, recalc bool) error {
	if recalc {
		ok, _, err := d.checksum.Call(uintptr(unsafe.Pointer(&packet[0])), uintptr(len(packet)),
			uintptr(unsafe.Pointer(addr)), 0)
		if ok == 0 {
			return err
		}
	}
	ok, _, err := d.send.Call(h, uintptr(unsafe.Pointer(&packet[0])), uintptr(len(packet)), 0,
		uintptr(unsafe.Pointer(addr)))
	if ok == 0 {
		return err
	}
	return nil
}
