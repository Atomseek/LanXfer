//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"lanxfer/client"
	"lanxfer/xlog"
)

const appVersion = "1.2.0"

func logPath() string {
	dir, _ := os.UserConfigDir() // %APPDATA%
	d := filepath.Join(dir, "LanXfer")
	os.MkdirAll(d, 0o755)
	return filepath.Join(d, "lanxfer.log")
}

func (a *app) initLog() {
	a.log = xlog.Open(logPath())
	client.Logf = a.log.Log
	a.log.Logf("启动", "LanXfer %s 启动，日志文件：%s", appVersion, a.log.Path())
}

// log filters shown in the log window: label -> categories ("" = all)
var logFilters = []struct {
	label string
	cats  []string
}{
	{"全部", nil},
	{"连接 / 断开 / 重连", []string{"连接", "断开", "重连"}},
	{"传输", []string{"传输", "重试"}},
	{"文件名截断", []string{"截断"}},
	{"错误", []string{"错误"}},
}

type logWindow struct {
	dlg    *walk.Dialog
	te     *walk.TextEdit
	filter *walk.ComboBox
	count  int
}

func (lw *logWindow) match(line string) bool {
	i := lw.filter.CurrentIndex()
	if i <= 0 || i >= len(logFilters) {
		return true
	}
	c := xlog.Category(line)
	for _, x := range logFilters[i].cats {
		if c == x {
			return true
		}
	}
	return false
}

func (lw *logWindow) reload(lines []string) {
	var b strings.Builder
	n := 0
	for _, l := range lines {
		if lw.match(l) {
			b.WriteString(l)
			b.WriteString("\r\n")
			n++
		}
	}
	lw.count = n
	lw.te.SetText(b.String())
	lw.te.SetTextSelection(len(lw.te.Text()), len(lw.te.Text()))
	lw.te.ScrollToCaret()
}

func (lw *logWindow) add(line string) {
	if !lw.match(line) {
		return
	}
	lw.count++
	if lw.count > 3000 { // keep the edit control fast
		lw.reload(nil)
	}
	lw.te.AppendText(line + "\r\n")
}

// showLog opens (or raises) the log window. It is modeless and stays updated.
func (a *app) showLog() {
	if a.logWin != nil {
		a.logWin.dlg.Show()
		win32ShowRestore(a.logWin.dlg.Handle())
		return
	}
	lw := &logWindow{}
	var names []string
	for _, f := range logFilters {
		names = append(names, f.label)
	}
	err := Dialog{
		AssignTo: &lw.dlg,
		Title:    "LanXfer 日志",
		MinSize:  Size{Width: 820, Height: 480},
		Layout:   VBox{},
		Children: []Widget{
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					Label{Text: "显示:"},
					ComboBox{AssignTo: &lw.filter, Model: names, CurrentIndex: 0, OnCurrentIndexChanged: func() {
						if lw.te != nil {
							lw.reload(a.log.Lines())
						}
					}},
					HSpacer{},
					PushButton{Text: "复制全部", OnClicked: func() { walk.Clipboard().SetText(lw.te.Text()) }},
					PushButton{Text: "打开日志文件夹", OnClicked: func() {
						exec.Command("explorer.exe", "/select,", a.log.Path()).Start()
					}},
				},
			},
			TextEdit{AssignTo: &lw.te, ReadOnly: true, VScroll: true, HScroll: true, Font: Font{Family: "Consolas", PointSize: 9}},
		},
	}.Create(a.mw)
	if err != nil {
		walk.MsgBox(a.mw, "LanXfer", err.Error(), walk.MsgBoxIconError)
		return
	}
	lw.dlg.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		if !a.quitting {
			*canceled = true
			lw.dlg.Hide()
		}
	})
	a.logWin = lw
	lw.reload(a.log.Lines())
	a.log.Subscribe(func(line string) {
		a.mw.Synchronize(func() { lw.add(line) })
	})
	lw.dlg.Show()
}
