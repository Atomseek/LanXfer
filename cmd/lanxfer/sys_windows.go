//go:build windows

package main

import (
	"encoding/json"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// ---------- config ----------

type Config struct {
	Host      string `json:"host"`  // last phone ip:port
	Token     string `json:"token"` // shown by the Magisk module action
	Conns     int    `json:"conns"` // parallel connections
	HotMods   uint32 `json:"hot_mods"`
	HotKey    uint32 `json:"hot_key"` // virtual-key code
	Autostart bool   `json:"autostart"`
	LastPath  string `json:"last_path"`
	LastLocal string `json:"last_local"` // last "download to" folder
}

const (
	MOD_ALT      = 0x1
	MOD_CONTROL  = 0x2
	MOD_SHIFT    = 0x4
	MOD_WIN      = 0x8
	MOD_NOREPEAT = 0x4000
)

func configPath() string {
	dir, _ := os.UserConfigDir() // %APPDATA%
	return filepath.Join(dir, "LanXfer", "config.json")
}

func loadConfig() *Config {
	c := &Config{Conns: 8, HotMods: MOD_CONTROL | MOD_ALT, HotKey: 'F', LastPath: "/sdcard"}
	if b, err := os.ReadFile(configPath()); err == nil {
		json.Unmarshal(b, c)
	}
	if c.Conns < 1 {
		c.Conns = 8
	}
	return c
}

func (c *Config) save() error {
	if err := os.MkdirAll(filepath.Dir(configPath()), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(configPath(), b, 0o644)
}

// ---------- autostart (HKCU\...\Run) ----------

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runName = "LanXfer"

func setAutostart(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		err := k.DeleteValue(runName)
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue(runName, `"`+exe+`" --tray`)
}

// ---------- global hotkey ----------

var (
	procRegisterHotKey      = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey    = user32.NewProc("UnregisterHotKey")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procPostThreadMessageW  = user32.NewProc("PostThreadMessageW")
	procGetCurrentThreadId  = kernel32.NewProc("GetCurrentThreadId")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procFindWindowW         = user32.NewProc("FindWindowW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procCreateMutexW        = kernel32.NewProc("CreateMutexW")
)

const (
	WM_HOTKEY       = 0x0312
	WM_APP_REHOTKEY = 0x8000 + 1
)

type winMSG struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      [2]int32
}

type hotkeyMgr struct {
	mu       sync.Mutex
	mods, vk uint32
	tid      uintptr
	onPress  func()
	onResult func(ok bool)
}

// start runs the hotkey thread. RegisterHotKey with a NULL window posts
// WM_HOTKEY to this thread's own message queue.
func (h *hotkeyMgr) start() {
	ready := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		h.tid, _, _ = procGetCurrentThreadId.Call()
		var m winMSG
		// create the message queue before anyone posts to it
		procPostThreadMessageW.Call(h.tid, WM_APP_REHOTKEY, 0, 0)
		close(ready)
		registered := false
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(r) <= 0 {
				return
			}
			switch m.Message {
			case WM_HOTKEY:
				if h.onPress != nil {
					h.onPress()
				}
			case WM_APP_REHOTKEY:
				if registered {
					procUnregisterHotKey.Call(0, 1)
				}
				h.mu.Lock()
				mods, vk := h.mods, h.vk
				h.mu.Unlock()
				ok := false
				if vk != 0 {
					r, _, _ := procRegisterHotKey.Call(0, 1, uintptr(mods|MOD_NOREPEAT), uintptr(vk))
					ok = r != 0
				}
				registered = ok
				if h.onResult != nil && vk != 0 {
					h.onResult(ok)
				}
			}
		}
	}()
	<-ready
}

func (h *hotkeyMgr) set(mods, vk uint32) {
	h.mu.Lock()
	h.mods, h.vk = mods, vk
	h.mu.Unlock()
	procPostThreadMessageW.Call(h.tid, WM_APP_REHOTKEY, 0, 0)
}

func hotkeyText(mods, vk uint32) string {
	s := ""
	if mods&MOD_CONTROL != 0 {
		s += "Ctrl+"
	}
	if mods&MOD_ALT != 0 {
		s += "Alt+"
	}
	if mods&MOD_SHIFT != 0 {
		s += "Shift+"
	}
	if mods&MOD_WIN != 0 {
		s += "Win+"
	}
	return s + vkName(vk)
}

func vkName(vk uint32) string {
	switch {
	case vk >= 'A' && vk <= 'Z', vk >= '0' && vk <= '9':
		return string(rune(vk))
	case vk >= 0x70 && vk <= 0x7B:
		return "F" + itoa(int(vk-0x70+1))
	case vk == 0xC0:
		return "`"
	case vk == 0x20:
		return "Space"
	}
	return "?"
}

func itoa(i int) string {
	if i >= 10 {
		return string(rune('0'+i/10)) + string(rune('0'+i%10))
	}
	return string(rune('0' + i))
}

// keyChoices are the keys offered in the settings dialog.
func keyChoices() ([]string, []uint32) {
	var names []string
	var vks []uint32
	for c := 'A'; c <= 'Z'; c++ {
		names, vks = append(names, string(c)), append(vks, uint32(c))
	}
	for c := '0'; c <= '9'; c++ {
		names, vks = append(names, string(c)), append(vks, uint32(c))
	}
	for i := uint32(0); i < 12; i++ {
		names, vks = append(names, vkName(0x70+i)), append(vks, 0x70+i)
	}
	names, vks = append(names, "`", "Space"), append(vks, 0xC0, 0x20)
	return names, vks
}

// ---------- single instance ----------

const windowTitle = "LanXfer"

// ensureSingle returns false (after waking the running copy) if another instance exists.
func ensureSingle() bool {
	name, _ := syscall.UTF16PtrFromString("Local\\LanXferSingleInstance")
	procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if syscall.GetLastError() == syscall.ERROR_ALREADY_EXISTS {
		t, _ := syscall.UTF16PtrFromString(windowTitle)
		if h, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(t))); h != 0 {
			procShowWindow.Call(h, 9) // SW_RESTORE
			procSetForegroundWindow.Call(h)
		}
		return false
	}
	return true
}

// ---------- icon ----------

// appImage draws the tray / window icon: a blue rounded tile with two white arrows.
func appImage() image.Image {
	const n = 32
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	blue := color.RGBA{0x1f, 0x7a, 0xe0, 0xff}
	white := color.RGBA{0xff, 0xff, 0xff, 0xff}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			// rounded corners, radius 6
			dx, dy := 0, 0
			if x < 6 {
				dx = 6 - x
			} else if x > n-7 {
				dx = x - (n - 7)
			}
			if y < 6 {
				dy = 6 - y
			} else if y > n-7 {
				dy = y - (n - 7)
			}
			if dx*dx+dy*dy <= 36 {
				img.Set(x, y, blue)
			}
		}
	}
	// right arrow (upper) and left arrow (lower)
	for x := 7; x <= 24; x++ {
		img.Set(x, 11, white)
		img.Set(x, 12, white)
		img.Set(x, 20, white)
		img.Set(x, 21, white)
	}
	for k := 0; k < 5; k++ {
		for t := 0; t < 2; t++ {
			img.Set(24-k, 11-k+t, white)
			img.Set(24-k, 12+k-t, white)
			img.Set(7+k, 20-k+t, white)
			img.Set(7+k, 21+k-t, white)
		}
	}
	return img
}
