package client

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Small files are sent many per HTTP request (one stream, no per-file round
// trip), which is what makes folders with tens of thousands of photos and
// chat cache files fast.
const (
	BatchFileMax  = 1 << 20  // files up to this size are batched
	BatchMaxFiles = 256      // files per batch
	BatchMaxBytes = 16 << 20 // bytes per batch
)

// SupportsBatch reports whether the phone daemon has /api/batch and /api/wbatch (v1.2.0+).
func (c *Client) SupportsBatch() bool {
	v := strings.SplitN(c.Info.Version, ".", 3)
	if len(v) < 2 {
		return false
	}
	maj, _ := strconv.Atoi(v[0])
	min, _ := strconv.Atoi(v[1])
	return maj > 1 || (maj == 1 && min >= 2)
}

func (j *Job) buildBatches() {
	var cur []*fileState
	var bytes int64
	flush := func() {
		if len(cur) > 0 {
			j.tasks = append(j.tasks, task{batch: cur})
			cur, bytes = nil, 0
		}
	}
	for _, f := range j.smalls {
		if len(cur) >= BatchMaxFiles || bytes+f.size > BatchMaxBytes {
			flush()
		}
		cur = append(cur, f)
		bytes += f.size
	}
	flush()
	j.smalls = nil
}

func (j *Job) runBatch(base string, files []*fileState) {
	p := j.Progress
	p.current.Store(files[0].remote)
	var err error
	for attempt := 0; attempt <= len(retryDelays); attempt++ {
		if j.ctx.Err() != nil {
			return
		}
		if j.upload {
			err = j.upBatch(base, files)
		} else {
			files, err = j.downBatch(base, files)
		}
		if err == nil || len(files) == 0 {
			return
		}
		if errors.Is(err, ErrBadToken) || isPermanent(err) || j.ctx.Err() != nil || attempt >= len(retryDelays) {
			break
		}
		logf("重试", "批量传输（%d 个文件，从 %s 起）失败：%v，%v 后重试", len(files), files[0].remote, err, retryDelays[attempt])
		select {
		case <-time.After(retryDelays[attempt]):
		case <-j.ctx.Done():
		}
	}
	if err != nil && j.ctx.Err() == nil {
		for _, f := range files {
			if !f.failed.Swap(true) {
				p.addErr(f.remote + ": " + err.Error())
			}
		}
		logf("错误", "批量传输失败，已跳过 %d 个文件（从 %s 起）：%v", len(files), files[0].remote, err)
	}
}

func (j *Job) fileFailed(f *fileState, msg string) {
	if !f.failed.Swap(true) {
		j.Progress.addErr(f.remote + ": " + msg)
		logf("错误", "传输失败，已跳过：%s：%s", f.remote, msg)
	}
}

var dirMade sync.Map // local dirs already created during downloads

func mkdirOnce(dir string) error {
	if _, ok := dirMade.Load(dir); ok {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dirMade.Store(dir, true)
	return nil
}

// downBatch fetches files in one request and writes them straight to disk.
// It returns the files not finished yet when the stream broke.
func (j *Job) downBatch(base string, files []*fileState) ([]*fileState, error) {
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.remote
	}
	body, _ := json.Marshal(paths)
	resp, err := j.c.req(j.ctx, j.c.http, "POST", base, "/api/batch", nil, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return files, err
	}
	defer resp.Body.Close()
	r := resp.Body
	var hdr [8]byte
	buf := make([]byte, 256<<10)
	for i, f := range files {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return files[i:], err
		}
		size := int64(binary.BigEndian.Uint64(hdr[:]))
		if size < 0 {
			if _, err := io.ReadFull(r, hdr[:2]); err != nil {
				return files[i:], err
			}
			msg := make([]byte, binary.BigEndian.Uint16(hdr[:2]))
			if _, err := io.ReadFull(r, msg); err != nil {
				return files[i:], err
			}
			j.fileFailed(f, string(msg))
			continue
		}
		var moved int64
		werr := func() error {
			if err := mkdirOnce(filepath.Dir(f.local)); err != nil {
				return err
			}
			fh, err := os.OpenFile(f.local, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			_, err = io.CopyBuffer(fh, countReader{r: io.LimitReader(r, size), p: j.Progress, n: &moved}, buf)
			if cerr := fh.Close(); err == nil {
				err = cerr
			}
			return err
		}()
		if moved < size { // network broke inside this file, or a local write error
			j.Progress.DoneBytes.Add(-moved)
			if werr != nil && !isNetErr(werr) {
				// local problem (disk, name): skip the file, keep the stream in sync
				j.fileFailed(f, werr.Error())
				if _, err := io.CopyN(io.Discard, r, size-moved); err != nil {
					return files[i+1:], err
				}
				continue
			}
			if werr == nil {
				werr = io.ErrUnexpectedEOF
			}
			return files[i:], werr
		}
		if werr != nil {
			j.fileFailed(f, werr.Error())
			continue
		}
		if f.mtime > 0 {
			t := time.UnixMilli(f.mtime)
			os.Chtimes(f.local, t, t)
		}
		j.Progress.DoneFiles.Add(1)
	}
	return nil, nil
}

// isNetErr guesses whether err came from the response body (network) rather
// than from the local file.
func isNetErr(err error) bool {
	var pe *os.PathError
	return !errors.As(err, &pe)
}

// upBatch sends files in one request body. On a network error the whole
// batch is retried (the phone simply rewrites the files).
func (j *Job) upBatch(base string, files []*fileState) error {
	var moved int64
	pr, pw := io.Pipe()
	skipped := map[int]bool{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var hdr [16]byte
		buf := make([]byte, 256<<10)
		for i, f := range files {
			fh, err := os.Open(f.local)
			if err != nil {
				skipped[i] = true
				j.fileFailed(f, err.Error())
				// still send an empty entry so indexes match
				fh = nil
			}
			size := f.size
			if fh != nil {
				if st, err := fh.Stat(); err == nil {
					size = st.Size()
				}
			} else {
				size = 0
			}
			pb := []byte(f.remote)
			binary.BigEndian.PutUint16(hdr[:2], uint16(len(pb)))
			if _, err := pw.Write(hdr[:2]); err != nil {
				closeF(fh)
				return
			}
			pw.Write(pb)
			binary.BigEndian.PutUint64(hdr[:8], uint64(f.mtime))
			binary.BigEndian.PutUint64(hdr[8:16], uint64(size))
			if _, err := pw.Write(hdr[:16]); err != nil {
				closeF(fh)
				return
			}
			if fh != nil {
				n, err := io.CopyBuffer(pw, countReader{r: io.LimitReader(fh, size), p: j.Progress, n: &moved}, buf)
				fh.Close()
				if n < size { // file shrank: pad to keep framing
					if err == nil || errors.Is(err, io.EOF) || isLocalErr(err) {
						if _, err := pw.Write(make([]byte, size-n)); err != nil {
							return
						}
					} else {
						return
					}
				}
			}
		}
		pw.Close()
	}()
	resp, err := j.c.req(j.ctx, j.c.http, "PUT", base, "/api/wbatch", nil, pr, -1)
	pr.CloseWithError(errors.New("request finished"))
	<-done
	if err != nil {
		j.Progress.DoneBytes.Add(-moved)
		return err
	}
	defer resp.Body.Close()
	var errs []struct {
		I int    `json:"i"`
		E string `json:"e"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&errs); err != nil {
		j.Progress.DoneBytes.Add(-moved)
		return fmt.Errorf("批量上传应答错误：%w", err)
	}
	bad := map[int]bool{}
	for _, e := range errs {
		if e.I >= 0 && e.I < len(files) {
			bad[e.I] = true
			j.fileFailed(files[e.I], e.E)
		}
	}
	for i := range files {
		if !bad[i] && !skipped[i] {
			j.Progress.DoneFiles.Add(1)
		}
	}
	return nil
}

func closeF(f *os.File) {
	if f != nil {
		f.Close()
	}
}

func isLocalErr(err error) bool {
	var pe *os.PathError
	return errors.As(err, &pe)
}
