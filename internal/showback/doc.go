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

// Package showback counts every completed request once, at completion, into the open hour's
// aggregator (struct-keyed tuples in 256 locked shards), snapshots the whole hour to
// current/<hour>-<node>.ndjson every flush interval, seals it into usage/YYYY/MM/DD/HH-<node>.ndjson
// when the clock crosses the hour, recovers the snapshot at restart, and reads the files back for
// query and verify (docs/DESIGN.md §6, docs/SHOWBACK_SCHEMA.md). The snapshot is the state.
package showback
