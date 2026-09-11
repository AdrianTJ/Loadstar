package main

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServeStartup(t *testing.T) {
	// Simple smoke test to see if the server starts.
	// Bind and dial an explicit IPv4 address so the client and server can't end
	// up on different address families (localhost may resolve to ::1).
	const addr = "127.0.0.1:9090"
	t.Setenv("LOADSTAR_LISTEN_ADDR", addr)
	t.Setenv("DATABASE_URL", ":memory:") // Use in-memory SQLite for testing

	go func() {
		serveCmd([]string{"-insecure"})
	}()

	// Poll the health endpoint until the server is listening, rather than
	// racing a fixed sleep against startup.
	client := &http.Client{Timeout: 1 * time.Second}
	healthURL := "http://" + addr + "/v1/health"

	var resp *http.Response
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = client.Get(healthURL)
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Failed to connect to loadstar serve after startup window: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected health check 200 from server, got status: %d", resp.StatusCode)
	}
}

// TestNewHTTPServer_Timeouts is the regression for the July 2026 audit finding
// SEC-3. The daemon previously used a bare &http.Server{Addr, Handler}, and
// net/http applies no timeouts by default, so a stalled-header connection was
// held open indefinitely (Slowloris).
func TestNewHTTPServer_Timeouts(t *testing.T) {
	srv := newHTTPServer(":8080", http.NewServeMux())

	for _, tc := range []struct {
		name string
		got  time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout},
		{"ReadTimeout", srv.ReadTimeout},
		{"WriteTimeout", srv.WriteTimeout},
		{"IdleTimeout", srv.IdleTimeout},
	} {
		if tc.got <= 0 {
			t.Errorf("%s = %v, want a positive timeout", tc.name, tc.got)
		}
	}

	// A read deadline that fires before headers are complete would cut off
	// legitimate slow clients, so the ordering matters as much as presence.
	if srv.ReadHeaderTimeout > srv.ReadTimeout {
		t.Errorf("ReadHeaderTimeout (%v) must not exceed ReadTimeout (%v)",
			srv.ReadHeaderTimeout, srv.ReadTimeout)
	}
}

// TestServer_ClosesStalledHeaderConnection drives the actual Slowloris case
// over a real socket: send a request line and a partial header, then never
// finish. The server must hang up on its own.
func TestServer_ClosesStalledHeaderConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := newHTTPServer(ln.Addr().String(), http.NewServeMux())
	// Shrink the header deadline so the test does not sit for 10s.
	srv.ReadHeaderTimeout = 300 * time.Millisecond
	go srv.Serve(ln)
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "GET /v1/health HTTP/1.1\r\nHost: x\r\nX-Slow: "); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Generous relative to the 300ms deadline, but far below "forever".
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(conn); err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("server held the stalled connection open; Slowloris is possible")
		}
		// Any other error means the peer went away, which is the goal.
	}
}
