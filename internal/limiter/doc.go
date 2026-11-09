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

// Package limiter holds the compiled rate-limit rules, their matchers, and the GCRA key store
// (docs/DESIGN.md §5). One pass over the matching rules in config order consumes a token from
// each; the first denial throttles and decides the response mode. Keys are struct values of the
// identity's substrings in 256 locked shards; a background sweep deletes full buckets exactly.
// Failure inside the limiter fails open.
package limiter
