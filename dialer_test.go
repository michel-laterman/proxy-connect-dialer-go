package dialer

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewProxyConnectDialer(t *testing.T) {
	u, err := url.Parse("http://localhost:8080")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	dialer, err := NewProxyConnectDialer(u, &net.Dialer{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if dialer == nil {
		t.Error("Dialer is nil")
	}
}

func TestNewProxyConnectDialerWithOptions(t *testing.T) {
	u, err := url.Parse("https://localhost:8080")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	headers := http.Header{}
	headers.Add("test-header", "test-value")

	dialer, err := NewProxyConnectDialer(u, &net.Dialer{}, WithProxyConnectHeaders(headers))
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if dialer == nil {
		t.Fatal("Dialer is nil")
	}
	pcd := dialer.(*proxyConnectDialer)
	if pcd.options.proxyConnect == nil {
		t.Fatal("ProxyConnect headers is nil")
	}
}

type proxyHandlerOptions struct {
	// connected is used by the test proxy server to signal it recieved a CONNECT request; this is required to be not-nil.
	connected *atomic.Bool
	// errorStatus will signal the test proxy to immeidatly return the passed status code once the CONNECT request has been recieved.
	errorStatus int
	// authValue if set will be used to check the Proxy-Authorization header.
	authValue string
}

func genProxyHandler(t *testing.T, options proxyHandlerOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		options.connected.Store(true)
		if options.errorStatus > 0 {
			w.WriteHeader(options.errorStatus)
			return
		}
		if options.authValue != "" {
			if auth := req.Header.Get("Proxy-Authorization"); auth != options.authValue {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}

		targetConn, err := net.DialTimeout("tcp", req.Host, 10*time.Second)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer targetConn.Close()

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		clientConn, _, err := hijacker.Hijack()
		if err != nil {
			t.Logf("Hijack error: %v", err)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		clientConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		defer clientConn.Close()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := io.Copy(targetConn, clientConn)
			if err != nil {
				t.Errorf("Proxy encounted an error copying to destination: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			_, err := io.Copy(clientConn, targetConn)
			if err != nil {
				t.Errorf("Proxy encounted an error copying to client: %v", err)
			}
		}()
		wg.Wait()
	})
}

func TestDial(t *testing.T) {
	t.Run("http proxy", func(t *testing.T) {
		var serverResponded atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method == http.MethodGet {
				serverResponded.Store(true)
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`ok`))
				return
			}
			w.WriteHeader(http.StatusMethodNotAllowed)
			w.Write([]byte(`error`))
		}))
		defer srv.Close()

		var proxyConnected atomic.Bool
		proxyServer := httptest.NewServer(genProxyHandler(t, proxyHandlerOptions{connected: &proxyConnected}))
		defer proxyServer.Close()
		t.Logf("Server URL: %s", srv.URL)
		t.Logf("Proxy URL: %s", proxyServer.URL)

		// Create HTTP client that uses custom dialer
		pURL, err := url.Parse(proxyServer.URL)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		dialer, err := NewProxyConnectDialer(pURL, &net.Dialer{})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		client := &http.Client{
			Transport: &http.Transport{
				Dial: dialer.Dial,
			},
		}

		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected response status code 200, got: %d", resp.StatusCode)
		}
		if !proxyConnected.Load() {
			t.Error("Expected proxy to receive CONNECT request.")
		}
		if !serverResponded.Load() {
			t.Error("Expected server response.")
		}
	})

	t.Run("https proxy", func(t *testing.T) {
		var serverResponded atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method == http.MethodGet {
				serverResponded.Store(true)
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`ok`))
				return
			}
			w.WriteHeader(http.StatusMethodNotAllowed)
			w.Write([]byte(`error`))
		}))
		defer srv.Close()

		var proxyConnected atomic.Bool
		proxyServer := httptest.NewTLSServer(genProxyHandler(t, proxyHandlerOptions{connected: &proxyConnected}))
		defer proxyServer.Close()
		t.Logf("Server URL: %s", srv.URL)
		t.Logf("Proxy URL: %s", proxyServer.URL)

		// Create HTTP client that uses custom dialer
		tr, ok := proxyServer.Client().Transport.(*http.Transport)
		if !ok {
			t.Fatalf("expected transport of type *http.Transport, got: %T", proxyServer.Client().Transport)
		}
		cfg := tr.TLSClientConfig.Clone()

		pURL, err := url.Parse(proxyServer.URL)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		proxyHost, _, err := net.SplitHostPort(pURL.Host)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		cfg.ServerName = proxyHost

		dialer, err := NewProxyConnectDialer(pURL, &net.Dialer{}, WithTLS(cfg))
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		// The client that connects through the proxy does not need TLS configuration as the dialer handles it.
		client := &http.Client{
			Transport: &http.Transport{
				Dial: dialer.Dial,
			},
		}

		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected response status code 200, got: %d", resp.StatusCode)
		}
		if !proxyConnected.Load() {
			t.Error("Expected proxy to receive CONNECT request.")
		}
		if !serverResponded.Load() {
			t.Error("Expected server response.")
		}
	})

	t.Run("proxy requires auth", func(t *testing.T) {
		var serverResponded atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method == http.MethodGet {
				serverResponded.Store(true)
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`ok`))
				return
			}
			w.WriteHeader(http.StatusMethodNotAllowed)
			w.Write([]byte(`error`))
		}))
		defer srv.Close()

		var proxyConnected atomic.Bool
		proxyServer := httptest.NewServer(genProxyHandler(t, proxyHandlerOptions{connected: &proxyConnected, authValue: "Basic dGVzdDp0ZXN0"})) // test:test
		defer proxyServer.Close()
		t.Logf("Server URL: %s", srv.URL)
		t.Logf("Proxy URL: %s", proxyServer.URL)

		// Create HTTP client that uses custom dialer
		pURL, err := url.Parse(proxyServer.URL)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		dialer, err := NewProxyConnectDialer(pURL, &net.Dialer{}, WithProxyAuthorization("test", "test"))
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		client := &http.Client{
			Transport: &http.Transport{
				Dial: dialer.Dial,
			},
		}

		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected response status code 200, got: %d", resp.StatusCode)
		}
		if !proxyConnected.Load() {
			t.Error("Expected proxy to receive CONNECT request.")
		}
		if !serverResponded.Load() {
			t.Error("Expected server response.")
		}
	})

	t.Run("proxy failure", func(t *testing.T) {
		var serverResponded atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method == http.MethodGet {
				serverResponded.Store(true)
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`ok`))
				return
			}
			w.WriteHeader(http.StatusMethodNotAllowed)
			w.Write([]byte(`error`))
		}))
		defer srv.Close()

		var proxyConnected atomic.Bool
		proxyServer := httptest.NewServer(genProxyHandler(t, proxyHandlerOptions{connected: &proxyConnected, errorStatus: http.StatusInternalServerError}))
		defer proxyServer.Close()
		t.Logf("Server URL: %s", srv.URL)
		t.Logf("Proxy URL: %s", proxyServer.URL)

		// Create HTTP client that uses custom dialer
		pURL, err := url.Parse(proxyServer.URL)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		dialer, err := NewProxyConnectDialer(pURL, &net.Dialer{})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		client := &http.Client{
			Transport: &http.Transport{
				Dial: dialer.Dial,
			},
		}

		_, err = client.Get(srv.URL)
		if err == nil {
			t.Fatal("Expected to revieve error connecting to proxy")
		}
		if !strings.Contains(err.Error(), "dialer recieved non-200 response for CONNECT request: 500") {
			t.Fatalf("Unexpected error: %v", err)
		}
		if !proxyConnected.Load() {
			t.Error("Expected proxy to receive CONNECT request.")
		}
		if serverResponded.Load() {
			t.Error("Expected no server response.")
		}
	})

	t.Run("server failure", func(t *testing.T) {
		var serverResponded atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method == http.MethodGet {
				serverResponded.Store(true)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`ok`))
				return
			}
			w.WriteHeader(http.StatusMethodNotAllowed)
			w.Write([]byte(`error`))
		}))
		defer srv.Close()

		var proxyConnected atomic.Bool
		proxyServer := httptest.NewServer(genProxyHandler(t, proxyHandlerOptions{connected: &proxyConnected}))
		defer proxyServer.Close()
		t.Logf("Server URL: %s", srv.URL)
		t.Logf("Proxy URL: %s", proxyServer.URL)

		// Create HTTP client that uses custom dialer
		pURL, err := url.Parse(proxyServer.URL)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		dialer, err := NewProxyConnectDialer(pURL, &net.Dialer{})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		client := &http.Client{
			Transport: &http.Transport{
				Dial: dialer.Dial,
			},
		}

		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("Expected response status code 500, got: %d", resp.StatusCode)
		}
		if !proxyConnected.Load() {
			t.Error("Expected proxy to receive CONNECT request.")
		}
		if !serverResponded.Load() {
			t.Error("Expected server response.")
		}
	})
}
