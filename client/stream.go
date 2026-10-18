package client

import (
	"context"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"
)

// StreamChunk is the read-ahead unit for streaming a phone file (used for
// drag-out to Explorer, which pulls the file sequentially through IStream).
const StreamChunk = 4 << 20

// Stream reads one phone file sequentially while fetching the next chunks in
// parallel over several connections / channels.
type Stream struct {
	c      *Client
	path   string
	size   int64
	ahead  int
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	pos     int64             // read position
	chunks  map[int64]*sChunk // chunk index -> chunk
	nextReq int64             // next chunk index to request
	OnBytes func(n int64)     // optional progress callback
}

type sChunk struct {
	done chan struct{}
	data []byte
	err  error
}

func (c *Client) OpenStream(p string, size int64, ahead int) *Stream {
	if ahead < 1 {
		ahead = 4
	}
	s := &Stream{c: c, path: p, size: size, ahead: ahead, chunks: map[int64]*sChunk{}}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

func (s *Stream) Size() int64 { return s.size }

func (s *Stream) Close() error { s.cancel(); return nil }

// Seek repositions the stream (whence as io.Seek*).
func (s *Stream) Seek(off int64, whence int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch whence {
	case io.SeekCurrent:
		off += s.pos
	case io.SeekEnd:
		off += s.size
	}
	if off < 0 {
		off = 0
	}
	if off != s.pos {
		s.pos = off
		// drop chunks that are behind; requests restart from the new position
		ci := off / StreamChunk
		for k := range s.chunks {
			if k < ci {
				delete(s.chunks, k)
			}
		}
		if s.nextReq < ci {
			s.nextReq = ci
		}
		if _, ok := s.chunks[ci]; !ok {
			s.nextReq = ci
		}
	}
	return s.pos, nil
}

func (s *Stream) fetch(ci int64, ch *sChunk, base string) {
	defer close(ch.done)
	off := ci * StreamChunk
	n := min(StreamChunk, s.size-off)
	for attempt := 0; attempt <= len(retryDelays); attempt++ {
		if attempt > 0 {
			logf("重试", "读取 %s（偏移 %d）失败：%v，%v 后重试", s.path, off, ch.err, retryDelays[attempt-1])
			select {
			case <-time.After(retryDelays[attempt-1]):
			case <-s.ctx.Done():
				return
			}
		}
		resp, err := s.c.req(s.ctx, s.c.http, "GET", base, "/api/read", P("p", s.path, "off", strconv.FormatInt(off, 10), "len", strconv.FormatInt(n, 10)), nil, 0)
		if err != nil {
			ch.err = err
			if s.ctx.Err() != nil || errors.Is(err, ErrBadToken) || isPermanent(err) {
				return
			}
			continue
		}
		buf := make([]byte, n)
		_, err = io.ReadFull(resp.Body, buf)
		resp.Body.Close()
		if err == nil {
			ch.data, ch.err = buf, nil
			return
		}
		ch.err = err
	}
	if s.ctx.Err() == nil {
		logf("错误", "读取 %s 失败，已放弃：%v", s.path, ch.err)
	}
}

// schedule keeps up to `ahead` chunk requests in flight (s.mu held).
func (s *Stream) schedule() {
	total := (s.size + StreamChunk - 1) / StreamChunk
	cur := s.pos / StreamChunk
	if s.nextReq < cur {
		s.nextReq = cur
	}
	for s.nextReq < total && s.nextReq < cur+int64(s.ahead) {
		ci := s.nextReq
		s.nextReq++
		if _, ok := s.chunks[ci]; ok {
			continue
		}
		ch := &sChunk{done: make(chan struct{})}
		s.chunks[ci] = ch
		base := s.c.base(int(ci))
		go s.fetch(ci, ch, base)
	}
}

func (s *Stream) Read(p []byte) (int, error) {
	s.mu.Lock()
	if s.pos >= s.size {
		s.mu.Unlock()
		return 0, io.EOF
	}
	s.schedule()
	ci := s.pos / StreamChunk
	ch := s.chunks[ci]
	if ch == nil {
		ch = &sChunk{done: make(chan struct{})}
		s.chunks[ci] = ch
		go s.fetch(ci, ch, s.c.base(0))
	}
	s.mu.Unlock()

	<-ch.done
	if ch.err != nil {
		s.mu.Lock()
		delete(s.chunks, ci) // allow a retry on the next Read
		s.mu.Unlock()
		return 0, ch.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	in := s.pos - ci*StreamChunk
	n := copy(p, ch.data[in:])
	s.pos += int64(n)
	if s.pos >= (ci+1)*StreamChunk {
		delete(s.chunks, ci) // consumed
	}
	if s.OnBytes != nil {
		s.OnBytes(int64(n))
	}
	return n, nil
}
