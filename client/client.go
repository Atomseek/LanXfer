// Package client talks to lanxferd and runs parallel multi-channel transfers.
// It is platform independent; the Windows GUI is a thin layer on top.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"lanxfer/proto"
)

var ErrBadToken = errors.New("令牌错误")

// Client is a connection to one phone. It may hold several base URLs, one
// per reachable phone NIC; transfer workers are spread across them.
type Client struct {
	Token string
	Info  proto.Info
	Bases []string // e.g. http://192.168.1.5:5740
	http  *http.Client
	ctl   *http.Client
}

func newTransport(conns int) *http.Transport {
	d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := d.DialContext(ctx, network, addr)
			if tc, ok := c.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
				tc.SetReadBuffer(4 << 20)
				tc.SetWriteBuffer(4 << 20)
			}
			return c, err
		},
		MaxIdleConns:        conns * 4,
		MaxIdleConnsPerHost: conns,
		MaxConnsPerHost:     conns,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
		WriteBufferSize:     256 << 10,
		ReadBufferSize:      256 << 10,
	}
}

// Connect contacts host (ip or ip:port), verifies the token and probes every
// address the phone advertises, keeping the reachable ones as channels.
func Connect(host, token string, conns int) (*Client, error) {
	if conns < 1 {
		conns = 4
	}
	if !strings.Contains(host, ":") {
		host = fmt.Sprintf("%s:%d", host, proto.DefaultPort)
	}
	c := &Client{
		Token: token,
		http:  &http.Client{Transport: newTransport(conns)},
		ctl:   &http.Client{Transport: newTransport(4), Timeout: 30 * time.Second},
	}
	first := "http://" + host
	if err := c.getJSON(first, "/api/info", nil, &c.Info); err != nil {
		return nil, err
	}
	c.Bases = []string{first}
	firstIP, _, _ := net.SplitHostPort(host)
	// probe other NICs of the phone in parallel
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, a := range c.Info.Addrs {
		if a == firstIP {
			continue
		}
		wg.Add(1)
		go func(a string) {
			defer wg.Done()
			base := fmt.Sprintf("http://%s:%d", a, c.Info.Port)
			pc := &http.Client{Timeout: 800 * time.Millisecond}
			resp, err := pc.Get(base + "/api/ping")
			if err != nil {
				return
			}
			resp.Body.Close()
			if resp.StatusCode == 200 {
				mu.Lock()
				c.Bases = append(c.Bases, base)
				mu.Unlock()
			}
		}(a)
	}
	wg.Wait()
	return c, nil
}

func (c *Client) req(ctx context.Context, hc *http.Client, method, base, ep string, params url.Values, body io.Reader, size int64) (*http.Response, error) {
	u := base + ep
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	r, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		r.ContentLength = size
	}
	r.Header.Set(proto.TokenHeader, c.Token)
	resp, err := hc.Do(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, ErrBadToken
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

func (c *Client) getJSON(base, ep string, params url.Values, v any) error {
	resp, err := c.req(context.Background(), c.ctlClient(), "GET", base, ep, params, nil, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

func (c *Client) ctlClient() *http.Client {
	if c.ctl == nil {
		c.ctl = &http.Client{Timeout: 30 * time.Second}
	}
	return c.ctl
}

func (c *Client) post(ep string, params url.Values) error {
	resp, err := c.req(context.Background(), c.ctlClient(), "POST", c.base(0), ep, params, nil, 0)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func P(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

// ---------- file operations ----------

func (c *Client) List(p string) ([]proto.Entry, error) {
	var out []proto.Entry
	err := c.getJSON(c.base(0), "/api/list", P("p", p), &out)
	return out, err
}

func (c *Client) Walk(p string) ([]proto.Entry, error) {
	var out []proto.Entry
	err := c.getJSON(c.base(0), "/api/walk", P("p", p), &out)
	return out, err
}

func (c *Client) Mkdir(p string) error  { return c.post("/api/mkdir", P("p", p)) }
func (c *Client) Delete(p string) error { return c.post("/api/delete", P("p", p)) }
func (c *Client) Rename(p, to string) error {
	return c.post("/api/rename", P("p", p, "to", to))
}

// Discover broadcasts a probe on every IPv4 LAN interface and collects replies.
func Discover(wait time.Duration) []Found {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil
	}
	defer conn.Close()
	targets := map[string]bool{"255.255.255.255": true}
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagBroadcast == 0 {
			continue
		}
		as, _ := i.Addrs()
		for _, a := range as {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			ip, m := ipn.IP.To4(), ipn.Mask
			if len(m) == 16 {
				m = m[12:]
			}
			b := make(net.IP, 4)
			for k := 0; k < 4; k++ {
				b[k] = ip[k] | ^m[k]
			}
			targets[b.String()] = true
		}
	}
	for t := range targets {
		conn.WriteToUDP([]byte(proto.DiscoverMagic), &net.UDPAddr{IP: net.ParseIP(t), Port: proto.DiscoveryPort})
	}
	seen := map[string]bool{}
	var out []Found
	buf := make([]byte, 1024)
	deadline := time.Now().Add(wait)
	for {
		conn.SetReadDeadline(deadline)
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		var r proto.DiscoverReply
		if json.Unmarshal(buf[:n], &r) != nil || r.Magic != proto.DiscoverMagic {
			continue
		}
		key := from.IP.String() + ":" + strconv.Itoa(r.Port)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Found{Name: r.Name, Addr: key})
	}
	return out
}

type Found struct {
	Name string
	Addr string // ip:port
}

func (f Found) String() string { return f.Name + "  " + f.Addr }

// base returns the i-th server base URL (round-robin). It never panics on an
// empty list; requests to "" simply fail with a clear error.
func (c *Client) base(i int) string {
	if c == nil || len(c.Bases) == 0 {
		return "http://not-connected.invalid"
	}
	if i < 0 {
		i = -i
	}
	return c.Bases[i%len(c.Bases)]
}

// Ping checks that the phone still answers (token-checked) within timeout.
func (c *Client) Ping(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := c.req(ctx, c.ctlClient(), "GET", c.base(0), "/api/info", nil, nil, 0)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Host returns ip:port of the primary channel.
func (c *Client) Host() string { return strings.TrimPrefix(c.base(0), "http://") }

// CloseIdle drops pooled connections (used when a client is replaced).
func (c *Client) CloseIdle() {
	if c.http != nil {
		c.http.CloseIdleConnections()
	}
	if c.ctl != nil {
		c.ctl.CloseIdleConnections()
	}
}
