package client

import (
	"strings"
	"testing"
)

func TestPhoneName(t *testing.T) {
	long := strings.Repeat("中文名字", 30) + ".mp4" // 360+ bytes
	n, cut := PhoneName(long)
	if !cut || len(n) > PhoneNameMax || !strings.HasSuffix(n, ".mp4") {
		t.Fatalf("bad %q %d", n, len(n))
	}
	n2, _ := PhoneName(strings.Repeat("中文名字", 30) + "x.mp4")
	if n == n2 {
		t.Fatal("collision")
	}
	if s, c := PhoneName("short.txt"); c || s != "short.txt" {
		t.Fatal("short changed")
	}
	if !strings.HasPrefix(n, "中文名字") {
		t.Fatal("prefix lost")
	}
}

func TestDragNamer(t *testing.T) {
	d := NewDragNamer()
	dir := strings.Repeat("目录", 80)
	f := strings.Repeat("文件", 80) + ".jpg"
	a := d.Name(dir, true)
	b := d.Name(dir+"/sub/"+f, false)
	c := d.Name(dir+"/sub/"+f+"2.png", false)
	if !strings.HasPrefix(b, a+`\`) || !strings.HasPrefix(c, a+`\sub\`) {
		t.Fatalf("inconsistent dirs:\n%s\n%s\n%s", a, b, c)
	}
	if utf16Len(b) > WinDragBudget || !strings.HasSuffix(b, ".jpg") {
		t.Fatalf("too long %d %s", utf16Len(b), b)
	}
	if n, _ := WinName("we:ird?.txt"); n != "we：ird？.txt" {
		t.Fatal(n)
	}
}
