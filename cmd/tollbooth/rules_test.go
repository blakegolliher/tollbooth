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

package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/limiter"
	"github.com/blakegolliher/tollbooth/internal/s3id"
)

const rulesTestConfig = `node_id: t
backend: {scheme: http, endpoints: [127.0.0.1:1]}
showback: {dir: /tmp/x}
admin: {token: tok}
cluster: {node_count: 2}
limits:
  - name: tenant-a-writes
    operation: [PutObject, UploadPart, CopyObject]
    access_key: [AKIAEXAMPLE1, AKIAEXAMPLE2]
    bucket: training-data
    prefix: ckpt/
    limit_per_second: 2000
    burst: 4000
    per: [access_key, bucket]
  - name: list-storm
    operation: List*
    limit_per_second: 50
    burst: 200
    per: source_ip
    respond: 429
  - name: frozen
    access_key: AKIAREVOKED
    blocked: true
`

// adminFor starts an admin server with the rules loaded and a few denials recorded, and returns
// its address.
func adminFor(t *testing.T) (string, *limiter.Limiter) {
	t.Helper()
	c, err := config.Parse([]byte(rulesTestConfig))
	if err != nil {
		t.Fatal(err)
	}
	m := admin.NewMetrics()
	lim := limiter.New(&m.Limiter)
	if err := lim.Apply(c); err != nil {
		t.Fatal(err)
	}
	id := s3id.RequestIdentity{AccessKey: "AKIAREVOKED", Bucket: "b", Key: "k", Op: s3id.OpGetObject, Class: s3id.ClassRead, Level: s3id.LevelObject}
	for i := 0; i < 3; i++ {
		lim.Decide(&id, lim.Now(time.Now()), true)
	}
	s := &admin.Server{Metrics: m, Limiter: lim, Token: "tok"}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), lim
}

func runOut(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	out, errOut := tempFiles(t)
	rc := run(args, out, errOut)
	return rc, read(out), read(errOut)
}

func TestRulesList(t *testing.T) {
	addr, _ := adminFor(t)
	rc, out, errOut := runOut(t, "rules", "list", "--admin", addr, "--token", "tok")
	if rc != 0 {
		t.Fatalf("rc %d: %s", rc, errOut)
	}
	for _, want := range []string{"NAME", "LIMIT", "tenant-a-writes", "access_key(2) bucket=training-data prefix=ckpt/", "access_key,bucket", "2,000 per second (1000/s on this node)",
		"list-storm", "List*", "429", "frozen", "access_key=AKIAREVOKED", "blocked", "3 rules, 0 live keys",
		// The read-back: every rule as a sentence under the table.
		"At most 50 List* calls (11 operations) per second from each source IP, up to 200 at once. Over the limit, the client gets 429 TooManyRequests.",
		"Refuses every request by access key AKIAREVOKED. The client gets 503 SlowDown."} {
		if !strings.Contains(out, want) {
			t.Errorf("table output lacks %q:\n%s", want, out)
		}
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasSuffix(lines[3], "  3") { // frozen: DENIED column is 3
		t.Errorf("frozen row should end with 3 denials: %q", lines[3])
	}

	rc, out, _ = runOut(t, "rules", "list", "--admin", "http://"+addr, "--token", "tok", "--format", "json")
	var lim admin.Limits
	if rc != 0 || json.Unmarshal([]byte(out), &lim) != nil || len(lim.Rules) != 3 || lim.Rules[2].Denied != 3 {
		t.Fatalf("json: rc %d\n%s", rc, out)
	}

	rc, out, _ = runOut(t, "rules", "list", "--admin", addr, "--token", "tok", "--format", "csv")
	if rc != 0 || !strings.HasPrefix(out, "NAME,LIMIT,PER,OPERATION,ONLY_FOR,RESPOND,KEYS,MATCHED,DENIED\n") || !strings.Contains(out, "frozen,blocked,-,any,access_key=AKIAREVOKED,503,0,3,3\n") {
		t.Fatalf("csv: rc %d\n%s", rc, out)
	}
}

func TestRulesShow(t *testing.T) {
	addr, _ := adminFor(t)
	rc, out, _ := runOut(t, "rules", "show", "list-storm", "--admin", addr, "--token", "tok")
	if rc != 0 || !strings.Contains(out, "per") || !strings.Contains(out, "source_ip") || !strings.Contains(out, "50 per second (25/s on this node)") || !strings.Contains(out, "429") ||
		!strings.Contains(out, "At most 50 List* calls (11 operations) per second from each source IP, up to 200 at once.") {
		t.Fatalf("rc %d\n%s", rc, out)
	}
	rc, out, _ = runOut(t, "rules", "show", "frozen", "--admin", addr, "--token", "tok", "--format", "json")
	var r limiter.RuleStatus
	if rc != 0 || json.Unmarshal([]byte(out), &r) != nil || r.Name != "frozen" || r.Denied != 3 || !r.Blocked {
		t.Fatalf("json: rc %d\n%s", rc, out)
	}
	rc, _, errOut := runOut(t, "rules", "show", "nope", "--admin", addr, "--token", "tok")
	if rc != 1 || !strings.Contains(errOut, `no rule "nope" in force (3 rules loaded)`) {
		t.Fatalf("missing rule: rc %d %q", rc, errOut)
	}
	if rc, _, _ := runOut(t, "rules", "show", "--admin", addr); rc != 1 {
		t.Fatal("show without a name must fail")
	}
}

func TestRulesTest(t *testing.T) {
	addr, lim := adminFor(t)
	rc, out, errOut := runOut(t, "rules", "test", "--admin", addr, "--token", "tok", "--op", "PutObject", "--access-key", "AKIAEXAMPLE1", "--bucket", "training-data", "--key", "ckpt/1", "--ip", "10.1.1.1")
	if rc != 0 {
		t.Fatalf("rc %d: %s", rc, errOut)
	}
	if !strings.Contains(out, "PutObject (key level): allowed; 1 rule matches") || !strings.Contains(out, "tenant-a-writes") || !strings.Contains(out, "key=AKIAEXAMPLE1 bucket=training-data") || !strings.Contains(out, "allow") {
		t.Fatalf("%s", out)
	}
	if lim.Keys() != 0 {
		t.Fatal("rules test consumed a token")
	}
	rc, out, _ = runOut(t, "rules", "test", "--admin", addr, "--token", "tok", "--op", "ListBuckets", "--access-key", "AKIAREVOKED")
	if rc != 0 || !strings.Contains(out, "denied by frozen (503 SlowDown, Retry-After 1s); 2 rules match") || !strings.Contains(out, "(shared)") || !strings.Contains(out, "deny") {
		t.Fatalf("rc %d\n%s", rc, out)
	}
	rc, out, _ = runOut(t, "rules", "test", "--admin", addr, "--token", "tok", "--op", "GetObject", "--bucket", "other", "--key", "k")
	if rc != 0 || !strings.Contains(out, "allowed; no rule matches") {
		t.Fatalf("rc %d\n%s", rc, out)
	}
	rc, out, _ = runOut(t, "rules", "test", "--admin", addr, "--token", "tok", "--op", "ListObjectsV2", "--bucket", "b", "--ip", "10.0.0.9", "--format", "json")
	var res admin.TestResponse
	if rc != 0 || json.Unmarshal([]byte(out), &res) != nil || res.Level != "bucket" || len(res.Result.Matches) != 1 || res.Result.Matches[0].SourceIP != "10.0.0.9" {
		t.Fatalf("json: rc %d\n%s", rc, out)
	}
	rc, out, _ = runOut(t, "rules", "test", "--admin", addr, "--token", "tok", "--op", "ListObjectsV2", "--bucket", "b", "--ip", "10.0.0.9", "--format", "csv")
	if rc != 0 || !strings.Contains(out, "RULE,KEY,LIVE_KEYS,VERDICT,RETRY_AFTER,RESPOND\nlist-storm,ip=10.0.0.9,0,allow,-,429\n") {
		t.Fatalf("csv: rc %d\n%s", rc, out)
	}
	if rc, _, errOut := runOut(t, "rules", "test", "--admin", addr, "--token", "tok"); rc != 1 || !strings.Contains(errOut, "--op is required") {
		t.Fatalf("rc %d %q", rc, errOut)
	}
	if rc, _, errOut := runOut(t, "rules", "test", "--admin", addr, "--token", "tok", "--op", "Nope"); rc != 1 || !strings.Contains(errOut, "400") || !strings.Contains(errOut, "unknown operation") {
		t.Fatalf("rc %d %q", rc, errOut)
	}
}

func TestRulesTop(t *testing.T) {
	addr, _ := adminFor(t)
	rc, out, _ := runOut(t, "rules", "top", "--admin", addr, "--token", "tok", "--n", "5")
	// frozen has no `per`, so the key carries no dimensions: the row shows the rule and the count.
	if rc != 0 || !strings.Contains(out, "RULE") || !strings.Contains(out, "frozen") || strings.Contains(out, "AKIAREVOKED") {
		t.Fatalf("rc %d\n%s", rc, out)
	}
	if !strings.Contains(out, "  3  ") && !strings.Contains(out, " 3 ") {
		t.Fatalf("denied count missing:\n%s", out)
	}
	rc, out, _ = runOut(t, "rules", "top", "--admin", addr, "--token", "tok", "--format", "json", "--window", "5m")
	var top admin.Top
	if rc != 0 || json.Unmarshal([]byte(out), &top) != nil || top.WindowSeconds != 300 || len(top.Top) != 1 || top.Top[0].Denied != 3 {
		t.Fatalf("json: rc %d\n%s", rc, out)
	}
	rc, out, _ = runOut(t, "rules", "top", "--admin", addr, "--token", "tok", "--format", "csv")
	if rc != 0 || !strings.HasPrefix(out, "RULE,ACCESS_KEY,BUCKET,PREFIX,SOURCE_IP,DENIED,LAST\nfrozen,,,,,3,") {
		t.Fatalf("csv: rc %d\n%s", rc, out)
	}
	rc, out, _ = runOut(t, "rules", "top", "--admin", addr, "--token", "tok", "--window", "1ms")
	time.Sleep(2 * time.Millisecond)
	if rc != 0 || !strings.Contains(out, "no denials in the last 1ms") {
		t.Fatalf("empty window: rc %d\n%s", rc, out)
	}
}

// The endpoint and token resolve from flags, then the environment, then the config file; a wrong
// token is one line on stderr and exit 1; the usage paths exit 2.
func TestRulesResolution(t *testing.T) {
	addr, _ := adminFor(t)
	t.Setenv("TOLLBOOTH_ADMIN", addr)
	t.Setenv("TOLLBOOTH_TOKEN", "tok")
	if rc, out, errOut := runOut(t, "rules", "list"); rc != 0 || !strings.Contains(out, "frozen") {
		t.Fatalf("env: rc %d %s %s", rc, out, errOut)
	}
	t.Setenv("TOLLBOOTH_TOKEN", "wrong")
	if rc, _, errOut := runOut(t, "rules", "list"); rc != 1 || !strings.Contains(errOut, "401") || strings.Count(strings.TrimSpace(errOut), "\n") != 0 {
		t.Fatalf("wrong token: rc %d %q", rc, errOut)
	}
	t.Setenv("TOLLBOOTH_ADMIN", "")
	t.Setenv("TOLLBOOTH_TOKEN", "")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tollbooth.yaml")
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(rulesTestConfig, "admin: {token: tok}", "admin: {address: "+addr+", token: tok}", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc, out, errOut := runOut(t, "rules", "list", "--config", cfgPath); rc != 0 || !strings.Contains(out, "frozen") {
		t.Fatalf("config: rc %d %s %s", rc, out, errOut)
	}
	// No flags, no environment, no file: the default address, which nothing answers.
	if rc, _, errOut := runOut(t, "rules", "list", "--config", filepath.Join(dir, "missing.yaml")); rc != 1 || !strings.Contains(errOut, "admin API at http://127.0.0.1:9900") {
		t.Fatalf("default: rc %d %q", rc, errOut)
	}
	if rc, _, errOut := runOut(t, "rules", "list", "--admin", addr, "--format", "yaml"); rc != 1 || !strings.Contains(errOut, "--format must be") {
		t.Fatalf("format: rc %d %q", rc, errOut)
	}
	if rc, _, _ := runOut(t, "rules"); rc != 2 {
		t.Fatal("rules alone is usage")
	}
	if rc, _, _ := runOut(t, "rules", "bogus"); rc != 2 {
		t.Fatal("unknown verb is usage")
	}
	if rc, out, _ := runOut(t, "rules", "help"); rc != 0 || !strings.Contains(out, "verbs:") {
		t.Fatal("rules help")
	}
	if rc, _, _ := runOut(t, "rules", "list", "--nope"); rc != 2 {
		t.Fatal("bad flag is usage")
	}
}
