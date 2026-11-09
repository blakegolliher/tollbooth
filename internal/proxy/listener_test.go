// Copyright 2026 Blake Golliher
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Adapted from github.com/blakegolliher/shunt internal/listener/listener_test.go (writeCert,
// TestListenHandshake, TestListenPlaintext) at commit 9b36d09ed109add4a2bedfe1dbfae6108038ee5b
// (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: SNI map tests
// dropped (one pair); certificate hot reload, PROXY protocol v1 and v2 framing, and the
// end-to-end source address through the identity pass added.

package proxy

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	proxyproto "github.com/pires/go-proxyproto"

	"github.com/blakegolliher/tollbooth/internal/config"
)

// writeCert generates a self-signed cert for names into dir and returns the pair paths.
func writeCert(t *testing.T, dir, base string, names ...string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(dir, base+".crt"), filepath.Join(dir, base+".key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestListenTLSHandshakeAndReload(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeCert(t, dir, "a", "*.tb.example.com", "tb.example.com")
	ln, cs, err := Listen(&config.Listen{Address: "127.0.0.1:0", TLS: config.TLS{Cert: cert, Key: key, MinVersion: "1.2"}, ReadHeaderTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if cs == nil {
		t.Fatal("TLS listener must return a CertStore")
	}
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	dial := func(minVersion uint16) *tls.Conn {
		conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{ServerName: "b.tb.example.com", InsecureSkipVerify: true, NextProtos: []string{"h2", "http/1.1"}, MinVersion: minVersion, MaxVersion: minVersion}) //nolint:gosec // test
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return conn
	}
	conn := dial(tls.VersionTLS12)
	st := conn.ConnectionState()
	if st.NegotiatedProtocol != "http/1.1" || st.Version != tls.VersionTLS12 || st.PeerCertificates[0].Subject.CommonName != "*.tb.example.com" {
		t.Fatalf("negotiated %q version %x cn %s", st.NegotiatedProtocol, st.Version, st.PeerCertificates[0].Subject.CommonName)
	}
	conn.Close()
	// Hot reload: a new pair is served to the next handshake without touching the listener.
	cert2, key2 := writeCert(t, dir, "b", "renewed.tb.example.com")
	if err := cs.Load(cert2, key2); err != nil {
		t.Fatal(err)
	}
	conn = dial(tls.VersionTLS13)
	if cn := conn.ConnectionState().PeerCertificates[0].Subject.CommonName; cn != "renewed.tb.example.com" {
		t.Fatalf("after reload got cn %s", cn)
	}
	conn.Close()
	// A bad pair is refused and the old one stays.
	if err := cs.Load("/nope.crt", "/nope.key"); err == nil {
		t.Fatal("missing files must fail")
	}
	conn = dial(tls.VersionTLS13)
	if cn := conn.ConnectionState().PeerCertificates[0].Subject.CommonName; cn != "renewed.tb.example.com" {
		t.Fatalf("failed reload replaced the certificate: %s", cn)
	}
	conn.Close()
	// TLS 1.1 is refused.
	if _, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}); err == nil { //nolint:gosec // test
		t.Fatal("TLS 1.1 handshake succeeded")
	}
}

func TestListenMinVersion13(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeCert(t, dir, "a", "tb.example.com")
	ln, _, err := Listen(&config.Listen{Address: "127.0.0.1:0", TLS: config.TLS{Cert: cert, Key: key, MinVersion: "1.3"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	if _, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}); err == nil { //nolint:gosec // test
		t.Fatal("TLS 1.2 accepted with min_version 1.3")
	}
}

// A plaintext listener serves raw TCP and needs no certificate at all; a half-configured pair
// is a config error upstream, and a missing file fails here.
func TestListenPlaintextAndBadPair(t *testing.T) {
	ln, cs, err := Listen(&config.Listen{Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if cs != nil {
		t.Fatal("plaintext listener returned a CertStore")
	}
	if _, isTLS := ln.(interface{ ConnectionState() tls.ConnectionState }); isTLS {
		t.Fatal("plaintext listener wraps TLS")
	}
	if _, _, err := Listen(&config.Listen{Address: "127.0.0.1:0", TLS: config.TLS{Cert: "/nope.crt", Key: "/nope.key"}}); err == nil {
		t.Fatal("a TLS listener with missing files opened")
	}
}

// PROXY protocol v1 and v2 headers set the source address the identity pass sees; a connection
// without a header is closed, because the header is required when the feature is on.
func TestProxyProtocolSourceAddress(t *testing.T) {
	nb := newRig(t, 1, time.Second) // for its backend and config shape
	c := *nb.cfg.Load()
	c.Listen.Address = "127.0.0.1:0"
	c.Listen.ProxyProtocol = true
	c.Listen.ReadHeaderTimeout = time.Second
	ln, _, err := Listen(&c.Listen)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	nb.cfg.Store(&c)
	srv := NewServer(nb.h, &c.Listen, nb.h.Metrics, nil)
	go srv.Serve(ln) //nolint:errcheck // test server
	defer srv.Close()

	send := func(t *testing.T, header []byte) (*http.Response, error) {
		t.Helper()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		if _, err := conn.Write(header); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "GET /b/k HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n")
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		return http.ReadResponse(bufio.NewReader(conn), nil)
	}

	t.Run("v1", func(t *testing.T) {
		resp, err := send(t, []byte("PROXY TCP4 203.0.113.9 10.0.0.1 51000 8443\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
		resp.Body.Close()              //nolint:errcheck // test
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if l := nb.waitLog(t, `"client":"203.0.113.9"`); !strings.Contains(l, `"operation":"GetObject"`) {
			t.Fatalf("log: %s", l)
		}
	})
	t.Run("v2", func(t *testing.T) {
		h := &proxyproto.Header{
			Version: 2, Command: proxyproto.PROXY, TransportProtocol: proxyproto.TCPv6,
			SourceAddr:      &net.TCPAddr{IP: net.ParseIP("2001:db8::9"), Port: 51000},
			DestinationAddr: &net.TCPAddr{IP: net.ParseIP("::1"), Port: 8443},
		}
		b, err := h.Format()
		if err != nil {
			t.Fatal(err)
		}
		resp, err := send(t, b)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
		resp.Body.Close()              //nolint:errcheck // test
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		nb.waitLog(t, `"client":"2001:db8::9"`)
	})
	t.Run("missing header is refused", func(t *testing.T) {
		resp, err := send(t, nil)
		if err == nil {
			resp.Body.Close() //nolint:errcheck // test
			t.Fatalf("a connection without a PROXY header was served (status %d)", resp.StatusCode)
		}
	})
	t.Run("trusted XFF still applies after PROXY", func(t *testing.T) {
		// The PROXY source is 127.0.0.1, which the rig trusts, so X-Forwarded-For is honored.
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprintf(conn, "PROXY TCP4 127.0.0.5 127.0.0.1 51000 8443\r\nGET /b/k HTTP/1.1\r\nHost: h\r\nX-Forwarded-For: 198.51.100.4\r\nConnection: close\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
		resp.Body.Close()              //nolint:errcheck // test
		nb.waitLog(t, `"client":"198.51.100.4"`)
	})
}
