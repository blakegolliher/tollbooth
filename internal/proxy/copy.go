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
// Adapted from github.com/blakegolliher/shunt internal/proxy/copy.go at commit
// 9b36d09ed109add4a2bedfe1dbfae6108038ee5b (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: answerWatch and
// the mutation-outcome tail dropped (tollbooth has no barriers); deadlineWriter flushes every
// chunk (D-006); progressReader stops the watchdog at EOF so the wait for response headers is
// governed by response_header_timeout alone.

package proxy

import (
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// bufPool hands out fixed-size copy buffers. Pooled so a streaming request allocates no
// per-byte memory after warm-up; *[]byte avoids an interface allocation on Put.
type bufPool struct {
	size int
	p    sync.Pool
}

func newBufPool(size int) *bufPool {
	bp := &bufPool{size: size}
	bp.p.New = func() any { b := make([]byte, size); return &b }
	return bp
}

func (bp *bufPool) get() *[]byte  { return bp.p.Get().(*[]byte) } //nolint:errcheck // pool holds only *[]byte
func (bp *bufPool) put(b *[]byte) { bp.p.Put(b) }

// reqState is the per-request streaming state, allocated once per request so the watchdog, the
// two body readers, and the response writer cost one allocation instead of four (docs/DESIGN.md
// §9). Its fields hold interface values (the bodies), so it escapes to the heap regardless.
type reqState struct {
	wd  watchdog
	in  progressReader
	out progressReader
	dst deadlineWriter
}

// watchdog cancels the backend request when no bytes move for idle. One timer per request;
// kick() is a timer reset, no allocation. It is armed only while bytes are expected to flow: the
// request body (until EOF) and the response body (from the first header byte to the end).
type watchdog struct {
	t    *time.Timer
	idle time.Duration
}

// arm creates the timer stopped; the first kick starts it.
func (w *watchdog) arm(idle time.Duration, cancel func()) {
	w.t = time.AfterFunc(idle, cancel)
	w.t.Stop()
	w.idle = idle
}

func (w *watchdog) kick() { w.t.Reset(w.idle) }
func (w *watchdog) stop() {
	if w.t != nil {
		w.t.Stop()
	}
}

// progressReader counts bytes and keeps the stall detectors alive on every successful read. On
// the request body it also pushes the server's read deadline forward, so a client that stops
// sending is cut off by net/http rather than by us. A request body is read by net/http's
// transport write loop, a different goroutine from the one that reads the count afterwards, so
// the counter is atomic.
type progressReader struct {
	r    io.Reader
	wd   *watchdog
	n    atomic.Int64
	rc   *http.ResponseController // set for the client request body: refresh the server read deadline
	idle time.Duration
	eof  atomic.Bool
}

// count reports the bytes read so far.
func (p *progressReader) count() int64 { return p.n.Load() }

// Close satisfies io.ReadCloser for the outbound request body. It does nothing: net/http's server
// owns the inbound body and closes it when the handler returns.
func (p *progressReader) Close() error { return nil }

func (p *progressReader) Read(b []byte) (int, error) {
	if p.rc != nil {
		_ = p.rc.SetReadDeadline(time.Now().Add(p.idle)) // unsupported writers return an error we can ignore
	}
	n, err := p.r.Read(b)
	if n > 0 {
		p.n.Add(int64(n))
		if p.wd != nil {
			p.wd.kick()
		}
	}
	if err == io.EOF {
		p.eof.Store(true)
		if p.wd != nil {
			p.wd.stop() // nothing more will flow on this side; the header wait has its own timeout
		}
	}
	return n, err
}

// deadlineWriter refreshes the server write deadline before every write, counts bytes, and
// flushes every chunk so a trickle response reaches the client as it arrives (D-006). It hides
// ReaderFrom so io.CopyBuffer uses our pooled buffer.
type deadlineWriter struct {
	w    io.Writer
	rc   *http.ResponseController
	idle time.Duration
	n    int64
}

func (d *deadlineWriter) Write(b []byte) (int, error) {
	if d.rc != nil {
		_ = d.rc.SetWriteDeadline(time.Now().Add(d.idle))
	}
	n, err := d.w.Write(b)
	d.n += int64(n)
	if err == nil && d.rc != nil {
		err = d.rc.Flush()
	}
	return n, err
}
