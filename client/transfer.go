package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lanxfer/proto"
)

// ChunkSize is the unit of parallelism for large files. Small files are
// sent whole, several at a time, one per connection.
const ChunkSize = 8 << 20

type Progress struct {
	TotalBytes int64
	DoneBytes  atomic.Int64
	TotalFiles int64
	DoneFiles  atomic.Int64
	Started    time.Time
	Skipped    int64        // download: already present with same size and time
	current    atomic.Value // string
	errMu      sync.Mutex
	Errors     []string
}

func (p *Progress) Current() string {
	s, _ := p.current.Load().(string)
	return s
}
func (p *Progress) addErr(s string) {
	p.errMu.Lock()
	p.Errors = append(p.Errors, s)
	p.errMu.Unlock()
}
func (p *Progress) ErrorList() []string {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	return append([]string(nil), p.Errors...)
}

type fileState struct {
	local, remote string
	size, mtime   int64
	remaining     atomic.Int32
	failed        atomic.Bool
}

type task struct {
	batch  []*fileState // several small files in one request
	f      *fileState
	off, n int64
	single bool
}

// Job is one batch of transfers (upload or download).
type Job struct {
	c        *Client
	upload   bool
	Progress *Progress
	tasks    []task
	smalls   []*fileState
	cancel   context.CancelFunc
	ctx      context.Context
	batch    bool
}

func (j *Job) Cancel() { j.cancel() }

// ---------- planning ----------

// PlanUpload prepares sending local files/folders into remote directory dst.
func (c *Client) PlanUpload(locals []string, dst string) (*Job, error) {
	j := c.newJob(true)
	for _, l := range locals {
		st, err := os.Stat(l)
		if err != nil {
			return nil, err
		}
		base := PhonePath(filepath.Base(l))
		if !st.IsDir() {
			j.addFile(l, path.Join(dst, base), st.Size(), st.ModTime().UnixMilli())
			continue
		}
		root := path.Join(dst, base)
		filepath.WalkDir(l, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(l, p)
			rp := path.Join(root, PhonePath(filepath.ToSlash(rel)))
			if d.IsDir() {
				// make empty directories explicitly; others appear with their files
				if ents, _ := os.ReadDir(p); len(ents) == 0 {
					if err := c.Mkdir(rp); err != nil {
						j.Progress.addErr(rp + ": " + err.Error())
					}
				}
				return nil
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			j.addFile(p, rp, info.Size(), info.ModTime().UnixMilli())
			return nil
		})
	}
	return j, nil
}

// PlanDownload prepares fetching remote entries (from directory srcDir) into local directory dst.
func (c *Client) PlanDownload(srcDir string, items []proto.Entry, dst string) (*Job, error) {
	j := c.newJob(false)
	for _, it := range items {
		rp := path.Join(srcDir, it.Name)
		lp := filepath.Join(dst, WinParts([]string{it.Name})[0])
		if !it.Dir {
			j.addDown(lp, rp, it.Size, it.MTime)
			continue
		}
		if err := os.MkdirAll(lp, 0o755); err != nil {
			return nil, err
		}
		sub, err := c.Walk(rp)
		if err != nil {
			return nil, err
		}
		for _, e := range sub {
			parts := WinParts(strings.Split(e.Name, "/"))
			l := filepath.Join(append([]string{lp}, parts...)...)
			if e.Dir {
				os.MkdirAll(l, 0o755)
				continue
			}
			j.addDown(l, path.Join(rp, e.Name), e.Size, e.MTime)
		}
	}
	return j, nil
}

// addDown queues a download unless an identical file (same size, mtime
// within 2 s) is already there, like FastCopy's "different" mode.
func (j *Job) addDown(local, remote string, size, mtime int64) {
	if st, err := os.Stat(local); err == nil && st.Mode().IsRegular() && st.Size() == size {
		if d := st.ModTime().UnixMilli() - mtime; d > -2000 && d < 2000 {
			j.Progress.Skipped++
			return
		}
	}
	j.addFile(local, remote, size, mtime)
}

func (c *Client) newJob(upload bool) *Job {
	ctx, cancel := context.WithCancel(context.Background())
	return &Job{c: c, upload: upload, Progress: &Progress{}, ctx: ctx, cancel: cancel, batch: c.SupportsBatch()}
}

func (j *Job) addFile(local, remote string, size, mtime int64) {
	f := &fileState{local: local, remote: remote, size: size, mtime: mtime}
	j.Progress.TotalBytes += size
	j.Progress.TotalFiles++
	if size <= BatchFileMax && j.batch {
		j.smalls = append(j.smalls, f)
		return
	}
	if size <= ChunkSize {
		f.remaining.Store(1)
		j.tasks = append(j.tasks, task{f: f, off: 0, n: size, single: true})
		return
	}
	n := int32((size + ChunkSize - 1) / ChunkSize)
	f.remaining.Store(n)
	for off := int64(0); off < size; off += ChunkSize {
		j.tasks = append(j.tasks, task{f: f, off: off, n: min(ChunkSize, size-off)})
	}
}

// ---------- running ----------

// Run executes the job with `workers` parallel connections and blocks until done.
func (j *Job) Run(workers int) error {
	if workers < 1 {
		workers = 4
	}
	p := j.Progress
	p.Started = time.Now()
	j.buildBatches()
	ch := make(chan task)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		base := j.c.base(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range ch {
				j.runTask(base, t)
			}
		}()
	}
	prepared := map[*fileState]bool{}
	for _, t := range j.tasks {
		if j.ctx.Err() != nil {
			break
		}
		if t.batch != nil {
			select {
			case ch <- t:
			case <-j.ctx.Done():
			}
			continue
		}
		if !t.single && !prepared[t.f] {
			prepared[t.f] = true
			if err := j.prepare(t.f); err != nil {
				t.f.failed.Store(true)
				p.addErr(t.f.remote + ": " + err.Error())
			}
		}
		if t.f.failed.Load() {
			continue
		}
		select {
		case ch <- t:
		case <-j.ctx.Done():
		}
	}
	close(ch)
	wg.Wait()
	if j.ctx.Err() != nil {
		return errors.New("已取消")
	}
	if errs := p.ErrorList(); len(errs) > 0 {
		return fmt.Errorf("%d 个错误，第一个：%s", len(errs), errs[0])
	}
	return nil
}

// prepare pre-creates a multi-chunk file at full size on the receiving side.
func (j *Job) prepare(f *fileState) error {
	if j.upload {
		return j.c.post("/api/create", P("p", f.remote, "size", strconv.FormatInt(f.size, 10)))
	}
	if err := os.MkdirAll(filepath.Dir(f.local), 0o755); err != nil {
		return err
	}
	fh, err := os.OpenFile(f.local, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	err = fh.Truncate(f.size)
	fh.Close()
	return err
}

func (j *Job) runTask(base string, t task) {
	if t.batch != nil {
		j.runBatch(base, t.batch)
		return
	}
	p := j.Progress
	p.current.Store(t.f.remote)
	var err error
	for attempt := 0; attempt < len(retryDelays)+1; attempt++ {
		if t.f.failed.Load() || j.ctx.Err() != nil {
			return
		}
		var moved int64
		if j.upload {
			moved, err = j.upChunk(base, t)
		} else {
			moved, err = j.downChunk(base, t)
		}
		if err == nil {
			break
		}
		p.DoneBytes.Add(-moved) // roll back partial progress before retrying
		if errors.Is(err, ErrBadToken) || isPermanent(err) || j.ctx.Err() != nil || attempt >= len(retryDelays) {
			break
		}
		logf("重试", "%s（偏移 %d）第 %d 次失败：%v，%v 后重试", t.f.remote, t.off, attempt+1, err, retryDelays[attempt])
		select {
		case <-time.After(retryDelays[attempt]):
		case <-j.ctx.Done():
		}
	}
	if err != nil {
		if j.ctx.Err() == nil && !t.f.failed.Swap(true) {
			p.addErr(t.f.remote + ": " + err.Error())
			logf("错误", "传输失败，已跳过：%s：%v", t.f.remote, err)
		}
		return
	}
	if t.f.remaining.Add(-1) == 0 {
		j.finishFile(t.f, base)
		p.DoneFiles.Add(1)
	}
}

func (j *Job) finishFile(f *fileState, base string) {
	if j.upload {
		if f.size > ChunkSize { // single-chunk files were finished by done=1
			resp, err := j.c.req(j.ctx, j.c.ctlClient(), "POST", base, "/api/done", P("p", f.remote, "mtime", strconv.FormatInt(f.mtime, 10)), nil, 0)
			if err == nil {
				resp.Body.Close()
			}
		}
		return
	}
	if f.mtime > 0 {
		t := time.UnixMilli(f.mtime)
		os.Chtimes(f.local, t, t)
	}
}

type countReader struct {
	r io.Reader
	p *Progress
	n *int64
}

func (c countReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.p.DoneBytes.Add(int64(n))
	*c.n += int64(n)
	return n, err
}

func (j *Job) upChunk(base string, t task) (int64, error) {
	var moved int64
	fh, err := os.Open(t.f.local)
	if err != nil {
		return 0, err
	}
	defer fh.Close()
	params := P("p", t.f.remote, "off", strconv.FormatInt(t.off, 10))
	if t.single {
		params.Set("trunc", "1")
		params.Set("done", "1")
		params.Set("mtime", strconv.FormatInt(t.f.mtime, 10))
	}
	body := countReader{r: io.NewSectionReader(fh, t.off, t.n), p: j.Progress, n: &moved}
	resp, err := j.c.req(j.ctx, j.c.http, "PUT", base, "/api/write", params, body, t.n)
	if err != nil {
		return moved, err
	}
	resp.Body.Close()
	return moved, nil
}

func (j *Job) downChunk(base string, t task) (int64, error) {
	var moved int64
	params := P("p", t.f.remote, "off", strconv.FormatInt(t.off, 10), "len", strconv.FormatInt(t.n, 10))
	resp, err := j.c.req(j.ctx, j.c.http, "GET", base, "/api/read", params, nil, 0)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var fh *os.File
	if t.single {
		os.MkdirAll(filepath.Dir(t.f.local), 0o755)
		fh, err = os.OpenFile(t.f.local, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	} else {
		fh, err = os.OpenFile(t.f.local, os.O_WRONLY, 0o644)
	}
	if err != nil {
		return 0, err
	}
	defer fh.Close()
	w := io.NewOffsetWriter(fh, t.off)
	buf := make([]byte, 1<<20)
	n, err := io.CopyBuffer(w, countReader{r: resp.Body, p: j.Progress, n: &moved}, buf)
	if err != nil {
		return moved, err
	}
	if n != t.n {
		return moved, fmt.Errorf("数据不完整 %d/%d", n, t.n)
	}
	return moved, nil
}

// retryDelays rides out short Wi-Fi drops (~20 s in total) before a chunk is
// given up; the GUI reconnects in the meantime.
var retryDelays = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 8 * time.Second}

// isPermanent reports errors a retry cannot fix (HTTP 4xx from the phone).
func isPermanent(err error) bool {
	m := err.Error()
	return strings.HasPrefix(m, "400") || strings.HasPrefix(m, "403") || strings.HasPrefix(m, "404")
}

// SafeName makes a phone file name legal on Windows.
func SafeName(s string) string {
	r := strings.NewReplacer("<", "＜", ">", "＞", ":", "：", "\"", "＂", "\\", "＼", "|", "｜", "?", "？", "*", "＊")
	s = r.Replace(s)
	s = strings.TrimRight(s, ". ")
	if s == "" {
		s = "_"
	}
	switch strings.ToUpper(strings.SplitN(s, ".", 2)[0]) {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		s = "_" + s
	}
	return s
}
