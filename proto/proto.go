// Package proto holds the types shared by the phone daemon and the PC client.
package proto

const (
	Version       = "1.2.0"
	DefaultPort   = 5740 // HTTP API
	DiscoveryPort = 5741 // UDP discovery
	DiscoverMagic = "LANXFER_DISCOVER_V1"
	TokenHeader   = "X-Lanxfer-Token"
)

// Entry is one item in a directory listing or a recursive walk.
type Entry struct {
	Name  string `json:"n"`           // base name (list) or slash-separated relative path (walk)
	Size  int64  `json:"s"`           // bytes
	MTime int64  `json:"m"`           // unix milliseconds
	Dir   bool   `json:"d,omitempty"` // is directory
}

// Info describes the phone daemon.
type Info struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Port    int      `json:"port"`
	Addrs   []string `json:"addrs"` // all IPv4 addresses of the phone (one per NIC = one channel)
	Home    string   `json:"home"`  // suggested start directory
	Raw     bool     `json:"raw"`   // true if /sdcard is served from /data/media/0 directly (bypassing FUSE)
}

// DiscoverReply is the UDP answer to a discovery probe.
type DiscoverReply struct {
	Magic string `json:"magic"`
	Name  string `json:"name"`
	Port  int    `json:"port"`
}
