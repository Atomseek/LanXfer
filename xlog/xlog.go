// Package xlog is LanXfer's small log system: one line per event, written to
// a file (rotated at MaxSize) and kept in memory for the GUI log window.
//
// Line format:  2026-10-17 11:20:03.123 [连接] message
package xlog

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	MaxSize  = 5 << 20 // rotate the file to .1 beyond this
	RingSize = 5000    // lines kept in memory
)

type Logger struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	ring []string
	subs []func(string)
}

// Open opens (appending) the log file at path. A file error is not fatal:
// the logger then keeps lines in memory only.
func Open(path string) *Logger {
	l := &Logger{path: path}
	l.open()
	return l
}

func (l *Logger) open() {
	if l.path == "" {
		return
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	st, _ := f.Stat()
	l.f = f
	if st != nil {
		l.size = st.Size()
	}
}

func (l *Logger) Path() string { return l.path }

// Log records one event under a category such as 连接, 断开, 重连, 传输, 截断, 错误.
func (l *Logger) Log(cat, msg string) {
	if l == nil {
		return
	}
	msg = strings.ReplaceAll(strings.TrimSpace(msg), "\n", " ")
	line := fmt.Sprintf("%s [%s] %s", time.Now().Format("2006-01-02 15:04:05.000"), cat, msg)
	l.mu.Lock()
	l.ring = append(l.ring, line)
	if len(l.ring) > RingSize {
		l.ring = append([]string(nil), l.ring[len(l.ring)-RingSize:]...)
	}
	if l.f != nil {
		n, _ := l.f.WriteString(line + "\r\n")
		l.size += int64(n)
		if l.size > MaxSize {
			l.f.Close()
			os.Remove(l.path + ".1")
			os.Rename(l.path, l.path+".1")
			l.f, l.size = nil, 0
			l.open()
		}
	}
	subs := append([]func(string){}, l.subs...)
	l.mu.Unlock()
	for _, s := range subs {
		s(line)
	}
}

func (l *Logger) Logf(cat, format string, a ...any) { l.Log(cat, fmt.Sprintf(format, a...)) }

// Lines returns the in-memory lines (oldest first).
func (l *Logger) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ring...)
}

// Subscribe calls fn for every new line (from the logging goroutine).
func (l *Logger) Subscribe(fn func(string)) {
	l.mu.Lock()
	l.subs = append(l.subs, fn)
	l.mu.Unlock()
}

func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

// Category extracts the [category] of a line ("" if none).
func Category(line string) string {
	i := strings.Index(line, " [")
	if i < 0 {
		return ""
	}
	j := strings.Index(line[i+2:], "]")
	if j < 0 {
		return ""
	}
	return line[i+2 : i+2+j]
}

// Dur formats a duration for humans: 3秒, 2分05秒, 1小时02分.
func Dur(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%d秒", s)
	case s < 3600:
		return fmt.Sprintf("%d分%02d秒", s/60, s%60)
	default:
		return fmt.Sprintf("%d小时%02d分", s/3600, s%3600/60)
	}
}
