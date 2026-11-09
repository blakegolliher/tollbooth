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

package config

// Features holds feature flags: behavior that is off until an operator turns it on. Every flag
// defaults off and carries a removal criterion (CLAUDE.md). v1 has none; the struct exists so the
// first one is added in one place and `features:` is a known key.
type Features struct{}

// KillSwitches turn off behavior that is normally on, for an operator who hits a bug in it while
// tollbooth is serving. A kill switch is not a feature flag: it defaults to the normal behavior
// (false), it has no removal criterion, and it documents what breaks when it is flipped
// (CLAUDE.md). It is removed when the behavior it guards no longer needs a switch.
type KillSwitches struct {
	// LimiterDisable skips rule evaluation: every request is forwarded.
	//
	// Flipped, no rule is enforced and tollbooth_limiter_denied_total stops moving. Turn it on
	// only to get past a limiter bug while serving.
	LimiterDisable bool `yaml:"limiter_disable"`
	// ShowbackDisable stops counting and snapshotting.
	//
	// Flipped, the hours it covers are missing from the store and `usage verify` reports them as
	// downtime. Turn it on only to confirm a suspected showback cost or fault.
	ShowbackDisable bool `yaml:"showback_disable"`
}
