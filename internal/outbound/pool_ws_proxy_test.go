package outbound

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// A plain http:// proxy is wired into the dialer via Dialer.Proxy, and
// gorilla/websocket only performs the CONNECT handshake when Proxy is set.
// The pool used to borrow just NetDialContext — which for http proxies is
// still the direct dialer from directClients() — so every WebSocket dial
// bypassed the proxy. Verified 2026-09-29 against a live gateway: with
// http://43.132.180.193:8818 configured and healthy, upstream sockets went
// straight to substrate.office.com (2603:1026::/32) and none to the proxy.

// TestPoolWebSocketDialerHonorsHTTPProxy: an http entry must surface its proxy
// URL through Dialer.Proxy, which is what makes gorilla CONNECT through it.
func TestPoolWebSocketDialerHonorsHTTPProxy(t *testing.T) {
	p, err := NewPool([]string{"http://127.0.0.1:9"})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	d := p.WebSocketDialer()
	if d.Proxy == nil {
		t.Fatal("pool dialer must expose Proxy so gorilla issues CONNECT for http proxies")
	}
	got, err := d.Proxy(&http.Request{})
	if err != nil {
		t.Fatalf("Proxy: %v", err)
	}
	if got == nil {
		t.Fatal("Proxy must return the http proxy URL, not nil (nil = direct)")
	}
	if got.Host != "127.0.0.1:9" {
		t.Fatalf("want proxy host 127.0.0.1:9, got %q", got.Host)
	}
}

// TestPoolWebSocketDialerHonorsSocks5: a socks5 entry tunnels through its own
// NetDialContext and must report NO proxy URL (returning one would make
// gorilla try a second, wrong CONNECT hop).
func TestPoolWebSocketDialerHonorsSocks5(t *testing.T) {
	p, err := NewPool([]string{"socks5://127.0.0.1:1080"})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	entry := p.entries[0]
	if entry.clients.WebSocket.NetDialContext == nil {
		t.Fatal("socks5 entry must tunnel via its own NetDialContext")
	}
	d := p.WebSocketDialer()
	got, err := d.Proxy(&http.Request{})
	if err != nil {
		t.Fatalf("Proxy: %v", err)
	}
	if got != nil {
		t.Fatalf("socks5 tunnels via NetDialContext; Proxy must stay nil, got %q", got)
	}
}

// TestPoolWebSocketDialerHTTPSAlsoTunnels: an https proxy sets NetDialContext
// (see New) and must likewise report no Proxy URL.
func TestPoolWebSocketDialerHTTPSAlsoTunnels(t *testing.T) {
	p, err := NewPool([]string{"https://127.0.0.1:9"})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if p.entries[0].clients.WebSocket.NetDialContext == nil {
		t.Fatal("https entry must tunnel via its own NetDialContext")
	}
	got, _ := p.WebSocketDialer().Proxy(&http.Request{})
	if got != nil {
		t.Fatalf("https tunnels via NetDialContext; Proxy must stay nil, got %q", got)
	}
}

// TestPoolWebSocketDialerEmptyPoolGoesDirect: with no entries the dialer must
// fall back to a direct connection rather than erroring.
func TestPoolWebSocketDialerEmptyPoolGoesDirect(t *testing.T) {
	p, err := NewPool(nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	d := p.WebSocketDialer()
	got, err := d.Proxy(&http.Request{})
	if err != nil || got != nil {
		t.Fatalf("empty pool must report no proxy, got (%v, %v)", got, err)
	}
	// NetDialContext must still exist and attempt a real dial.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := d.NetDialContext(ctx, "tcp", "127.0.0.1:1"); err == nil {
		t.Log("dial to a closed port succeeded (unexpected but not fatal)")
	}
}

// TestPoolWebSocketDialerPreservesCredentials: a proxy with user:pass must
// still be reported to gorilla intact, otherwise the CONNECT is rejected by
// authenticating proxies.
func TestPoolWebSocketDialerPreservesCredentials(t *testing.T) {
	p, err := NewPool([]string{"http://user:pw@proxy.example.com:8080"})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	got, err := p.WebSocketDialer().Proxy(&http.Request{})
	if err != nil {
		t.Fatalf("Proxy: %v", err)
	}
	if got == nil {
		t.Fatal("credentialed http proxy must be reported, not dropped to direct")
	}
	if got.Host != "proxy.example.com:8080" {
		t.Fatalf("want proxy.example.com:8080, got %q", got.Host)
	}
	if got.User == nil || got.User.Username() != "user" {
		t.Fatalf("credentials must survive, got %v", got.User)
	}
	if _, err := url.Parse(p.entries[0].raw); err != nil {
		t.Fatalf("raw must stay parseable: %v", err)
	}
}
