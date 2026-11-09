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
// Adapted from github.com/blakegolliher/shunt internal/listener/listener.go at commit
// 9b36d09ed109add4a2bedfe1dbfae6108038ee5b (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: one certificate
// pair behind an atomic pointer so reload swaps it without a new listener (no SNI map); PROXY
// protocol v1/v2 via go-proxyproto with the header required when enabled.

package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync/atomic"

	proxyproto "github.com/pires/go-proxyproto"

	"github.com/blakegolliher/tollbooth/internal/config"
)

// CertStore holds the listener's certificate pair and swaps it on reload. The TLS config's
// GetCertificate reads the pointer on every handshake, so a reload takes effect for the next
// connection without touching the listener.
type CertStore struct {
	cert atomic.Pointer[tls.Certificate]
}

// Load reads the pair from disk and installs it.
func (cs *CertStore) Load(certFile, keyFile string) error {
	c, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("listener: certificate: %w", err)
	}
	cs.cert.Store(&c)
	return nil
}

func (cs *CertStore) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c := cs.cert.Load(); c != nil {
		return c, nil
	}
	return nil, errors.New("listener: no certificate loaded")
}

// Listen opens the data listener on c.Address: plaintext when no certificate pair is configured,
// TLS otherwise (TLS 1.2 minimum, http/1.1 only, Go's cipher ordering untouched), wrapped in a
// PROXY protocol reader when c.ProxyProtocol is set. The returned CertStore is nil for plaintext.
func Listen(c *config.Listen) (net.Listener, *CertStore, error) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", c.Address)
	if err != nil {
		return nil, nil, fmt.Errorf("listener: %w", err)
	}
	if c.ProxyProtocol {
		ln = &proxyproto.Listener{
			Listener: ln,
			// Every connection must carry a header; one without is closed. A connection that
			// could skip it could also forge the source address by omission.
			ConnPolicy:        func(proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) { return proxyproto.REQUIRE, nil },
			ReadHeaderTimeout: c.ReadHeaderTimeout,
		}
	}
	if c.TLS.Cert == "" && c.TLS.Key == "" {
		return ln, nil, nil
	}
	cs := &CertStore{}
	if err := cs.Load(c.TLS.Cert, c.TLS.Key); err != nil {
		_ = ln.Close() // closing after a failed setup
		return nil, nil, err
	}
	minVersion := uint16(tls.VersionTLS12)
	if c.TLS.MinVersion == "1.3" {
		minVersion = tls.VersionTLS13
	}
	tc := &tls.Config{
		MinVersion:     minVersion,
		GetCertificate: cs.get,
		NextProtos:     []string{"http/1.1"}, // no h2
		// CipherSuites left nil: Go orders by CPU support.
	}
	return tls.NewListener(ln, tc), cs, nil
}
