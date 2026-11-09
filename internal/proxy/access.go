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
// Adapted from github.com/blakegolliher/shunt internal/telemetry/accesslog.go at commit
// 9b36d09ed109add4a2bedfe1dbfae6108038ee5b (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: tollbooth's
// field set (class, status class, result, endpoint; no request id, tenant, or cluster); the
// object key and the access key are never written (CLAUDE.md).

package proxy

import (
	"io"
	"log/slog"
	"net/netip"
	"time"
)

// Access is one access-log record. Off by default (logging.access); a debugging aid, never an
// accounting source. The object key and the access key are not fields.
type Access struct {
	Client      netip.Addr
	Method      string
	Host        string
	Addressing  string
	Bucket      string
	Op          string
	Class       string
	Status      int
	StatusClass string
	Result      string
	BytesIn     int64
	BytesOut    int64
	Duration    time.Duration
	TTFB        time.Duration // backend time to first header byte; 0 if none
	Endpoint    string        // backend host:port, "" when none was reached
	TLS         string        // "1.2", "1.3", "" for plaintext
	Error       string        // "" on success
}

// AccessLogger writes JSON lines.
type AccessLogger struct {
	l *slog.Logger
}

// NewAccessLogger writes one JSON object per line to w. A nil w disables logging.
func NewAccessLogger(w io.Writer) *AccessLogger {
	if w == nil {
		return nil
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.LevelKey || a.Key == slog.MessageKey {
				return slog.Attr{} // every record is an access record
			}
			if a.Key == slog.TimeKey {
				a.Key = "ts"
			}
			return a
		},
	})
	return &AccessLogger{l: slog.New(h)}
}

// Log emits the record. Attribute order is the documented schema order.
func (a *AccessLogger) Log(r *Access) {
	a.l.LogAttrs(nil, slog.LevelInfo, "", //nolint:staticcheck // nil ctx is documented as allowed by slog
		slog.String("client", r.Client.String()),
		slog.String("method", r.Method),
		slog.String("host", r.Host),
		slog.String("addressing", r.Addressing),
		slog.String("bucket", r.Bucket),
		slog.String("operation", r.Op),
		slog.String("op_class", r.Class),
		slog.Int("status", r.Status),
		slog.String("status_class", r.StatusClass),
		slog.String("result", r.Result),
		slog.Int64("bytes_in", r.BytesIn),
		slog.Int64("bytes_out", r.BytesOut),
		slog.Float64("duration_ms", ms(r.Duration)),
		slog.Float64("ttfb_ms", ms(r.TTFB)),
		slog.String("endpoint", r.Endpoint),
		slog.String("tls", r.TLS),
		slog.String("error", r.Error),
	)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
