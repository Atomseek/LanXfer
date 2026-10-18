// lanxferd is the phone-side daemon. It runs as root (started by the Magisk
// module), serves the file system over a small HTTP API, and answers UDP
// discovery probes so the PC can find it on the LAN.
//
// Speed ideas borrowed from HybridFileXfer:
//   - the PC opens many TCP connections at once and moves files in parallel
//     chunks (no single-stream bottleneck);
//   - every IPv4 address of the phone (Wi-Fi, second Wi-Fi, hotspot, USB
//     tethering, Ethernet) is advertised, so the PC can spread connections
//     over several NICs;
//   - /sdcard is served straight from /data/media/0, bypassing the FUSE layer.
package main

import (
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"lanxfer/proto"
)

var (
	flagPort  = flag.Int("port", proto.DefaultPort, "HTTP port")
	flagToken = flag.String("token", "", "access token (required)")
	flagRaw   = flag.Bool("raw", true, "serve /sdcard from /data/media/0 (bypass FUSE, root only)")
	flagName  = flag.String("name", "", "device name shown on PC (default: ro.product.model)")
	flagScan  = flag.Bool("scan", true, "ask MediaProvider to index uploaded files")
)

const rawMedia = "/data/media/0"

var (
	rawOn      bool
	deviceName string
)

func main() {
	flag.Parse()
	if *flagToken == "" {
		log.Fatal("token required")
	}
	rawOn = *flagRaw
	if rawOn {
		if st, err := os.Stat(rawMedia); err != nil || !st.IsDir() {
			rawOn = false
		}
	}
	deviceName = *flagName
	if deviceName == "" {
		deviceName = getprop("ro.product.model")
	}
	if deviceName == "" {
		deviceName, _ = os.Hostname()
	}
	go discovery()
	go sessionReaper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/api/info", auth(handleInfo))
	mux.HandleFunc("/api/list", auth(handleList))
	mux.HandleFunc("/api/walk", auth(handleWalk))
	mux.HandleFunc("/api/read", auth(handleRead))
	mux.HandleFunc("/api/batch", auth(handleBatch))
	mux.HandleFunc("/api/wbatch", auth(handleWBatch))
	mux.HandleFunc("/api/create", auth(handleCreate))
	mux.HandleFunc("/api/write", auth(handleWrite))
	mux.HandleFunc("/api/done", auth(handleDone))
	mux.HandleFunc("/api/mkdir", auth(handleMkdir))
	mux.HandleFunc("/api/delete", auth(handleDelete))
	mux.HandleFunc("/api/rename", auth(handleRename))

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", *flagPort),
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("lanxferd %s on :%d raw=%v name=%q", proto.Version, *flagPort, rawOn, deviceName)
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(srv.Serve(tuneListener{ln.(*net.TCPListener)}))
}

// tuneListener enlarges socket buffers for every accepted connection.
type tuneListener struct{ *net.TCPListener }

func (l tuneListener) Accept() (net.Conn, error) {
	c, err := l.AcceptTCP()
	if err != nil {
		return nil, err
	}
	c.SetNoDelay(true)
	c.SetReadBuffer(4 << 20)
	c.SetWriteBuffer(4 << 20)
	return c, nil
}

func getprop(k string) string {
	out, err := exec.Command("getprop", k).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ---------- discovery ----------

func discovery() {
	addr := &net.UDPAddr{Port: proto.DiscoveryPort}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		log.Printf("discovery disabled: %v", err)
		return
	}
	buf := make([]byte, 512)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(buf[:n])) != proto.DiscoverMagic {
			continue
		}
		b, _ := json.Marshal(proto.DiscoverReply{Magic: proto.DiscoverMagic, Name: deviceName, Port: *flagPort})
		conn.WriteToUDP(b, from)
	}
}

// ---------- helpers ----------

func auth(h http.HandlerFunc) http.HandlerFunc {
	tok := []byte(*flagToken)
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(proto.TokenHeader)), tok) != 1 {
			log.Printf("[错误] 令牌错误，来自 %s", r.RemoteAddr)
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		seen(r.RemoteAddr)
		h(w, r)
	}
}

// ---------- client sessions (for the log) ----------

// The PC pings every 3 s while connected, so a client silent for
// sessionIdle is considered gone.
const sessionIdle = 15 * time.Second

var (
	sessMu sync.Mutex
	sess   = map[string]*session{} // client IP -> session
)

type session struct{ first, last time.Time }

func seen(remote string) {
	ip, _, err := net.SplitHostPort(remote)
	if err != nil {
		ip = remote
	}
	now := time.Now()
	sessMu.Lock()
	s := sess[ip]
	if s == nil {
		s = &session{first: now}
		sess[ip] = s
		log.Printf("[连接] 电脑 %s 已连接", ip)
	}
	s.last = now
	sessMu.Unlock()
}

func sessionReaper() {
	for range time.Tick(5 * time.Second) {
		now := time.Now()
		sessMu.Lock()
		for ip, s := range sess {
			if now.Sub(s.last) > sessionIdle {
				log.Printf("[断开] 电脑 %s 已断开，本次连接持续 %s", ip, s.last.Sub(s.first).Round(time.Second))
				delete(sess, ip)
			}
		}
		sessMu.Unlock()
	}
}

// realPath maps a phone path as the user sees it to the path we touch on disk.
func realPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty path")
	}
	p = path.Clean("/" + p)
	if rawOn {
		for _, pre := range []string{"/sdcard", "/storage/emulated/0", "/storage/self/primary"} {
			if p == pre || strings.HasPrefix(p, pre+"/") {
				return rawMedia + p[len(pre):], nil
			}
		}
	}
	return p, nil
}

func isMedia(real string) bool {
	return rawOn && (real == rawMedia || strings.HasPrefix(real, rawMedia+"/"))
}

// fixOwner makes files we create under /data/media look like normal
// shared-storage files (owner media_rw, correct SELinux label), so apps
// can see and delete them.
func fixOwner(real string) {
	if !isMedia(real) {
		return
	}
	os.Lchown(real, 1023, 1023)
	setxattr(real, "security.selinux", "u:object_r:media_rw_data_file:s0")
}

func setxattr(p, name, val string) {
	syscall.Setxattr(p, name, append([]byte(val), 0), 0)
}

// mkdirAll creates directories and fixes ownership of each new one.
func mkdirAll(real string) error {
	if st, err := os.Stat(real); err == nil {
		if st.IsDir() {
			return nil
		}
		return fmt.Errorf("%s exists and is not a directory", real)
	}
	parent := filepath.Dir(real)
	if parent != real {
		if err := mkdirAll(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(real, 0o775); err != nil && !os.IsExist(err) {
		return err
	}
	fixOwner(real)
	return nil
}

func q(r *http.Request, k string) string { return r.URL.Query().Get(k) }
func qi(r *http.Request, k string) int64 {
	v, _ := strconv.ParseInt(q(r, k), 10, 64)
	return v
}

func fail(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	if errors.Is(err, syscall.ENAMETOOLONG) {
		log.Printf("[错误] 文件名过长：%v", err)
		http.Error(w, "文件名过长（手机单个名称最多 255 字节）: "+err.Error(), http.StatusBadRequest)
		return
	}
	if os.IsNotExist(err) {
		code = http.StatusNotFound
	} else if os.IsPermission(err) {
		code = http.StatusForbidden
	}
	http.Error(w, err.Error(), code)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func localAddrs() []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		n := strings.ToLower(i.Name)
		// skip cellular / VPN tunnels, they can't reach the LAN
		if strings.HasPrefix(n, "rmnet") || strings.HasPrefix(n, "ccmni") || strings.HasPrefix(n, "tun") || strings.HasPrefix(n, "dummy") {
			continue
		}
		as, _ := i.Addrs()
		for _, a := range as {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}

// ---------- handlers ----------

func handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, proto.Info{Name: deviceName, Version: proto.Version, Port: *flagPort, Addrs: localAddrs(), Home: "/sdcard", Raw: rawOn})
}

func handleList(w http.ResponseWriter, r *http.Request) {
	real, err := realPath(q(r, "p"))
	if err != nil {
		fail(w, err)
		return
	}
	des, err := os.ReadDir(real)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]proto.Entry, 0, len(des))
	for _, de := range des {
		info, err := de.Info()
		if err != nil {
			continue
		}
		dir := info.IsDir()
		if info.Mode()&os.ModeSymlink != 0 {
			if st, err := os.Stat(filepath.Join(real, de.Name())); err == nil {
				dir = st.IsDir()
				info = st
			}
		}
		e := proto.Entry{Name: de.Name(), MTime: info.ModTime().UnixMilli(), Dir: dir}
		if !dir {
			e.Size = info.Size()
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, out)
}

// handleWalk returns every file and directory below p (relative paths),
// directories before their contents. Used to expand a folder download.
func handleWalk(w http.ResponseWriter, r *http.Request) {
	real, err := realPath(q(r, "p"))
	if err != nil {
		fail(w, err)
		return
	}
	var out []proto.Entry
	err = filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == real {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(real, p)
		e := proto.Entry{Name: filepath.ToSlash(rel), MTime: info.ModTime().UnixMilli(), Dir: d.IsDir()}
		if !d.IsDir() {
			if !info.Mode().IsRegular() {
				return nil
			}
			e.Size = info.Size()
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, out)
}

// handleBatch streams many small files in one response, so the PC does not
// pay one HTTP round trip per file. Request body: JSON array of paths.
// Response, per path in order: int64 size (big endian) then size bytes; or
// size -1, uint16 length and an error message.
func handleBatch(w http.ResponseWriter, r *http.Request) {
	var paths []string
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&paths); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	var hdr [8]byte
	for _, p := range paths {
		f, size, err := openForBatch(p)
		if err != nil {
			msg := err.Error()
			if len(msg) > 1000 {
				msg = msg[:1000]
			}
			binary.BigEndian.PutUint64(hdr[:], ^uint64(0))
			w.Write(hdr[:])
			binary.BigEndian.PutUint16(hdr[:2], uint16(len(msg)))
			w.Write(hdr[:2])
			if _, err := io.WriteString(w, msg); err != nil {
				return
			}
			continue
		}
		binary.BigEndian.PutUint64(hdr[:], uint64(size))
		if _, err := w.Write(hdr[:]); err != nil {
			f.Close()
			return
		}
		n, err := io.Copy(w, io.LimitReader(f, size))
		f.Close()
		if n < size { // file shrank meanwhile (or read error): pad to keep the framing
			_ = err
			if _, err := w.Write(make([]byte, size-n)); err != nil {
				return
			}
		}
	}
}

// handleWBatch receives many small files in one request body, framed per
// file as: uint16 path length, path, int64 mtime (ms), int64 size, data.
// It answers a JSON array of {"i":index,"e":"error"} for files that failed.
func handleWBatch(w http.ResponseWriter, r *http.Request) {
	type ferr struct {
		I int    `json:"i"`
		E string `json:"e"`
	}
	var errs []ferr
	made := map[string]bool{}
	var hdr [16]byte
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	for i := 0; ; i++ {
		if _, err := io.ReadFull(r.Body, hdr[:2]); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			http.Error(w, "bad batch: "+err.Error(), http.StatusBadRequest)
			return
		}
		pb := make([]byte, binary.BigEndian.Uint16(hdr[:2]))
		if _, err := io.ReadFull(r.Body, pb); err != nil {
			http.Error(w, "bad batch", http.StatusBadRequest)
			return
		}
		if _, err := io.ReadFull(r.Body, hdr[:16]); err != nil {
			http.Error(w, "bad batch", http.StatusBadRequest)
			return
		}
		mtime := int64(binary.BigEndian.Uint64(hdr[:8]))
		size := int64(binary.BigEndian.Uint64(hdr[8:16]))
		err := func() error {
			real, err := realPath(string(pb))
			if err != nil {
				return err
			}
			dir := filepath.Dir(real)
			if !made[dir] {
				if err := mkdirAll(dir); err != nil {
					return err
				}
				made[dir] = true
			}
			f, err := os.OpenFile(real, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o664)
			if err != nil {
				return err
			}
			n, err := io.CopyBuffer(struct{ io.Writer }{f}, io.LimitReader(r.Body, size), *bp)
			size -= n
			cerr := f.Close()
			if err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			if size != 0 {
				return io.ErrUnexpectedEOF
			}
			fixOwner(real)
			finish(real, mtime)
			return nil
		}()
		if err != nil {
			if errors.Is(err, syscall.ENAMETOOLONG) {
				log.Printf("[错误] 文件名过长：%v", err)
			}
			errs = append(errs, ferr{i, err.Error()})
			if size > 0 { // skip the rest of this file's data
				if _, err := io.CopyN(io.Discard, r.Body, size); err != nil {
					http.Error(w, "bad batch", http.StatusBadRequest)
					return
				}
			}
		}
	}
	writeJSON(w, errs)
}

func openForBatch(p string) (*os.File, int64, error) {
	real, err := realPath(p)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	if st.IsDir() {
		f.Close()
		return nil, 0, errors.New("is a directory")
	}
	return f, st.Size(), nil
}

func handleRead(w http.ResponseWriter, r *http.Request) {
	real, err := realPath(q(r, "p"))
	if err != nil {
		fail(w, err)
		return
	}
	f, err := os.Open(real)
	if err != nil {
		fail(w, err)
		return
	}
	defer f.Close()
	off, n := qi(r, "off"), qi(r, "len")
	if n <= 0 {
		st, err := f.Stat()
		if err != nil {
			fail(w, err)
			return
		}
		n = st.Size() - off
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	io.Copy(w, &io.LimitedReader{R: f, N: n}) // net/http uses sendfile for this
}

// create (or truncate) a file and pre-size it, so parallel chunk writes can land anywhere.
func handleCreate(w http.ResponseWriter, r *http.Request) {
	real, err := realPath(q(r, "p"))
	if err != nil {
		fail(w, err)
		return
	}
	if err := mkdirAll(filepath.Dir(real)); err != nil {
		fail(w, err)
		return
	}
	f, err := os.OpenFile(real, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o664)
	if err != nil {
		fail(w, err)
		return
	}
	if sz := qi(r, "size"); sz > 0 {
		f.Truncate(sz)
	}
	f.Close()
	fixOwner(real)
	w.Write([]byte("ok"))
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

// write the request body at offset off. With trunc=1 the file is created
// first (single-request path for small files); with done=1 it is finished too.
func handleWrite(w http.ResponseWriter, r *http.Request) {
	real, err := realPath(q(r, "p"))
	if err != nil {
		fail(w, err)
		return
	}
	flags := os.O_WRONLY
	if q(r, "trunc") == "1" {
		if err := mkdirAll(filepath.Dir(real)); err != nil {
			fail(w, err)
			return
		}
		flags |= os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(real, flags, 0o664)
	if err != nil {
		fail(w, err)
		return
	}
	if _, err := f.Seek(qi(r, "off"), io.SeekStart); err != nil {
		f.Close()
		fail(w, err)
		return
	}
	bp := bufPool.Get().(*[]byte)
	n, err := io.CopyBuffer(struct{ io.Writer }{f}, r.Body, *bp)
	bufPool.Put(bp)
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		fail(w, err)
		return
	}
	if want := r.ContentLength; want >= 0 && n != want {
		http.Error(w, "short body", http.StatusBadRequest)
		return
	}
	if q(r, "trunc") == "1" {
		fixOwner(real)
	}
	if q(r, "done") == "1" {
		finish(real, qi(r, "mtime"))
	}
	w.Write([]byte("ok"))
}

func handleDone(w http.ResponseWriter, r *http.Request) {
	real, err := realPath(q(r, "p"))
	if err != nil {
		fail(w, err)
		return
	}
	finish(real, qi(r, "mtime"))
	w.Write([]byte("ok"))
}

func finish(real string, mtime int64) {
	if mtime > 0 {
		t := time.UnixMilli(mtime)
		os.Chtimes(real, t, t)
	}
	if *flagScan && isMedia(real) {
		queueScan(real)
	}
}

// ---------- media scan (batched, best effort) ----------

var (
	scanMu    sync.Mutex
	scanQueue []string
	scanTimer *time.Timer
)

func queueScan(real string) {
	scanMu.Lock()
	defer scanMu.Unlock()
	scanQueue = append(scanQueue, "/sdcard"+strings.TrimPrefix(real, rawMedia))
	if scanTimer == nil {
		scanTimer = time.AfterFunc(3*time.Second, runScan)
	}
}

func runScan() {
	scanMu.Lock()
	files := scanQueue
	scanQueue, scanTimer = nil, nil
	scanMu.Unlock()
	// Only media files matter to the gallery / music apps.
	for _, f := range files {
		switch strings.ToLower(path.Ext(f)) {
		case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".heic", ".bmp", ".mp4", ".mkv", ".mov", ".avi", ".webm", ".3gp", ".mp3", ".flac", ".m4a", ".aac", ".wav", ".ogg", ".opus":
			exec.Command("am", "broadcast", "-a", "android.intent.action.MEDIA_SCANNER_SCAN_FILE", "-d", "file://"+f).Run()
		}
	}
}

func handleMkdir(w http.ResponseWriter, r *http.Request) {
	real, err := realPath(q(r, "p"))
	if err != nil {
		fail(w, err)
		return
	}
	if err := mkdirAll(real); err != nil {
		fail(w, err)
		return
	}
	w.Write([]byte("ok"))
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	real, err := realPath(q(r, "p"))
	if err != nil {
		fail(w, err)
		return
	}
	if real == "/" || real == rawMedia || real == "/data" || real == "/system" {
		http.Error(w, "refusing to delete "+real, http.StatusForbidden)
		return
	}
	if err := os.RemoveAll(real); err != nil {
		fail(w, err)
		return
	}
	w.Write([]byte("ok"))
}

func handleRename(w http.ResponseWriter, r *http.Request) {
	from, err := realPath(q(r, "p"))
	if err != nil {
		fail(w, err)
		return
	}
	to, err := realPath(q(r, "to"))
	if err != nil {
		fail(w, err)
		return
	}
	if err := os.Rename(from, to); err != nil {
		fail(w, err)
		return
	}
	w.Write([]byte("ok"))
}
