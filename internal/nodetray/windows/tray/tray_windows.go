//go:build windows

package tray

import (
	"errors"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	trayCallbackMessage = 0x8000 + 73
	trayIconID          = 1
	wailsAppIconID      = 3

	wmDestroy        = 0x0002
	wmClose          = 0x0010
	wmNull           = 0x0000
	wmLButtonDblClk  = 0x0203
	wmRButtonUp      = 0x0205
	nimAdd           = 0x00000000
	nimModify        = 0x00000001
	nimDelete        = 0x00000002
	nimSetVersion    = 0x00000004
	nifMessage       = 0x00000001
	nifIcon          = 0x00000002
	nifTip           = 0x00000004
	nifInfo          = 0x00000010
	notifyIconV4     = 4
	niifWarning      = 0x00000002
	mfString         = 0x00000000
	mfGrayed         = 0x00000001
	mfSeparator      = 0x00000800
	tpmRightButton   = 0x00000002
	tpmNonotify      = 0x00000080
	tpmReturnCommand = 0x00000100
)

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	user32                    = windows.NewLazySystemDLL("user32.dll")
	shell32                   = windows.NewLazySystemDLL("shell32.dll")
	procGetModuleHandleW      = kernel32.NewProc("GetModuleHandleW")
	procRegisterClassExW      = user32.NewProc("RegisterClassExW")
	procCreateWindowExW       = user32.NewProc("CreateWindowExW")
	procDestroyWindow         = user32.NewProc("DestroyWindow")
	procDefWindowProcW        = user32.NewProc("DefWindowProcW")
	procRegisterWindowMessage = user32.NewProc("RegisterWindowMessageW")
	procGetMessageW           = user32.NewProc("GetMessageW")
	procTranslateMessage      = user32.NewProc("TranslateMessage")
	procDispatchMessageW      = user32.NewProc("DispatchMessageW")
	procPostMessageW          = user32.NewProc("PostMessageW")
	procPostQuitMessage       = user32.NewProc("PostQuitMessage")
	procLoadIconW             = user32.NewProc("LoadIconW")
	procCreatePopupMenu       = user32.NewProc("CreatePopupMenu")
	procAppendMenuW           = user32.NewProc("AppendMenuW")
	procTrackPopupMenu        = user32.NewProc("TrackPopupMenu")
	procDestroyMenu           = user32.NewProc("DestroyMenu")
	procGetCursorPos          = user32.NewProc("GetCursorPos")
	procSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	procShellNotifyIconW      = shell32.NewProc("Shell_NotifyIconW")
	windowSessions            sync.Map
	trayWndProc               = syscall.NewCallback(windowProc)
)

type notifyIconData struct {
	CbSize           uint32
	HWnd             windows.Handle
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            windows.Handle
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     windows.Handle
}

type windowClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       windows.Handle
}

type nativeMessage struct {
	HWnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Point   nativePoint
	Private uint32
}

type nativePoint struct{ X, Y int32 }

type windowsSession struct {
	mu             sync.Mutex
	hwnd           windows.Handle
	icon           windows.Handle
	module         windows.Handle
	taskbarCreated uint32
	events         func(nativeEvent)
	lockedThread   bool
	removed        bool
}

func Start(options Options) (Controller, error) {
	return startSession(&windowsSession{}, options)
}

func (s *windowsSession) Initialize(events func(nativeEvent)) error {
	runtime.LockOSThread()
	s.lockedThread = true
	s.events = events

	module, _, _ := procGetModuleHandleW.Call(0)
	if module == 0 {
		return ErrUnavailable
	}
	s.module = windows.Handle(module)
	icon, _, _ := procLoadIconW.Call(module, uintptr(wailsAppIconID))
	if icon == 0 {
		return ErrUnavailable
	}
	s.icon = windows.Handle(icon)

	className, err := windows.UTF16PtrFromString("MySingerServerNodeTrayWindowV1")
	if err != nil {
		return ErrUnavailable
	}
	class := windowClassEx{
		CbSize:        uint32(unsafe.Sizeof(windowClassEx{})),
		LpfnWndProc:   trayWndProc,
		HInstance:     s.module,
		HIcon:         s.icon,
		LpszClassName: className,
		HIconSm:       s.icon,
	}
	registered, _, registerErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&class)))
	if registered == 0 && !errors.Is(registerErr, windows.ERROR_CLASS_ALREADY_EXISTS) {
		return ErrUnavailable
	}

	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		0,
		0,
		0, 0, 0, 0,
		0, 0, module, 0,
	)
	if hwnd == 0 {
		return ErrUnavailable
	}
	s.hwnd = windows.Handle(hwnd)
	windowSessions.Store(hwnd, s)

	taskbarName, _ := windows.UTF16PtrFromString("TaskbarCreated")
	taskbarMessage, _, _ := procRegisterWindowMessage.Call(uintptr(unsafe.Pointer(taskbarName)))
	if taskbarMessage == 0 {
		return ErrUnavailable
	}
	s.taskbarCreated = uint32(taskbarMessage)
	return s.addIcon()
}

func (s *windowsSession) Run() error {
	for {
		var message nativeMessage
		result, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		switch result {
		case 0:
			return nil
		case ^uintptr(0):
			return errors.New("tray_message_loop_failed")
		default:
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&message)))
		}
	}
}

func (s *windowsSession) Readd() error { return s.addIcon() }

func (s *windowsSession) addIcon() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hwnd == 0 || s.icon == 0 || s.removed {
		return ErrUnavailable
	}
	data := notifyIconData{
		CbSize:           uint32(unsafe.Sizeof(notifyIconData{})),
		HWnd:             s.hwnd,
		UID:              trayIconID,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: trayCallbackMessage,
		HIcon:            s.icon,
	}
	copyUTF16(data.SzTip[:], "媒体节点控制台")
	if !shellNotify(nimAdd, &data) {
		return errors.New("tray_icon_add_failed")
	}
	data.UVersion = notifyIconV4
	if !shellNotify(nimSetVersion, &data) {
		_ = shellNotify(nimDelete, &data)
		return errors.New("tray_icon_version_failed")
	}
	return nil
}

func (s *windowsSession) ShowMenu(items []Item) (Command, bool, error) {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return "", false, errors.New("tray_menu_failed")
	}
	defer procDestroyMenu.Call(menu)
	commands := make(map[uintptr]Command)
	var nextID uintptr = 1000
	for _, item := range items {
		if item.Separator {
			if ok, _, _ := procAppendMenuW.Call(menu, mfSeparator, 0, 0); ok == 0 {
				return "", false, errors.New("tray_menu_failed")
			}
			continue
		}
		text, err := windows.UTF16PtrFromString(item.Label)
		if err != nil {
			return "", false, errors.New("tray_menu_failed")
		}
		flags := uintptr(mfString)
		if !item.Enabled {
			flags |= mfGrayed
		}
		id := nextID
		nextID++
		if ok, _, _ := procAppendMenuW.Call(menu, flags, id, uintptr(unsafe.Pointer(text))); ok == 0 {
			return "", false, errors.New("tray_menu_failed")
		}
		if item.Command != "" && item.Enabled {
			commands[id] = item.Command
		}
	}
	var point nativePoint
	if ok, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&point))); ok == 0 {
		return "", false, errors.New("tray_menu_failed")
	}
	procSetForegroundWindow.Call(uintptr(s.hwnd))
	selected, _, _ := procTrackPopupMenu.Call(menu, tpmRightButton|tpmNonotify|tpmReturnCommand, uintptr(point.X), uintptr(point.Y), 0, uintptr(s.hwnd), 0)
	procPostMessageW.Call(uintptr(s.hwnd), wmNull, 0, 0)
	command, ok := commands[selected]
	return command, ok, nil
}

func (s *windowsSession) Send(title, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hwnd == 0 || s.removed {
		return errors.New("tray_notification_failed")
	}
	data := notifyIconData{
		CbSize:       uint32(unsafe.Sizeof(notifyIconData{})),
		HWnd:         s.hwnd,
		UID:          trayIconID,
		UFlags:       nifInfo,
		DwInfoFlags:  niifWarning,
		HBalloonIcon: s.icon,
	}
	copyUTF16(data.SzInfoTitle[:], title)
	copyUTF16(data.SzInfo[:], body)
	if !shellNotify(nimModify, &data) {
		return errors.New("tray_notification_failed")
	}
	return nil
}

func (s *windowsSession) RequestClose() error {
	s.mu.Lock()
	hwnd := s.hwnd
	s.mu.Unlock()
	if hwnd == 0 {
		return errors.New("tray_close_failed")
	}
	if ok, _, _ := procPostMessageW.Call(uintptr(hwnd), wmClose, 0, 0); ok == 0 {
		return errors.New("tray_close_failed")
	}
	return nil
}

func (s *windowsSession) Remove() error {
	s.mu.Lock()
	if s.removed {
		s.mu.Unlock()
		return nil
	}
	s.removed = true
	hwnd := s.hwnd
	data := notifyIconData{CbSize: uint32(unsafe.Sizeof(notifyIconData{})), HWnd: hwnd, UID: trayIconID}
	locked := s.lockedThread
	s.lockedThread = false
	s.mu.Unlock()

	if hwnd != 0 {
		_ = shellNotify(nimDelete, &data)
		windowSessions.Delete(uintptr(hwnd))
		procDestroyWindow.Call(uintptr(hwnd))
	}
	if locked {
		runtime.UnlockOSThread()
	}
	return nil
}

func windowProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	if value, ok := windowSessions.Load(hwnd); ok {
		session := value.(*windowsSession)
		switch {
		case message == trayCallbackMessage:
			switch uint32(lParam) & 0xffff {
			case wmLButtonDblClk:
				session.events(eventDoubleClick)
			case wmRButtonUp:
				session.events(eventMenuRequested)
			}
			return 0
		case message == session.taskbarCreated:
			session.events(eventTaskbarCreated)
			return 0
		case message == wmClose:
			procDestroyWindow.Call(hwnd)
			return 0
		case message == wmDestroy:
			procPostQuitMessage.Call(0)
			return 0
		}
	}
	result, _, _ := procDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
	return result
}

func shellNotify(action uintptr, data *notifyIconData) bool {
	result, _, _ := procShellNotifyIconW.Call(action, uintptr(unsafe.Pointer(data)))
	return result != 0
}

func copyUTF16(destination []uint16, value string) {
	encoded, err := windows.UTF16FromString(value)
	if err != nil {
		return
	}
	copy(destination, encoded)
}
