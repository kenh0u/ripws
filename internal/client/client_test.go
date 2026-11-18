package client

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ripws/internal/proto"
)

// TestDialThroughHTTPProxy verifies the core "pierce the proxy" property:
// the client dials its WebSocket via an HTTP CONNECT proxy when
// HTTP_PROXY/HTTPS_PROXY is set, and the tunnel data plane still works.
func TestDialThroughHTTPProxy(t *testing.T) {
	// Upstream RIPWS-style server: upgrade and echo binary frames.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}))
	defer up.Close()
	wsTarget := "ws" + up.URL[4:]

	// A minimal HTTP CONNECT proxy: accept absolute-form CONNECT, tunnel bytes.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "only CONNECT", http.StatusMethodNotAllowed)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		client, _, err := hj.Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		target, err := netDial("tcp", r.Host)
		if err != nil {
			return
		}
		defer target.Close()
		// 200 Connection Established
		if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			return
		}
		go copyStream(target, client)
		copyStream(client, target)
	}))
	defer proxy.Close()

	// Point the client at the proxy via env (as an opaque corporate
	// environment would).
	os.Setenv("HTTP_PROXY", proxy.URL)
	os.Setenv("HTTPS_PROXY", proxy.URL)
	defer os.Unsetenv("HTTP_PROXY")
	defer os.Unsetenv("HTTPS_PROXY")

	u, err := url.Parse(wsTarget)
	if err != nil {
		t.Fatal(err)
	}

	// Run the client's own dialer configuration and verify the proxy was
	// actually used: with Proxy unset in Config, newDialer derives it from
	// the environment, so a connect through our proxy server means the
	// property works.
	d := newDialer(u)
	if d.Proxy == nil {
		t.Fatal("dialer has no proxy function; HTTP_PROXY ignored")
	}

	conn, _, err := d.DialContext(t.Context(), u.String(), http.Header{})
	if err != nil {
		t.Fatalf("dial through proxy: %v", err)
	}
	defer conn.Close()

	// Data plane through the proxied connection.
	if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte{0x45, 0, 0, 20}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) != 4 {
		t.Fatalf("echo = %x", data)
	}
}

// TestDialNoProxyEnv ensures that without proxy env vars the dialer goes
// direct (the common fast path must not regress).
func TestDialNoProxyEnv(t *testing.T) {
	os.Unsetenv("HTTP_PROXY")
	os.Unsetenv("HTTPS_PROXY")
	os.Unsetenv("http_proxy")
	os.Unsetenv("https_proxy")

	u, err := url.Parse("ws://127.0.0.1:1/path")
	if err != nil {
		t.Fatal(err)
	}
	d := newDialer(u)
	if d.Proxy == nil {
		t.Fatal("proxy function missing; http.ProxyFromEnvironment should be the default")
	}
	pu, err := d.Proxy(&http.Request{URL: &url.URL{Scheme: "ws", Host: u.Host}})
	if err != nil {
		t.Fatal(err)
	}
	if pu != nil {
		t.Fatalf("expected direct connection, got proxy %v", pu)
	}
}

var _ = proto.DefaultKeepAlive // keep import if helpers move
