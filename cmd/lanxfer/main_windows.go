//go:build windows

// LanXfer: a phone file manager for Windows. One pane shows the phone's
// files; drag files in from Explorer to upload, drag them out to download.
// Lives in the tray, global hotkey to show/hide, optional autostart.
package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"

	"lanxfer/client"
	"lanxfer/proto"
	"lanxfer/xlog"
)

// ---------- table model ----------

type fileModel struct {
	walk.TableModelBase
	items []proto.Entry
}

func (m *fileModel) RowCount() int { return len(m.items) }
func (m *fileModel) Value(row, col int) interface{} {
	e := m.items[row]
	switch col {
	case 0:
		if e.Dir {
			return "📁 " + e.Name
		}
		return "📄 " + e.Name
	case 1:
		if e.Dir {
			return ""
		}
		return humanSize(e.Size)
	case 2:
		return time.UnixMilli(e.MTime).Format("2006-01-02 15:04")
	}
	return ""
}

func humanSize(n int64) string {
	f := float64(n)
	for _, u := range []string{"B", "KB", "MB", "GB"} {
		if f < 1024 {
			if u == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1024
	}
	return fmt.Sprintf("%.2f TB", f)
}

// ---------- app ----------

type app struct {
	cfg *Config
	mw  *walk.MainWindow
	ni  *walk.NotifyIcon
	hk  hotkeyMgr

	devCombo *walk.ComboBox
	pathEdit *walk.LineEdit
	tv       *walk.TableView
	model    *fileModel
	bar      *walk.ProgressBar
	status   *walk.Label
	cancelBt *walk.PushButton
	connLbl  *walk.Label

	c         *client.Client
	cwd       string
	found     []client.Found
	job       *client.Job
	drag      *dragProgress
	preparing bool // a download is being planned (walking folders)
	jobVerb   string
	lastHkErr string
	quitting  bool
	autoAct   *walk.Action

	log    *xlog.Logger
	logWin *logWindow
	gen    atomic.Int64 // bumped by every connect; stops the old monitor
}

func main() {
	if !ensureSingle() {
		return
	}
	startHidden := len(os.Args) > 1 && os.Args[1] == "--tray"
	a := &app{cfg: loadConfig(), model: &fileModel{}, cwd: "/sdcard"}
	a.initLog()
	defer a.log.Close()
	if a.cfg.LastPath != "" {
		a.cwd = a.cfg.LastPath
	}
	if err := a.build(); err != nil {
		walk.MsgBox(nil, "LanXfer", err.Error(), walk.MsgBoxIconError)
		return
	}
	if !startHidden {
		a.mw.Show()
	}
	go a.autoConnect()
	a.mw.Run()
	a.log.Log("启动", "LanXfer 退出")
	a.ni.Dispose()
}

func (a *app) build() error {
	err := MainWindow{
		AssignTo: &a.mw,
		Title:    windowTitle,
		Size:     Size{Width: 860, Height: 600},
		Visible:  false,
		Layout:   VBox{MarginsZero: false},
		OnDropFiles: func(files []string) {
			a.upload(files)
		},
		Children: []Widget{
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					Label{Text: "手机:"},
					ComboBox{AssignTo: &a.devCombo, Editable: true, MinSize: Size{Width: 260}},
					PushButton{Text: "搜索", OnClicked: func() { go a.discover() }},
					PushButton{Text: "连接", OnClicked: func() { go a.connect(a.hostFromCombo()) }},
					Label{AssignTo: &a.connLbl, Text: "未连接"},
					HSpacer{},
					PushButton{Text: "日志", OnClicked: a.showLog},
					PushButton{Text: "设置", OnClicked: a.settings},
				},
			},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					PushButton{Text: "↑ 上级", MaxSize: Size{Width: 70}, OnClicked: func() { a.navigate(path.Dir(a.cwd)) }},
					LineEdit{AssignTo: &a.pathEdit, OnKeyDown: func(k walk.Key) {
						if k == walk.KeyReturn {
							a.navigate(a.pathEdit.Text())
						}
					}},
					PushButton{Text: "刷新", OnClicked: func() { a.navigate(a.cwd) }},
					PushButton{Text: "新建文件夹", OnClicked: a.newFolder},
					PushButton{Text: "重命名", OnClicked: a.rename},
					PushButton{Text: "删除", OnClicked: a.delete},
					PushButton{Text: "下载到…", OnClicked: a.downloadTo},
				},
			},
			TableView{
				AssignTo:         &a.tv,
				AlternatingRowBG: true,
				MultiSelection:   true,
				Columns: []TableViewColumn{
					{Title: "名称", Width: 460},
					{Title: "大小", Width: 110, Alignment: AlignFar},
					{Title: "修改时间", Width: 150},
				},
				Model:           a.model,
				OnItemActivated: a.activated,
				OnKeyDown: func(k walk.Key) {
					switch k {
					case walk.KeyDelete:
						a.delete()
					case walk.KeyBack:
						a.navigate(path.Dir(a.cwd))
					case walk.KeyF5:
						a.navigate(a.cwd)
					case walk.KeyF2:
						a.rename()
					}
				},
			},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					ProgressBar{AssignTo: &a.bar, MaxValue: 1000, MinSize: Size{Width: 260}},
					Label{AssignTo: &a.status, Text: "从资源管理器拖文件进来即可上传；把文件拖出去即可下载"},
					HSpacer{},
					PushButton{AssignTo: &a.cancelBt, Text: "取消传输", Enabled: false, OnClicked: func() {
						if a.job != nil {
							a.job.Cancel()
							a.status.SetText("正在取消…")
							a.log.Log("传输", "用户取消传输")
						}
					}},
				},
			},
		},
	}.Create()
	if err != nil {
		return err
	}
	a.pathEdit.SetText(a.cwd)

	icon, err := walk.NewIconFromImageForDPI(appImage(), 96)
	if err == nil {
		a.mw.SetIcon(icon)
	}

	// close button hides to tray
	a.mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		if !a.quitting && reason == walk.CloseReasonUser {
			*canceled = true
			a.mw.Hide()
		}
	})

	if err := a.buildTray(icon); err != nil {
		return err
	}

	// global hotkey
	a.hk.onPress = func() { a.mw.Synchronize(a.toggleWindow) }
	a.hk.onResult = func(ok bool) {
		if !ok {
			if k := hotkeyText(a.cfg.HotMods, a.cfg.HotKey); k != a.lastHkErr {
				a.lastHkErr = k
				a.log.Logf("错误", "全局快捷键 %s 注册失败（被占用，Ctrl+Alt+A 常被 QQ/TIM 截图占用）", k)
			}
			a.mw.Synchronize(func() {
				a.ni.ShowWarning("LanXfer", "快捷键 "+hotkeyText(a.cfg.HotMods, a.cfg.HotKey)+" 被占用，请在设置里换一个")
			})
		}
	}
	a.hk.start()
	a.hk.set(a.cfg.HotMods, a.cfg.HotKey)

	// drag-out: catch LVN_BEGINDRAG sent by the list view to the TableView
	hookBeginDrag(a.tv, a.beginDrag)

	go a.progressLoop()
	return nil
}

func (a *app) buildTray(icon *walk.Icon) error {
	ni, err := walk.NewNotifyIcon(a.mw)
	if err != nil {
		return err
	}
	a.ni = ni
	if icon != nil {
		ni.SetIcon(icon)
	}
	ni.SetToolTip("LanXfer 手机文件")
	ni.MouseUp().Attach(func(x, y int, b walk.MouseButton) {
		if b == walk.LeftButton {
			a.toggleWindow()
		}
	})
	show := walk.NewAction()
	show.SetText("显示主窗口")
	show.Triggered().Attach(a.showWindow)
	auto := walk.NewAction()
	auto.SetText("开机启动")
	auto.SetCheckable(true)
	auto.SetChecked(a.cfg.Autostart)
	auto.Triggered().Attach(func() {
		a.cfg.Autostart = !a.cfg.Autostart
		if err := setAutostart(a.cfg.Autostart); err != nil {
			a.cfg.Autostart = !a.cfg.Autostart
			walk.MsgBox(a.mw, "LanXfer", "设置开机启动失败："+err.Error(), walk.MsgBoxIconError)
		}
		auto.SetChecked(a.cfg.Autostart)
		a.cfg.save()
	})
	a.autoAct = auto
	logAct := walk.NewAction()
	logAct.SetText("日志…")
	logAct.Triggered().Attach(func() { a.showWindow(); a.showLog() })
	settings := walk.NewAction()
	settings.SetText("设置…")
	settings.Triggered().Attach(func() { a.showWindow(); a.settings() })
	quit := walk.NewAction()
	quit.SetText("退出")
	quit.Triggered().Attach(func() {
		a.quitting = true
		a.cfg.LastPath = a.cwd
		a.cfg.save()
		a.mw.Close()
	})
	acts := ni.ContextMenu().Actions()
	acts.Add(show)
	acts.Add(auto)
	acts.Add(logAct)
	acts.Add(settings)
	acts.Add(walk.NewSeparatorAction())
	acts.Add(quit)
	return ni.SetVisible(true)
}

func (a *app) showWindow() {
	a.mw.Show()
	win32ShowRestore(a.mw.Handle())
}

func (a *app) toggleWindow() {
	if a.mw.Visible() && isForeground(a.mw.Handle()) {
		a.mw.Hide()
		return
	}
	a.showWindow()
}

// ---------- connection ----------

func (a *app) hostFromCombo() string {
	t := strings.TrimSpace(a.devCombo.Text())
	// combo items look like "Name  ip:port"
	if i := strings.LastIndex(t, " "); i >= 0 {
		t = t[i+1:]
	}
	return t
}

func (a *app) setStatus(s string) { a.mw.Synchronize(func() { a.status.SetText(s) }) }

func (a *app) autoConnect() {
	if a.cfg.Host != "" {
		a.mw.Synchronize(func() { a.devCombo.SetText(a.cfg.Host) })
		if a.connect(a.cfg.Host) == nil {
			return
		}
	}
	a.discover()
}

func (a *app) discover() {
	a.setStatus("正在局域网中搜索手机…")
	found := client.Discover(1500 * time.Millisecond)
	if len(found) == 0 {
		a.log.Log("连接", "局域网搜索：未发现手机")
	}
	for _, f := range found {
		a.log.Logf("连接", "局域网搜索：发现 %s", f.String())
	}
	a.mw.Synchronize(func() {
		a.found = found
		var items []string
		for _, f := range found {
			items = append(items, f.String())
		}
		a.devCombo.SetModel(items)
		if len(items) > 0 {
			a.devCombo.SetCurrentIndex(0)
			a.status.SetText(fmt.Sprintf("找到 %d 台手机", len(items)))
		} else {
			a.status.SetText("没找到手机：确认模块服务已开启、手机与电脑在同一局域网，或手动输入手机 IP")
		}
	})
	if len(found) == 1 && a.c == nil {
		a.connect(found[0].Addr)
	}
}

func (a *app) connect(host string) error {
	if host == "" {
		a.setStatus("请先搜索或输入手机 IP")
		return errors.New("no host")
	}
	if a.cfg.Token == "" {
		a.mw.Synchronize(func() {
			walk.MsgBox(a.mw, "LanXfer", "请先在「设置」里填入令牌。\n令牌在 Magisk 里点本模块的「操作」按钮即可看到。", walk.MsgBoxIconInformation)
		})
		return errors.New("no token")
	}
	gen := a.gen.Add(1)
	a.setStatus("正在连接 " + host + " …")
	a.log.Logf("连接", "正在连接 %s（%d 条并行连接）", host, a.cfg.Conns)
	t0 := time.Now()
	c, err := client.Connect(host, a.cfg.Token, a.cfg.Conns)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, client.ErrBadToken) {
			msg = "令牌错误，请在设置里重新填写"
		}
		a.log.Logf("错误", "连接 %s 失败：%s", host, msg)
		a.setStatus("连接失败：" + msg)
		a.mw.Synchronize(func() { a.connLbl.SetText("未连接") })
		return err
	}
	a.log.Logf("连接", "已连接 %s（%s），手机端 v%s，%d 条通道 %v，直通存储=%v，耗时 %dms",
		c.Info.Name, c.Host(), c.Info.Version, len(c.Bases), c.Bases, c.Info.Raw, time.Since(t0).Milliseconds())
	a.adopt(c, true)
	go a.monitor(gen, c)
	return nil
}

// adopt makes c the current connection and refreshes the window.
func (a *app) adopt(c *client.Client, navigate bool) {
	a.mw.Synchronize(func() {
		a.c = c
		a.cfg.Host = c.Host()
		a.cfg.save()
		mode := ""
		if c.Info.Raw {
			mode = "，直通存储"
		}
		a.connLbl.SetText(fmt.Sprintf("已连接 %s（%d 条通道%s）", c.Info.Name, len(c.Bases), mode))
		a.ni.SetToolTip("LanXfer - " + c.Info.Name)
		if navigate {
			a.navigate(a.cwd)
		}
	})
}

// monitor pings the phone every few seconds. On loss it logs the drop and
// keeps reconnecting (same IP first, then by device name via discovery in
// case the phone got a new IP) until it is back or the user connects again.
func (a *app) monitor(gen int64, c *client.Client) {
	connAt := time.Now()
	fails := 0
	for {
		time.Sleep(3 * time.Second)
		if a.gen.Load() != gen {
			return
		}
		err := c.Ping(4 * time.Second)
		if err == nil {
			fails = 0
			continue
		}
		if errors.Is(err, client.ErrBadToken) {
			a.log.Log("错误", "手机端令牌已更改，请在设置里重新填写令牌")
			a.mw.Synchronize(func() { a.connLbl.SetText("令牌已失效") })
			return
		}
		fails++
		if fails < 2 {
			continue
		}
		lost := time.Now()
		a.log.Logf("断开", "与 %s（%s）的连接中断，本次连接持续 %s，原因：%v", c.Info.Name, c.Host(), xlog.Dur(lost.Sub(connAt)), err)
		a.mw.Synchronize(func() {
			a.connLbl.SetText("连接中断，正在重连…")
			a.ni.SetToolTip("LanXfer - 连接中断")
		})
		nc, tries := a.reconnect(gen, c)
		if nc == nil {
			return
		}
		a.log.Logf("重连", "重连成功：%s（%s），断开 %s，尝试 %d 次，%d 条通道",
			nc.Info.Name, nc.Host(), xlog.Dur(time.Since(lost)), tries, len(nc.Bases))
		a.adopt(nc, a.job == nil && a.drag == nil)
		c, connAt, fails = nc, time.Now(), 0
	}
}

func (a *app) reconnect(gen int64, old *client.Client) (*client.Client, int) {
	delays := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 5 * time.Second, 8 * time.Second, 10 * time.Second}
	for try := 1; ; try++ {
		d := 15 * time.Second
		if try-1 < len(delays) {
			d = delays[try-1]
		}
		time.Sleep(d)
		if a.gen.Load() != gen {
			return nil, try
		}
		hosts := []string{old.Host()}
		if try%3 == 0 { // the phone may have a new IP: look it up by name
			for _, f := range client.Discover(1200 * time.Millisecond) {
				if f.Name == old.Info.Name && f.Addr != old.Host() {
					a.log.Logf("重连", "手机 IP 可能已变化，发现 %s", f.String())
					hosts = append(hosts, f.Addr)
				}
			}
		}
		var lastErr error
		for _, h := range hosts {
			nc, err := client.Connect(h, a.cfg.Token, a.cfg.Conns)
			if err == nil {
				if a.gen.Load() != gen {
					return nil, try
				}
				return nc, try
			}
			lastErr = err
			if errors.Is(err, client.ErrBadToken) {
				a.log.Log("错误", "重连失败：令牌错误，已停止重连")
				a.mw.Synchronize(func() { a.connLbl.SetText("令牌已失效") })
				return nil, try
			}
		}
		if try <= 5 || try%10 == 0 {
			a.log.Logf("重连", "第 %d 次重连失败：%v", try, lastErr)
		}
		a.mw.Synchronize(func() { a.connLbl.SetText(fmt.Sprintf("连接中断，正在重连（第 %d 次）…", try)) })
	}
}

func (a *app) needConn() bool {
	if a.c == nil {
		walk.MsgBox(a.mw, "LanXfer", "还没有连接手机", walk.MsgBoxIconInformation)
		return false
	}
	return true
}

// ---------- browsing ----------

func (a *app) navigate(p string) {
	if a.c == nil {
		return
	}
	p = path.Clean("/" + strings.ReplaceAll(strings.TrimSpace(p), "\\", "/"))
	c := a.c
	go func() {
		items, err := c.List(p)
		a.mw.Synchronize(func() {
			if err != nil {
				a.status.SetText("打开失败：" + err.Error())
				a.pathEdit.SetText(a.cwd)
				return
			}
			a.cwd = p
			a.pathEdit.SetText(p)
			a.model.items = items
			a.model.PublishRowsReset()
			a.status.SetText(fmt.Sprintf("%d 项", len(items)))
		})
	}()
}

func (a *app) selected() []proto.Entry {
	var out []proto.Entry
	for _, i := range a.tv.SelectedIndexes() {
		if i >= 0 && i < len(a.model.items) {
			out = append(out, a.model.items[i])
		}
	}
	return out
}

func (a *app) activated() {
	i := a.tv.CurrentIndex()
	if i < 0 || i >= len(a.model.items) {
		return
	}
	e := a.model.items[i]
	if e.Dir {
		a.navigate(path.Join(a.cwd, e.Name))
	}
}

func (a *app) newFolder() {
	if !a.needConn() {
		return
	}
	name, ok := inputBox(a.mw, "新建文件夹", "文件夹名：", "新建文件夹")
	if !ok || name == "" {
		return
	}
	name = a.phoneNameCheck(name)
	a.remoteOp("新建文件夹", func(c *client.Client, cwd string) error {
		return c.Mkdir(path.Join(cwd, name))
	})
}

func (a *app) rename() {
	if !a.needConn() {
		return
	}
	sel := a.selected()
	if len(sel) != 1 {
		return
	}
	name, ok := inputBox(a.mw, "重命名", "新名称：", sel[0].Name)
	if !ok || name == "" || name == sel[0].Name {
		return
	}
	name = a.phoneNameCheck(name)
	old := sel[0].Name
	a.remoteOp("重命名", func(c *client.Client, cwd string) error {
		return c.Rename(path.Join(cwd, old), path.Join(cwd, name))
	})
}

func (a *app) delete() {
	if !a.needConn() {
		return
	}
	sel := a.selected()
	if len(sel) == 0 {
		return
	}
	msg := fmt.Sprintf("确定删除选中的 %d 项？此操作不可恢复。", len(sel))
	if len(sel) == 1 {
		msg = "确定删除「" + sel[0].Name + "」？此操作不可恢复。"
	}
	if walk.MsgBox(a.mw, "删除", msg, walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
		return
	}
	a.remoteOp("删除", func(c *client.Client, cwd string) error {
		for i, e := range sel {
			a.setStatus(fmt.Sprintf("正在删除 %d/%d：%s", i+1, len(sel), e.Name))
			if err := c.Delete(path.Join(cwd, e.Name)); err != nil {
				return fmt.Errorf("%s：%w", e.Name, err)
			}
		}
		return nil
	})
}

// phoneNameCheck shortens a typed name that is too long for the phone.
func (a *app) phoneNameCheck(name string) string {
	n, cut := client.PhoneName(name)
	if cut {
		a.log.Logf("截断", "名称超过手机 %d 字节上限，「%s」→「%s」", client.PhoneNameMax, name, n)
		walk.MsgBox(a.mw, "LanXfer", "名称太长，手机文件系统最多 255 字节（约 85 个汉字），已截短为：\n"+n, walk.MsgBoxIconInformation)
	}
	return n
}

// remoteOp runs a phone-side operation off the UI thread so the window never
// freezes on a slow network, then refreshes the listing.
func (a *app) remoteOp(verb string, fn func(c *client.Client, cwd string) error) {
	c, cwd := a.c, a.cwd
	a.status.SetText("正在" + verb + "…")
	go func() {
		err := fn(c, cwd)
		a.mw.Synchronize(func() {
			if err != nil {
				a.log.Logf("错误", "%s失败：%v", verb, err)
				a.status.SetText(verb + "失败：" + err.Error())
				walk.MsgBox(a.mw, "LanXfer", verb+"失败："+err.Error(), walk.MsgBoxIconError)
			}
			a.navigate(a.cwd)
		})
	}()
}

// ---------- upload (drop in from Explorer) ----------

func (a *app) upload(files []string) {
	if !a.needConn() || len(files) == 0 {
		return
	}
	if a.busy() {
		walk.MsgBox(a.mw, "LanXfer", "正在传输，请稍候", walk.MsgBoxIconInformation)
		return
	}
	c, dst := a.c, a.cwd
	a.status.SetText("正在准备上传…")
	go func() {
		j, err := c.PlanUpload(files, dst)
		if err != nil {
			a.log.Logf("错误", "上传准备失败：%v", err)
			a.setStatus("上传失败：" + err.Error())
			return
		}
		a.log.Logf("传输", "开始上传 %d 个文件（%s）到 %s", j.Progress.TotalFiles, humanSize(j.Progress.TotalBytes), dst)
		a.runJob(j, "上传")
	}()
}

func (a *app) runJob(j *client.Job, verb string) {
	a.mw.Synchronize(func() {
		a.job = j
		a.jobVerb = verb
		a.cancelBt.SetEnabled(true)
	})
	err := j.Run(a.cfg.Conns)
	p := j.Progress
	sec := time.Since(p.Started).Seconds()
	if speed := float64(p.DoneBytes.Load()) / 1e6 / max(sec, 0.001); err != nil {
		a.log.Logf("传输", "%s未完成：成功 %d/%d 个文件，%s，用时 %s，%.1f MB/s；%v", verb, p.DoneFiles.Load(), p.TotalFiles, humanSize(p.DoneBytes.Load()), xlog.Dur(time.Since(p.Started)), speed, err)
	} else {
		a.log.Logf("传输", "%s完成：%d 个文件，%s，用时 %s，平均 %.1f MB/s", verb, p.DoneFiles.Load(), humanSize(p.DoneBytes.Load()), xlog.Dur(time.Since(p.Started)), speed)
	}
	a.mw.Synchronize(func() {
		a.job = nil
		a.cancelBt.SetEnabled(false)
		a.bar.SetValue(0)
		speed := float64(p.DoneBytes.Load()) / 1e6 / max(sec, 0.001)
		if err != nil {
			a.status.SetText(verb + "未完成：" + err.Error())
		} else {
			a.status.SetText(fmt.Sprintf("%s完成：%d 个文件，%s，平均 %.1f MB/s", verb, p.DoneFiles.Load(), humanSize(p.DoneBytes.Load()), speed))
			if !a.mw.Visible() {
				a.ni.ShowInfo("LanXfer", fmt.Sprintf("%s完成：%d 个文件", verb, p.DoneFiles.Load()))
			}
		}
		a.navigate(a.cwd)
	})
}

// ---------- download (drag out to Explorer) ----------

func (a *app) beginDrag(idx []int) {
	if a.c == nil {
		return
	}
	var sel []proto.Entry
	hasDir := false
	for _, i := range idx {
		if i >= 0 && i < len(a.model.items) {
			sel = append(sel, a.model.items[i])
			hasDir = hasDir || a.model.items[i].Dir
		}
	}
	if len(sel) == 0 {
		return
	}
	if a.busy() {
		a.status.SetText("正在传输，请等当前任务结束或先取消")
		return
	}
	// Only the selected items go into the drag data: the folder tree is
	// walked after the drop, so a drag starts instantly even for 80k files.
	var items []dragItem
	var total int64
	namer := client.NewDragNamer()
	for _, e := range sel {
		items = append(items, dragItem{Name: namer.Name(e.Name, e.Dir), Remote: path.Join(a.cwd, e.Name), Size: e.Size, MTime: e.MTime, Dir: e.Dir})
		total += e.Size
	}
	var spot dropSpot
	decide := func() bool {
		spot = spotUnderCursor(uintptr(a.mw.Handle()))
		switch spot.kind {
		case spotExplorer, spotDesktop, spotSelf:
			return false // we copy ourselves (or it is our own window: nothing to do)
		}
		// some other application: virtual files work for plain files only
		return !hasDir
	}
	prog := &dragProgress{start: time.Now()}
	prog.total.Store(total)
	a.drag = prog
	delivered := startDragOut(a.c, items, a.cfg.Conns, prog, decide)
	if delivered {
		// another app pulled the files through OLE; DoDragDrop returns when it is done or cancelled
		a.log.Logf("传输", "拖出到其他程序：%d 项，已传 %s / %s", len(items), humanSize(prog.done.Load()), humanSize(total))
		a.drag = nil
		a.bar.SetValue(0)
		a.status.SetText(fmt.Sprintf("已交给目标程序：%s", humanSize(prog.done.Load())))
		return
	}
	a.drag = nil
	switch spot.kind {
	case spotSelf:
		return
	case spotNone:
		if hasDir {
			a.status.SetText("文件夹请拖到资源管理器窗口的文件列表或桌面上")
		}
		return
	}
	dir, err := folderOf(spot)
	if err != nil {
		a.log.Logf("错误", "无法识别拖放目标文件夹：%v", err)
		a.status.SetText("无法识别目标文件夹：" + err.Error())
		return
	}
	a.download(sel, a.cwd, dir)
}

// busy reports whether a transfer is preparing or running.
func (a *app) busy() bool { return a.job != nil || a.preparing }

// download copies phone entries into a local folder with the parallel engine.
func (a *app) download(sel []proto.Entry, srcDir, dst string) {
	c := a.c
	a.preparing = true
	a.cancelBt.SetEnabled(true)
	a.status.SetText("正在读取文件列表…")
	a.log.Logf("传输", "下载 %d 项：%s → %s", len(sel), srcDir, dst)
	go func() {
		j, err := c.PlanDownload(srcDir, sel, dst)
		a.mw.Synchronize(func() { a.preparing = false })
		if err != nil {
			a.log.Logf("错误", "下载准备失败：%v", err)
			a.setStatus("下载失败：" + err.Error())
			a.mw.Synchronize(func() { a.cancelBt.SetEnabled(false) })
			return
		}
		p := j.Progress
		if p.Skipped > 0 {
			a.log.Logf("传输", "跳过 %d 个目标处已存在且大小、时间相同的文件", p.Skipped)
		}
		a.log.Logf("传输", "开始下载 %d 个文件（%s）到 %s", p.TotalFiles, humanSize(p.TotalBytes), dst)
		a.runJob(j, "下载")
	}()
}

// downloadTo asks for a folder and downloads the selection there.
func (a *app) downloadTo() {
	if !a.needConn() {
		return
	}
	var sel []proto.Entry
	for _, i := range a.tv.SelectedIndexes() {
		if i >= 0 && i < len(a.model.items) {
			sel = append(sel, a.model.items[i])
		}
	}
	if len(sel) == 0 {
		a.status.SetText("先选中要下载的文件或文件夹")
		return
	}
	if a.busy() {
		a.status.SetText("正在传输，请等当前任务结束或先取消")
		return
	}
	dlg := &walk.FileDialog{Title: "下载到文件夹", InitialDirPath: a.cfg.LastLocal}
	if ok, err := dlg.ShowBrowseFolder(a.mw); err != nil || !ok {
		return
	}
	a.cfg.LastLocal = dlg.FilePath
	a.cfg.save()
	a.download(sel, a.cwd, dlg.FilePath)
}

// progressLoop refreshes the progress bar for whichever transfer is running.
func (a *app) progressLoop() {
	t := time.NewTicker(500 * time.Millisecond)
	var lastBytes int64
	lastT := time.Now()
	for range t.C {
		a.mw.Synchronize(func() {
			now := time.Now()
			if j := a.job; j != nil {
				p := j.Progress
				done := p.DoneBytes.Load()
				speed := float64(done-lastBytes) / now.Sub(lastT).Seconds() / 1e6
				lastBytes, lastT = done, now
				if p.TotalBytes > 0 {
					a.bar.SetValue(int(done * 1000 / p.TotalBytes))
				}
				a.status.SetText(fmt.Sprintf(a.jobVerb+"中 %d/%d 个文件  %s / %s  %.1f MB/s", p.DoneFiles.Load(), p.TotalFiles, humanSize(done), humanSize(p.TotalBytes), max(speed, 0)))
				return
			}
			if d := a.drag; d != nil {
				done, total := d.done.Load(), d.total.Load()
				speed := float64(done-lastBytes) / now.Sub(lastT).Seconds() / 1e6
				lastBytes, lastT = done, now
				if total > 0 {
					a.bar.SetValue(int(done * 1000 / total))
				}
				if done >= total && total > 0 {
					a.status.SetText(fmt.Sprintf("下载完成：%s，平均 %.1f MB/s", humanSize(total), float64(total)/1e6/max(now.Sub(d.start).Seconds(), 0.001)))
					a.bar.SetValue(0)
					a.drag = nil
				} else if d.files.Load() > 0 {
					a.status.SetText(fmt.Sprintf("下载中 %s / %s  %.1f MB/s", humanSize(done), humanSize(total), max(speed, 0)))
				}
				return
			}
			lastBytes, lastT = 0, now
		})
	}
}

// ---------- dialogs ----------

func inputBox(owner walk.Form, title, label, initial string) (string, bool) {
	var dlg *walk.Dialog
	var le *walk.LineEdit
	var ok, cancel *walk.PushButton
	var text string
	res, _ := Dialog{
		AssignTo:      &dlg,
		Title:         title,
		DefaultButton: &ok,
		CancelButton:  &cancel,
		MinSize:       Size{Width: 360},
		Layout:        VBox{},
		Children: []Widget{
			Label{Text: label},
			LineEdit{AssignTo: &le, Text: initial},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				HSpacer{},
				PushButton{AssignTo: &ok, Text: "确定", OnClicked: func() {
					// read before Accept: the dialog's widgets are destroyed once it closes
					text = strings.TrimSpace(le.Text())
					dlg.Accept()
				}},
				PushButton{AssignTo: &cancel, Text: "取消", OnClicked: func() { dlg.Cancel() }},
			}},
		},
	}.Run(owner)
	return text, res == walk.DlgCmdOK
}

func (a *app) settings() {
	var dlg *walk.Dialog
	var tokEd *walk.LineEdit
	var connEd *walk.NumberEdit
	var cCtrl, cAlt, cShift, cWin, cAuto *walk.CheckBox
	var keyCb *walk.ComboBox
	var ok, cancel *walk.PushButton
	var v struct {
		token                 string
		conns, key            int
		ctrl, alt, shift, win bool
		auto                  bool
	}
	names, vks := keyChoices()
	keyIdx := 0
	for i, v := range vks {
		if v == a.cfg.HotKey {
			keyIdx = i
		}
	}
	m := a.cfg.HotMods
	res, _ := Dialog{
		AssignTo:      &dlg,
		Title:         "设置",
		DefaultButton: &ok,
		CancelButton:  &cancel,
		MinSize:       Size{Width: 420},
		Layout:        Grid{Columns: 2},
		Children: []Widget{
			Label{Text: "令牌："},
			LineEdit{AssignTo: &tokEd, Text: a.cfg.Token, ToolTipText: "在 Magisk 中点本模块的「操作」可看到"},
			Label{Text: "并行连接数："},
			NumberEdit{AssignTo: &connEd, Value: float64(a.cfg.Conns), MinValue: 1, MaxValue: 32, Decimals: 0},
			Label{Text: "呼出快捷键："},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				CheckBox{AssignTo: &cCtrl, Text: "Ctrl", Checked: m&MOD_CONTROL != 0},
				CheckBox{AssignTo: &cAlt, Text: "Alt", Checked: m&MOD_ALT != 0},
				CheckBox{AssignTo: &cShift, Text: "Shift", Checked: m&MOD_SHIFT != 0},
				CheckBox{AssignTo: &cWin, Text: "Win", Checked: m&MOD_WIN != 0},
				ComboBox{AssignTo: &keyCb, Model: names, CurrentIndex: keyIdx},
			}},
			Label{Text: "开机启动："},
			CheckBox{AssignTo: &cAuto, Text: "登录 Windows 后自动在托盘运行", Checked: a.cfg.Autostart},
			HSpacer{},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				HSpacer{},
				PushButton{AssignTo: &ok, Text: "保存", OnClicked: func() {
					// read every value before Accept: the dialog's widgets are destroyed once it closes
					v.token = strings.TrimSpace(tokEd.Text())
					v.conns = int(connEd.Value())
					v.ctrl, v.alt, v.shift, v.win = cCtrl.Checked(), cAlt.Checked(), cShift.Checked(), cWin.Checked()
					v.key = keyCb.CurrentIndex()
					v.auto = cAuto.Checked()
					if v.token == "" {
						walk.MsgBox(dlg, "设置", "令牌不能为空。\n在 Magisk 里点本模块的「操作」即可看到令牌。", walk.MsgBoxIconWarning)
						return
					}
					dlg.Accept()
				}},
				PushButton{AssignTo: &cancel, Text: "取消", OnClicked: func() { dlg.Cancel() }},
			}},
		},
	}.Run(a.mw)
	if res != walk.DlgCmdOK {
		return
	}
	oldTok := a.cfg.Token
	a.cfg.Token = v.token
	if v.conns >= 1 {
		a.cfg.Conns = v.conns
	}
	var mods uint32
	if v.ctrl {
		mods |= MOD_CONTROL
	}
	if v.alt {
		mods |= MOD_ALT
	}
	if v.shift {
		mods |= MOD_SHIFT
	}
	if v.win {
		mods |= MOD_WIN
	}
	if v.key >= 0 && v.key < len(vks) {
		a.cfg.HotKey = vks[v.key]
	}
	if mods == 0 {
		mods = MOD_CONTROL | MOD_ALT
	}
	a.cfg.HotMods = mods
	a.hk.set(a.cfg.HotMods, a.cfg.HotKey)
	if v.auto != a.cfg.Autostart {
		if err := setAutostart(v.auto); err != nil {
			walk.MsgBox(a.mw, "LanXfer", "设置开机启动失败："+err.Error(), walk.MsgBoxIconError)
		} else {
			a.cfg.Autostart = v.auto
			a.autoAct.SetChecked(a.cfg.Autostart)
		}
	}
	if err := a.cfg.save(); err != nil {
		walk.MsgBox(a.mw, "LanXfer", "保存设置失败："+err.Error(), walk.MsgBoxIconError)
	} else {
		a.status.SetText("设置已保存")
	}
	if a.cfg.Token != oldTok || a.c == nil {
		go a.connect(a.hostFromCombo())
	}
}

// ---------- small win32 helpers ----------

var procGetForegroundWindow = user32.NewProc("GetForegroundWindow")

func isForeground(h win.HWND) bool {
	r, _, _ := procGetForegroundWindow.Call()
	return r == uintptr(h)
}

func win32ShowRestore(h win.HWND) {
	procShowWindow.Call(uintptr(h), 9) // SW_RESTORE
	procSetForegroundWindow.Call(uintptr(h))
}

// hookBeginDrag subclasses the TableView window (parent of its list views)
// and calls fn when a list view reports LVN_BEGINDRAG.
func hookBeginDrag(tv *walk.TableView, fn func(sel []int)) {
	cb := syscall.NewCallback(func(hwnd, msg, wp, lp, id, ref uintptr) uintptr {
		const WM_NOTIFY = 0x004E
		const LVN_BEGINDRAG = ^uint32(109 - 1) // LVN_FIRST(-100) - 9 = -109
		if msg == WM_NOTIFY && lp != 0 {
			code := *(*uint32)(unsafe.Pointer(lp + 16)) // NMHDR.code (after hwndFrom, idFrom)
			if code == LVN_BEGINDRAG {
				from := *(*uintptr)(unsafe.Pointer(lp))
				var sel []int
				idx := -1
				for {
					r := win.SendMessage(win.HWND(from), win.LVM_GETNEXTITEM, uintptr(idx), win.LVNI_SELECTED)
					if int32(r) < 0 {
						break
					}
					idx = int(int32(r))
					sel = append(sel, idx)
				}
				fn(sel)
				return 0
			}
		}
		r, _, _ := procDefSubclassProc.Call(hwnd, msg, wp, lp)
		return r
	})
	procSetWindowSubclass.Call(uintptr(tv.Handle()), cb, 0x4C58, 0)
}
