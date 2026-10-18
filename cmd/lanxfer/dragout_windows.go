//go:build windows

// Drag-out of phone files into Explorer, implemented as "virtual files":
// an OLE IDataObject that offers FileGroupDescriptorW + FileContents.
// Nothing is downloaded until the user actually drops; Explorer then pulls
// each file through an IStream, which we feed from the phone with parallel
// read-ahead. Pure Go COM (vtables built with syscall.NewCallback).
package main

import (
	"encoding/binary"
	"io"
	"path"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"lanxfer/client"
)

var (
	ole32    = syscall.NewLazyDLL("ole32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	user32   = syscall.NewLazyDLL("user32.dll")
	comctl32 = syscall.NewLazyDLL("comctl32.dll")

	procDoDragDrop            = ole32.NewProc("DoDragDrop")
	procCoTaskMemAlloc        = ole32.NewProc("CoTaskMemAlloc")
	procSHCreateStdEnumFmtEtc = shell32.NewProc("SHCreateStdEnumFmtEtc")
	procGlobalAlloc           = kernel32.NewProc("GlobalAlloc")
	procGlobalLock            = kernel32.NewProc("GlobalLock")
	procGlobalUnlock          = kernel32.NewProc("GlobalUnlock")
	procRegisterClipboardFmt  = user32.NewProc("RegisterClipboardFormatW")
	procSetWindowSubclass     = comctl32.NewProc("SetWindowSubclass")
	procDefSubclassProc       = comctl32.NewProc("DefSubclassProc")
)

const (
	S_OK                  = 0
	S_FALSE               = 1
	E_NOINTERFACE         = 0x80004002
	E_NOTIMPL             = 0x80004001
	E_FAIL                = 0x80004005
	E_INVALIDARG          = 0x80070057
	DV_E_FORMATETC        = 0x80040064
	DV_E_TYMED            = 0x80040069
	STG_E_INVALIDFUNCTION = 0x80030001

	DRAGDROP_S_DROP              = 0x00040100
	DRAGDROP_S_CANCEL            = 0x00040101
	DRAGDROP_S_USEDEFAULTCURSORS = 0x00040102
	DROPEFFECT_COPY              = 1
	MK_LBUTTON                   = 1

	TYMED_HGLOBAL    = 1
	TYMED_ISTREAM    = 4
	DVASPECT_CONTENT = 1

	GMEM_MOVEABLE = 0x2
	GMEM_ZEROINIT = 0x40

	FD_ATTRIBUTES            = 0x4
	FD_WRITESTIME            = 0x20
	FD_FILESIZE              = 0x40
	FD_PROGRESSUI            = 0x4000
	FD_UNICODE               = 0x80000000
	FILE_ATTRIBUTE_DIRECTORY = 0x10
	FILE_ATTRIBUTE_NORMAL    = 0x80
)

type GUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	IID_IUnknown          = GUID{0x00000000, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	IID_IDataObject       = GUID{0x0000010e, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	IID_IDropSource       = GUID{0x00000121, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	IID_IStream           = GUID{0x0000000c, 0, 0, [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	IID_ISequentialStream = GUID{0x0c733a30, 0x2a1c, 0x11ce, [8]byte{0xad, 0xe5, 0x00, 0xaa, 0x00, 0x44, 0x77, 0x3d}}
)

type FORMATETC struct {
	CfFormat uint16
	_        [6]byte
	Ptd      uintptr
	DwAspect uint32
	Lindex   int32
	Tymed    uint32
	_        uint32
}

type STGMEDIUM struct {
	Tymed          uint32
	_              uint32
	Union          uintptr
	PUnkForRelease uintptr
}

var cfFileDescriptor, cfFileContents uint16

func regFormat(name string) uint16 {
	p, _ := syscall.UTF16PtrFromString(name)
	r, _, _ := procRegisterClipboardFmt.Call(uintptr(unsafe.Pointer(p)))
	return uint16(r)
}

// ---------- generic COM object plumbing ----------

// comObj must be the first field of every object so that `this` points to it.
type comObj struct {
	vtbl *[16]uintptr
	refs int32
}

// live keeps Go objects reachable while COM holds references to them.
var (
	liveMu sync.Mutex
	live   = map[uintptr]any{}
)

func keep(p uintptr, o any) { liveMu.Lock(); live[p] = o; liveMu.Unlock() }
func drop(p uintptr)        { liveMu.Lock(); delete(live, p); liveMu.Unlock() }

func lookup(this uintptr) any {
	liveMu.Lock()
	defer liveMu.Unlock()
	return live[this]
}

func guidEq(p uintptr, g *GUID) bool { return *(*GUID)(unsafe.Pointer(p)) == *g }

func addRef(this uintptr) uintptr {
	o := (*comObj)(unsafe.Pointer(this))
	return uintptr(atomic.AddInt32(&o.refs, 1))
}

func release(this uintptr) uintptr {
	o := (*comObj)(unsafe.Pointer(this))
	n := atomic.AddInt32(&o.refs, -1)
	if n == 0 {
		if c, ok := lookup(this).(interface{ onFree() }); ok {
			c.onFree()
		}
		drop(this)
	}
	return uintptr(n)
}

func qi(this, riid, ppv uintptr, iids ...*GUID) uintptr {
	for _, g := range iids {
		if guidEq(riid, g) {
			*(*uintptr)(unsafe.Pointer(ppv)) = this
			addRef(this)
			return S_OK
		}
	}
	*(*uintptr)(unsafe.Pointer(ppv)) = 0
	return E_NOINTERFACE
}

func notImpl(this uintptr) uintptr { return E_NOTIMPL }

// ---------- IDropSource ----------

type dropSource struct {
	comObj
	// decide is called when the mouse button is released: true lets OLE
	// deliver the drop to the target, false cancels it (we copy ourselves).
	decide func() bool
}

var dropSourceVtbl [16]uintptr

func newDropSource(decide func() bool) *dropSource {
	d := &dropSource{comObj: comObj{vtbl: &dropSourceVtbl, refs: 1}, decide: decide}
	keep(uintptr(unsafe.Pointer(d)), d)
	return d
}

// ---------- IDataObject ----------

// dragItem is one entry offered to Explorer. Name is a relative path with
// backslashes ("Camera\\IMG_1.jpg") so folders are recreated.
type dragItem struct {
	Name   string
	Remote string
	Size   int64
	MTime  int64
	Dir    bool
}

type dataObject struct {
	comObj
	items []dragItem
	c     *client.Client
	conns int
	prog  *dragProgress
}

var dataObjectVtbl [16]uintptr

func newDataObject(c *client.Client, items []dragItem, conns int, prog *dragProgress) *dataObject {
	d := &dataObject{comObj: comObj{vtbl: &dataObjectVtbl, refs: 1}, items: items, c: c, conns: conns, prog: prog}
	keep(uintptr(unsafe.Pointer(d)), d)
	return d
}

func (d *dataObject) supports(fe *FORMATETC) uintptr {
	if fe.DwAspect != DVASPECT_CONTENT {
		return DV_E_FORMATETC
	}
	switch fe.CfFormat {
	case cfFileDescriptor:
		if fe.Tymed&TYMED_HGLOBAL == 0 {
			return DV_E_TYMED
		}
		return S_OK
	case cfFileContents:
		if fe.Tymed&TYMED_ISTREAM == 0 {
			return DV_E_TYMED
		}
		return S_OK
	}
	return DV_E_FORMATETC
}

// fileGroupDescriptor builds FILEGROUPDESCRIPTORW in an HGLOBAL.
func (d *dataObject) fileGroupDescriptor() uintptr {
	const fdSize = 592
	buf := make([]byte, 4+fdSize*len(d.items))
	binary.LittleEndian.PutUint32(buf, uint32(len(d.items)))
	for i, it := range d.items {
		b := buf[4+i*fdSize:]
		flags := uint32(FD_ATTRIBUTES | FD_WRITESTIME | FD_PROGRESSUI | FD_UNICODE)
		attr := uint32(FILE_ATTRIBUTE_NORMAL)
		if it.Dir {
			attr = FILE_ATTRIBUTE_DIRECTORY
		} else {
			flags |= FD_FILESIZE
		}
		binary.LittleEndian.PutUint32(b[0:], flags)
		// clsid(16) sizel(8) pointl(8) left zero -> offset 36
		binary.LittleEndian.PutUint32(b[36:], attr)
		// creation 40, access 48, write 56
		ft := unixMsToFiletime(it.MTime)
		binary.LittleEndian.PutUint64(b[56:], ft)
		binary.LittleEndian.PutUint32(b[64:], uint32(uint64(it.Size)>>32))
		binary.LittleEndian.PutUint32(b[68:], uint32(it.Size))
		name := utf16.Encode([]rune(it.Name))
		if len(name) > 259 {
			name = name[:259]
		}
		for k, ch := range name {
			binary.LittleEndian.PutUint16(b[72+2*k:], ch)
		}
	}
	h, _, _ := procGlobalAlloc.Call(GMEM_MOVEABLE|GMEM_ZEROINIT, uintptr(len(buf)))
	if h == 0 {
		return 0
	}
	p, _, _ := procGlobalLock.Call(h)
	copy(unsafe.Slice((*byte)(unsafe.Pointer(p)), len(buf)), buf)
	procGlobalUnlock.Call(h)
	return h
}

func unixMsToFiletime(ms int64) uint64 {
	if ms <= 0 {
		ms = time.Now().UnixMilli()
	}
	return uint64(ms)*10000 + 116444736000000000
}

// ---------- IStream ----------

type stream struct {
	comObj
	s    *client.Stream
	name string
	size int64
	mt   int64
}

var streamVtbl [16]uintptr

func newStream(s *client.Stream, name string, size, mt int64) *stream {
	st := &stream{comObj: comObj{vtbl: &streamVtbl, refs: 1}, s: s, name: name, size: size, mt: mt}
	keep(uintptr(unsafe.Pointer(st)), st)
	return st
}

func (st *stream) onFree() { st.s.Close() }

// ---------- progress shared with the GUI ----------

type dragProgress struct {
	total atomic.Int64
	done  atomic.Int64
	files atomic.Int64
	start time.Time
}

// ---------- vtables ----------

var vtblOnce sync.Once

func initVtables() {
	vtblOnce.Do(func() {
		cfFileDescriptor = regFormat("FileGroupDescriptorW")
		cfFileContents = regFormat("FileContents")

		// IDropSource: QI, AddRef, Release, QueryContinueDrag, GiveFeedback
		dropSourceVtbl[0] = syscall.NewCallback(func(this, riid, ppv uintptr) uintptr {
			return qi(this, riid, ppv, &IID_IUnknown, &IID_IDropSource)
		})
		dropSourceVtbl[1] = syscall.NewCallback(addRef)
		dropSourceVtbl[2] = syscall.NewCallback(release)
		dropSourceVtbl[3] = syscall.NewCallback(func(this, esc, keys uintptr) uintptr {
			if esc != 0 {
				return DRAGDROP_S_CANCEL
			}
			if keys&MK_LBUTTON == 0 {
				if ds, ok := lookup(this).(*dropSource); ok && ds.decide != nil && !ds.decide() {
					return DRAGDROP_S_CANCEL
				}
				return DRAGDROP_S_DROP
			}
			return S_OK
		})
		dropSourceVtbl[4] = syscall.NewCallback(func(this, effect uintptr) uintptr { return DRAGDROP_S_USEDEFAULTCURSORS })

		// IDataObject
		dataObjectVtbl[0] = syscall.NewCallback(func(this, riid, ppv uintptr) uintptr {
			return qi(this, riid, ppv, &IID_IUnknown, &IID_IDataObject)
		})
		dataObjectVtbl[1] = syscall.NewCallback(addRef)
		dataObjectVtbl[2] = syscall.NewCallback(release)
		dataObjectVtbl[3] = syscall.NewCallback(func(this, pfe, pmed uintptr) uintptr { // GetData
			d, ok := lookup(this).(*dataObject)
			if !ok {
				return E_FAIL
			}
			fe := (*FORMATETC)(unsafe.Pointer(pfe))
			med := (*STGMEDIUM)(unsafe.Pointer(pmed))
			if hr := d.supports(fe); hr != S_OK {
				return hr
			}
			switch fe.CfFormat {
			case cfFileDescriptor:
				h := d.fileGroupDescriptor()
				if h == 0 {
					return E_FAIL
				}
				med.Tymed, med.Union, med.PUnkForRelease = TYMED_HGLOBAL, h, 0
				return S_OK
			case cfFileContents:
				i := int(fe.Lindex)
				if i < 0 || i >= len(d.items) || d.items[i].Dir {
					return DV_E_FORMATETC
				}
				it := d.items[i]
				cs := d.c.OpenStream(it.Remote, it.Size, d.conns)
				cs.OnBytes = func(n int64) { d.prog.done.Add(n) }
				st := newStream(cs, path.Base(it.Remote), it.Size, it.MTime)
				d.prog.files.Add(1)
				med.Tymed, med.Union, med.PUnkForRelease = TYMED_ISTREAM, uintptr(unsafe.Pointer(st)), 0
				return S_OK
			}
			return DV_E_FORMATETC
		})
		dataObjectVtbl[4] = syscall.NewCallback(func(this, pfe, pmed uintptr) uintptr { return E_NOTIMPL }) // GetDataHere
		dataObjectVtbl[5] = syscall.NewCallback(func(this, pfe uintptr) uintptr {                           // QueryGetData
			d, ok := lookup(this).(*dataObject)
			if !ok {
				return E_FAIL
			}
			return d.supports((*FORMATETC)(unsafe.Pointer(pfe)))
		})
		dataObjectVtbl[6] = syscall.NewCallback(func(this, in, out uintptr) uintptr { // GetCanonicalFormatEtc
			if out != 0 {
				(*FORMATETC)(unsafe.Pointer(out)).Ptd = 0
			}
			return E_NOTIMPL
		})
		dataObjectVtbl[7] = syscall.NewCallback(func(this, pfe, pmed, rel uintptr) uintptr { return E_NOTIMPL }) // SetData
		dataObjectVtbl[8] = syscall.NewCallback(func(this, dir, ppenum uintptr) uintptr {                        // EnumFormatEtc
			if dir != 1 { // DATADIR_GET
				return E_NOTIMPL
			}
			fmts := []FORMATETC{
				{CfFormat: cfFileDescriptor, DwAspect: DVASPECT_CONTENT, Lindex: -1, Tymed: TYMED_HGLOBAL},
				{CfFormat: cfFileContents, DwAspect: DVASPECT_CONTENT, Lindex: -1, Tymed: TYMED_ISTREAM},
			}
			r, _, _ := procSHCreateStdEnumFmtEtc.Call(uintptr(len(fmts)), uintptr(unsafe.Pointer(&fmts[0])), ppenum)
			return r
		})
		dataObjectVtbl[9] = syscall.NewCallback(func(this, a, b, c, d uintptr) uintptr { return 0x80040003 }) // DAdvise: OLE_E_ADVISENOTSUPPORTED
		dataObjectVtbl[10] = syscall.NewCallback(func(this, a uintptr) uintptr { return 0x80040003 })
		dataObjectVtbl[11] = syscall.NewCallback(func(this, a uintptr) uintptr { return 0x80040003 })

		// IStream: QI AddRef Release Read Write Seek SetSize CopyTo Commit Revert LockRegion UnlockRegion Stat Clone
		streamVtbl[0] = syscall.NewCallback(func(this, riid, ppv uintptr) uintptr {
			return qi(this, riid, ppv, &IID_IUnknown, &IID_ISequentialStream, &IID_IStream)
		})
		streamVtbl[1] = syscall.NewCallback(addRef)
		streamVtbl[2] = syscall.NewCallback(release)
		streamVtbl[3] = syscall.NewCallback(func(this, pv, cb, pcbRead uintptr) uintptr { // Read
			st, ok := lookup(this).(*stream)
			if !ok {
				return E_FAIL
			}
			buf := unsafe.Slice((*byte)(unsafe.Pointer(pv)), int(cb))
			total := 0
			var err error
			for total < len(buf) {
				var n int
				n, err = st.s.Read(buf[total:])
				total += n
				if err != nil {
					break
				}
			}
			if pcbRead != 0 {
				*(*uint32)(unsafe.Pointer(pcbRead)) = uint32(total)
			}
			if err != nil && err != io.EOF {
				return E_FAIL
			}
			if total < len(buf) {
				return S_FALSE
			}
			return S_OK
		})
		streamVtbl[4] = syscall.NewCallback(func(this, pv, cb, pcb uintptr) uintptr { return STG_E_INVALIDFUNCTION }) // Write
		streamVtbl[5] = syscall.NewCallback(func(this, move, origin, newPos uintptr) uintptr {                        // Seek (LARGE_INTEGER by value = 64-bit arg on amd64)
			st, ok := lookup(this).(*stream)
			if !ok {
				return E_FAIL
			}
			p, err := st.s.Seek(int64(move), int(origin))
			if err != nil {
				return E_INVALIDARG
			}
			if newPos != 0 {
				*(*uint64)(unsafe.Pointer(newPos)) = uint64(p)
			}
			return S_OK
		})
		streamVtbl[6] = syscall.NewCallback(func(this, size uintptr) uintptr { return E_NOTIMPL })
		streamVtbl[7] = syscall.NewCallback(func(this, pstm, cb, pr, pw uintptr) uintptr { return E_NOTIMPL })
		streamVtbl[8] = syscall.NewCallback(func(this, flags uintptr) uintptr { return S_OK })
		streamVtbl[9] = syscall.NewCallback(func(this uintptr) uintptr { return S_OK })
		streamVtbl[10] = syscall.NewCallback(func(this, a, b, c uintptr) uintptr { return 0x80030100 }) // STG_E_INVALIDFUNCTION-ish: lock not supported
		streamVtbl[11] = syscall.NewCallback(func(this, a, b, c uintptr) uintptr { return 0x80030100 })
		streamVtbl[12] = syscall.NewCallback(func(this, pstat, flag uintptr) uintptr { // Stat
			st, ok := lookup(this).(*stream)
			if !ok {
				return E_FAIL
			}
			b := unsafe.Slice((*byte)(unsafe.Pointer(pstat)), 80)
			for i := range b {
				b[i] = 0
			}
			if flag&1 == 0 { // STATFLAG_DEFAULT: name wanted, CoTaskMemAlloc'ed
				u := utf16.Encode([]rune(st.name + "\x00"))
				m, _, _ := procCoTaskMemAlloc.Call(uintptr(len(u) * 2))
				if m != 0 {
					copy(unsafe.Slice((*uint16)(unsafe.Pointer(m)), len(u)), u)
					*(*uintptr)(unsafe.Pointer(&b[0])) = m
				}
			}
			binary.LittleEndian.PutUint32(b[8:], 2) // STGTY_STREAM
			binary.LittleEndian.PutUint64(b[16:], uint64(st.size))
			binary.LittleEndian.PutUint64(b[24:], unixMsToFiletime(st.mt)) // mtime
			return S_OK
		})
		streamVtbl[13] = syscall.NewCallback(func(this, pp uintptr) uintptr { return E_NOTIMPL })
	})
}

// startDragOut runs the modal OLE drag loop. Must be called on the UI thread.
// startDragOut runs the OLE drag loop (blocking). It returns true when the
// drop was delivered to the target through OLE (virtual files).
func startDragOut(c *client.Client, items []dragItem, conns int, prog *dragProgress, decide func() bool) bool {
	initVtables()
	do := newDataObject(c, items, conns, prog)
	ds := newDropSource(decide)
	var effect uint32
	r, _, _ := procDoDragDrop.Call(uintptr(unsafe.Pointer(do)), uintptr(unsafe.Pointer(ds)), DROPEFFECT_COPY, uintptr(unsafe.Pointer(&effect)))
	release(uintptr(unsafe.Pointer(ds)))
	release(uintptr(unsafe.Pointer(do)))
	return r == DRAGDROP_S_DROP && effect != 0
}
