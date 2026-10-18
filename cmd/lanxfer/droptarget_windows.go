//go:build windows

// Drop-target detection for FastCopy-style drag-out.
//
// When the mouse button is released over an Explorer file view or the
// desktop, we cancel the OLE drop (so Explorer never starts its own slow
// copy engine or progress dialog), work out which folder that view shows,
// and download there ourselves with the parallel engine.
package main

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"
)

var (
	procGetCursorPos    = user32.NewProc("GetCursorPos")
	procWindowFromPoint = user32.NewProc("WindowFromPoint")
	procGetAncestor     = user32.NewProc("GetAncestor")
	procGetParent       = user32.NewProc("GetParent")
	procGetClassNameW   = user32.NewProc("GetClassNameW")
	procIsWindowVisible = user32.NewProc("IsWindowVisible")
)

const gaRoot = 2

type dropSpot struct {
	kind int     // spotNone, spotExplorer, spotDesktop, spotSelf
	root uintptr // top-level window
	tab  uintptr // ShellTabWindowClass (Windows 11 tabs), 0 if none
}

const (
	spotNone = iota
	spotExplorer
	spotDesktop
	spotSelf
)

func className(h uintptr) string {
	var buf [256]uint16
	n, _, _ := procGetClassNameW.Call(h, uintptr(unsafe.Pointer(&buf[0])), 256)
	return syscall.UTF16ToString(buf[:n])
}

// spotUnderCursor classifies the window under the mouse. Only cheap Win32
// calls: it runs inside the OLE drag loop.
func spotUnderCursor(self uintptr) dropSpot {
	var pt struct{ X, Y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	h, _, _ := procWindowFromPoint.Call(uintptr(*(*uint64)(unsafe.Pointer(&pt))))
	if h == 0 {
		return dropSpot{}
	}
	root, _, _ := procGetAncestor.Call(h, gaRoot)
	if root == self {
		return dropSpot{kind: spotSelf, root: root}
	}
	// must be over the file list itself (not the tree, address bar, ...)
	inView := false
	var tab uintptr
	for w := h; w != 0 && w != root; {
		switch className(w) {
		case "SHELLDLL_DefView":
			inView = true
		case "ShellTabWindowClass":
			tab = w
		}
		w, _, _ = procGetParent.Call(w)
	}
	if !inView {
		return dropSpot{}
	}
	switch className(root) {
	case "CabinetWClass", "ExploreWClass":
		return dropSpot{kind: spotExplorer, root: root, tab: tab}
	case "Progman", "WorkerW":
		return dropSpot{kind: spotDesktop, root: root}
	}
	return dropSpot{}
}

var (
	iidServiceProvider  = ole.NewGUID("{6D5140C1-7436-11CE-8034-00AA006009FA}")
	sidSTopLevelBrowser = ole.NewGUID("{4C96BE40-915C-11CF-99D3-00AA004AE837}")
	iidIShellBrowser    = ole.NewGUID("{000214E2-0000-0000-C000-000000000046}")
)

// tabWindow returns the ShellTabWindowClass handle of one Shell.Windows item
// (IServiceProvider -> IShellBrowser -> IOleWindow::GetWindow).
func tabWindow(it *ole.IDispatch) uintptr {
	sp, err := it.QueryInterface(iidServiceProvider)
	if err != nil {
		return 0
	}
	defer sp.Release()
	vt := *(**[8]uintptr)(unsafe.Pointer(sp))
	var sb uintptr
	r, _, _ := syscall.SyscallN(vt[3], uintptr(unsafe.Pointer(sp)), uintptr(unsafe.Pointer(sidSTopLevelBrowser)), uintptr(unsafe.Pointer(iidIShellBrowser)), uintptr(unsafe.Pointer(&sb)))
	if r != 0 || sb == 0 {
		return 0
	}
	sbvt := *(**[8]uintptr)(unsafe.Pointer(sb))
	var hwnd uintptr
	syscall.SyscallN(sbvt[3], sb, uintptr(unsafe.Pointer(&hwnd))) // GetWindow
	syscall.SyscallN(sbvt[2], sb)                                 // Release
	return hwnd
}

// folderOf resolves the file-system folder shown at a drop spot.
func folderOf(s dropSpot) (string, error) {
	if s.kind == spotDesktop {
		return windows.KnownFolderPath(windows.FOLDERID_Desktop, 0)
	}
	if s.kind != spotExplorer {
		return "", errors.New("不是资源管理器窗口")
	}
	ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED) // already initialised on the UI thread: harmless
	unk, err := oleutil.CreateObject("Shell.Application")
	if err != nil {
		return "", err
	}
	defer unk.Release()
	sh, err := unk.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return "", err
	}
	defer sh.Release()
	wv, err := oleutil.CallMethod(sh, "Windows")
	if err != nil {
		return "", err
	}
	wins := wv.ToIDispatch()
	defer wins.Release()
	cv, err := oleutil.GetProperty(wins, "Count")
	if err != nil {
		return "", err
	}
	n := int(cv.Val)
	var candidates []string
	for i := 0; i < n; i++ {
		iv, err := oleutil.CallMethod(wins, "Item", i)
		if err != nil || iv.VT != ole.VT_DISPATCH {
			continue
		}
		it := iv.ToIDispatch()
		if it == nil {
			continue
		}
		p, ok := func() (string, bool) {
			defer it.Release()
			hv, err := oleutil.GetProperty(it, "HWND")
			if err != nil || uint32(hv.Val) != uint32(s.root) {
				return "", false
			}
			match := true
			if s.tab != 0 {
				match = tabWindow(it) == s.tab
			}
			path := itemPath(it)
			if path == "" {
				return "", false
			}
			if !match {
				candidates = append(candidates, path)
				return "", false
			}
			return path, true
		}()
		if ok {
			return checkDir(p)
		}
	}
	if len(candidates) == 1 { // tab lookup failed but only one tab in that window
		return checkDir(candidates[0])
	}
	return "", errors.New("找不到该资源管理器窗口当前的文件夹")
}

func itemPath(it *ole.IDispatch) string {
	dv, err := oleutil.GetProperty(it, "Document")
	if err != nil || dv.VT != ole.VT_DISPATCH {
		return ""
	}
	doc := dv.ToIDispatch()
	defer doc.Release()
	fv, err := oleutil.GetProperty(doc, "Folder")
	if err != nil || fv.VT != ole.VT_DISPATCH {
		return ""
	}
	folder := fv.ToIDispatch()
	defer folder.Release()
	sv, err := oleutil.GetProperty(folder, "Self")
	if err != nil || sv.VT != ole.VT_DISPATCH {
		return ""
	}
	self := sv.ToIDispatch()
	defer self.Release()
	pv, err := oleutil.GetProperty(self, "Path")
	if err != nil {
		return ""
	}
	return pv.ToString()
}

func checkDir(p string) (string, error) {
	if strings.HasPrefix(p, "::") {
		return "", errors.New("这里不是普通文件夹（如「此电脑」），请拖到具体的文件夹里")
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", errors.New(p + " 不是文件夹")
	}
	return p, nil
}
