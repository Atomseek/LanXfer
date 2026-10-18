// lanxfer-cli is a tiny command-line client, handy for testing and scripting.
//
//	lanxfer-cli -h 192.168.1.5 -t TOKEN discover
//	lanxfer-cli -h 192.168.1.5 -t TOKEN ls /sdcard
//	lanxfer-cli -h 192.168.1.5 -t TOKEN put /sdcard/Download  file-or-dir...
//	lanxfer-cli -h 192.168.1.5 -t TOKEN get /sdcard/DCIM Camera  ./out
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"lanxfer/client"
	"lanxfer/proto"
)

func main() {
	host := flag.String("h", "", "phone ip[:port]")
	tok := flag.String("t", "", "token")
	conns := flag.Int("n", 8, "parallel connections")
	flag.Parse()
	client.Logf = func(cat, msg string) { fmt.Fprintf(os.Stderr, "[%s] %s\n", cat, msg) }
	a := flag.Args()
	if len(a) == 0 {
		fmt.Println("commands: discover | ls DIR | put REMOTE_DIR LOCAL... | get REMOTE_DIR NAME LOCAL_DIR")
		os.Exit(2)
	}
	if a[0] == "discover" {
		for _, f := range client.Discover(1500 * time.Millisecond) {
			fmt.Println(f)
		}
		return
	}
	c, err := client.Connect(*host, *tok, *conns)
	check(err)
	fmt.Fprintf(os.Stderr, "connected to %s, channels: %v\n", c.Info.Name, c.Bases)
	switch a[0] {
	case "ls":
		es, err := c.List(a[1])
		check(err)
		for _, e := range es {
			fmt.Printf("%v\t%12d\t%s\n", e.Dir, e.Size, e.Name)
		}
	case "cat": // cat REMOTE_FILE SIZE  (exercises the streaming reader used by drag-out)
		sz, _ := strconv.ParseInt(a[2], 10, 64)
		st := c.OpenStream(a[1], sz, *conns)
		_, err := io.Copy(os.Stdout, st)
		check(err)
	case "put":
		j, err := c.PlanUpload(a[2:], a[1])
		check(err)
		run(j, *conns)
	case "get":
		es, err := c.List(a[1])
		check(err)
		var sel []proto.Entry
		for _, e := range es {
			if e.Name == a[2] {
				sel = append(sel, e)
			}
		}
		j, err := c.PlanDownload(a[1], sel, a[3])
		check(err)
		run(j, *conns)
	}
}

func run(j *client.Job, n int) {
	done := make(chan error)
	go func() { done <- j.Run(n) }()
	t := time.NewTicker(time.Second)
	for {
		select {
		case err := <-done:
			p := j.Progress
			sec := time.Since(p.Started).Seconds()
			fmt.Printf("\n%d files, %.1f MB in %.2fs = %.1f MB/s\n", p.DoneFiles.Load(), float64(p.DoneBytes.Load())/1e6, sec, float64(p.DoneBytes.Load())/1e6/sec)
			check(err)
			return
		case <-t.C:
			p := j.Progress
			fmt.Printf("\r%d/%d files  %.1f/%.1f MB", p.DoneFiles.Load(), p.TotalFiles, float64(p.DoneBytes.Load())/1e6, float64(p.TotalBytes)/1e6)
		}
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
