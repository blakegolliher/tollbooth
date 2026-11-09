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
// Adapted from github.com/blakegolliher/shunt internal/proxy/headers.go at commit
// 9b36d09ed109add4a2bedfe1dbfae6108038ee5b (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: the inbound map
// is stripped in place and reused instead of copied (docs/SPEC.md, Request identity); appendVia
// dropped, since tollbooth adds no header in either direction.

package proxy

import (
	"net/http"
	"strings"
)

// hopByHop headers are never forwarded in either direction (RFC 9110 §7.6.1). Expect is
// deliberately not here: it must reach the backend for 100-continue chaining.
var hopByHop = [...]string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// stripHopByHop deletes the hop-by-hop headers and any header named in Connection from h, in
// place. Nothing reads the inbound map after the outbound request is built, so reusing it costs
// no copy. Allocates only when Connection names extra headers.
func stripHopByHop(h http.Header) {
	for _, v := range h["Connection"] {
		for _, tok := range strings.Split(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				delete(h, http.CanonicalHeaderKey(tok))
			}
		}
	}
	for _, k := range hopByHop {
		delete(h, k)
	}
}

// copyResponseHeaders copies src into dst, sharing the value slices (neither side mutates header
// values after this point) and dropping hop-by-hop headers. dst is the ResponseWriter's map,
// already allocated by net/http.
func copyResponseHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isHopByHop(k) {
			continue
		}
		dst[k] = vv
	}
}

func isHopByHop(k string) bool {
	for _, h := range hopByHop {
		if h == k {
			return true
		}
	}
	return false
}
