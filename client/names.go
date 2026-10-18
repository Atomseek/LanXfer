package client

import (
	"fmt"
	"hash/fnv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Limits. Android file systems (ext4, f2fs, FUSE) allow 255 BYTES per name,
// so a long Chinese name (3 bytes per char) that is fine on Windows fails on
// the phone. Windows allows 255 UTF-16 units per name, but Explorer drag-drop
// (FILEDESCRIPTORW.cFileName) and many tools are limited to 260 units for the
// whole path, so names going to Windows are kept much shorter.
const (
	PhoneNameMax  = 250 // bytes per name on the phone
	WinNameMax    = 120 // UTF-16 units per name on Windows
	WinDragBudget = 200 // UTF-16 units for a whole relative path in a drag-out
	minNameUnits  = 24
)

// Logf, when set, receives client log lines (category, message). The GUI
// points it at its log file. It must be safe for concurrent use.
var Logf func(cat, msg string)

func logf(cat, format string, a ...any) {
	if f := Logf; f != nil {
		f(cat, fmt.Sprintf(format, a...))
	}
}

func bytesLen(s string) int { return len(s) }
func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// shorten cuts name so measure(name) <= max, keeping the extension and adding
// "~xxxxxx" (hash of the original name) so different long names stay distinct.
func shorten(name string, max int, measure func(string) int) (string, bool) {
	if measure(name) <= max {
		return name, false
	}
	ext := ""
	if i := strings.LastIndex(name, "."); i > 0 && measure(name[i:]) <= 16 && !strings.ContainsAny(name[i:], " ") {
		ext = name[i:]
	}
	base := strings.TrimSuffix(name, ext)
	h := fnv.New32a()
	h.Write([]byte(name))
	suffix := fmt.Sprintf("~%06x", h.Sum32()&0xffffff)
	for measure(base+suffix+ext) > max && base != "" {
		_, sz := utf8.DecodeLastRuneInString(base)
		base = base[:len(base)-sz]
	}
	base = strings.TrimRight(base, ". ")
	out := base + suffix + ext
	if measure(out) > max { // absurdly long extension: cut everything
		out = suffix
	}
	return out, true
}

// PhoneName makes a name fit the phone's file system limit.
func PhoneName(name string) (string, bool) {
	return shorten(name, PhoneNameMax, bytesLen)
}

// WinName makes a phone name legal and short enough for Windows.
func WinName(name string) (string, bool) {
	s := SafeName(name)
	out, cut := shorten(s, WinNameMax, utf16Len)
	return out, cut || out != name
}

// PhonePath applies PhoneName to every component of a slash path and logs
// each name that had to be shortened.
func PhonePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if n, cut := PhoneName(s); cut {
			logf("截断", "上传：名称超过手机 %d 字节上限（%d 字节），「%s」→「%s」", PhoneNameMax, len(s), s, n)
			parts[i] = n
		}
	}
	return strings.Join(parts, "/")
}

// WinParts applies WinName to path components and logs renames.
func WinParts(parts []string) []string {
	out := make([]string, len(parts))
	for i, s := range parts {
		n, changed := WinName(s)
		if changed {
			if utf16Len(SafeName(s)) > WinNameMax {
				logf("截断", "下载：名称过长（%d 字符），「%s」→「%s」", utf16Len(s), s, n)
			} else {
				logf("截断", "下载：含 Windows 不允许的字符，「%s」→「%s」", s, n)
			}
		}
		out[i] = n
	}
	return out
}

// DragNamer builds Windows relative paths for a drag-out so the whole path
// stays within WinDragBudget, while every folder keeps one consistent name.
type DragNamer struct {
	dirs map[string]string // original relative dir path -> short component
}

func NewDragNamer() *DragNamer { return &DragNamer{dirs: map[string]string{}} }

// Name returns the backslash-joined Windows path for a slash relative path.
func (d *DragNamer) Name(rel string, isDir bool) string {
	orig := strings.Split(rel, "/")
	out := make([]string, len(orig))
	used := 0
	for i, s := range orig {
		last := i == len(orig)-1
		key := strings.Join(orig[:i+1], "/")
		if !last || isDir {
			if v, ok := d.dirs[key]; ok {
				out[i] = v
				used += utf16Len(v) + 1
				continue
			}
		}
		n, changed := WinName(s)
		remaining := len(orig) - i
		budget := (WinDragBudget - used) / remaining
		if last {
			budget = WinDragBudget - used
		}
		if budget < minNameUnits {
			budget = minNameUnits
		}
		if utf16Len(n) > budget {
			n, _ = shorten(n, budget, utf16Len)
			changed = true
		}
		if changed {
			logf("截断", "拖出：「%s」→「%s」", s, n)
		}
		if !last || isDir {
			d.dirs[key] = n
		}
		out[i] = n
		used += utf16Len(n) + 1
	}
	res := strings.Join(out, `\`)
	if utf16Len(res) > 259 {
		logf("错误", "拖出：路径仍超过 260 字符，Explorer 可能无法创建：%s", res)
	}
	return res
}
