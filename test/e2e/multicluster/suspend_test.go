//go:build e2e

package multicluster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/agent"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

// tail is an open Loki tail through the proxy. It keeps the stream labels and lines of each entry.
type tail struct {
	mu      sync.Mutex
	entries []tailEntry
	done    chan error
}

type tailEntry struct {
	r    rec
	line string
}

func (f *fixture) openTail(t *testing.T, group, query string) *tail {
	t.Helper()
	u := "ws" + strings.TrimPrefix(f.h.proxyURL, "http") + logs.prefix + "/loki/api/v1/tail?" +
		url.Values{"query": {query}, "limit": {"1000"}}.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hdr := http.Header{"Authorization": {"Bearer " + f.h.issuer.Token(t, group, group)}}
	c, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		st := 0
		if resp != nil {
			st = resp.StatusCode
		}
		t.Fatalf("dial the tail: %v (status %d)", err, st)
	}
	c.SetReadLimit(8 << 20)
	t.Cleanup(func() { _ = c.CloseNow() })
	tl := &tail{done: make(chan error, 1)}
	go func() {
		for {
			_, msg, err := c.Read(context.Background())
			if err != nil {
				tl.done <- err
				return
			}
			var fr struct {
				Streams []struct {
					Stream map[string]string `json:"stream"`
					Values [][]string        `json:"values"`
				} `json:"streams"`
			}
			if json.Unmarshal(msg, &fr) != nil {
				continue
			}
			tl.mu.Lock()
			for _, s := range fr.Streams {
				for _, v := range s.Values {
					if len(v) >= 2 {
						tl.entries = append(tl.entries, tailEntry{rec{s.Stream["cluster"], s.Stream["namespace"]}, v[1]})
					}
				}
			}
			tl.mu.Unlock()
		}
	}()
	return tl
}

// has reports whether the tail received a line with the marker for the record.
func (tl *tail) has(r rec, marker string) bool {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	for _, e := range tl.entries {
		if e.r == r && strings.HasPrefix(e.line, marker+" ") {
			return true
		}
	}
	return false
}

// foreign returns the entries that are not of the record.
func (tl *tail) foreign(r rec) []tailEntry {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	var out []tailEntry
	for _, e := range tl.entries {
		if e.r != r {
			out = append(out, e)
		}
	}
	return out
}

// testStolenCert runs the procedure of PLAN §18.7 for cluster-a (TASKS T6.8):
//  1. suspended: true stops new requests and closes an open Loki tail, and the receiver rejects syncs;
//  2. the hub admin changes spec.identity to the URI SAN of a new agent cert;
//  3. the hub admin deletes the SpokePolicy objects of the spoke;
//  4. suspended: false. The grants return only after a new valid sync with the new cert.
func testStolenCert(t *testing.T, f *fixture) {
	s := f.spokes[clusterA]
	team := groupTeam + clusterA
	teamRec := rec{clusterA, nsTeam}
	ctx := context.Background()

	// An open tail of a spoke user: it receives new lines of its own cluster only.
	tl := f.openTail(t, team, `{app="mc"}`)
	eventually(t, 30*time.Second, "tail receives a new line", func() (bool, string) {
		pushLoki(t, f.b.lokiSingle.URL("3100"), "", []rec{teamRec, {clusterB, nsTeam}}, time.Now(), "tail")
		return tl.has(teamRec, "tail"), "no tail line yet"
	})
	if fe := tl.foreign(teamRec); len(fe) > 0 {
		t.Errorf("the tail of %s received entries of other records: %v", team, fe)
	}

	// Step 1: suspend.
	suspended := time.Now()
	f.h.patchSpoke(t, clusterA, func(sp *v1.SpokeClusterSpec) { sp.Suspended = true })
	f.eventuallyDenied(t, team, metrics, msgExpired, 10*time.Second)
	t.Logf("new requests stopped %s after the suspension", time.Since(suspended).Round(10*time.Millisecond))
	for _, ref := range sharedBackends {
		f.isDenied(t, team, ref, msgExpired)
		f.sees(t, groupBoth, ref, []rec{{clusterB, nsTeam}})
	}
	select {
	case err := <-tl.done:
		if st := websocket.CloseStatus(err); st != websocket.StatusPolicyViolation {
			t.Errorf("the tail closed with %v (status %d); want status %d", err, st, websocket.StatusPolicyViolation)
		}
		if d := time.Since(suspended); d > 10*time.Second {
			t.Errorf("the tail closed %s after the suspension; want at most 10s", d)
		}
		t.Logf("the tail closed %s after the suspension", time.Since(suspended).Round(10*time.Millisecond))
	case <-time.After(15 * time.Second):
		t.Fatal("the tail is still open 15s after the suspension")
	}

	// The receiver rejects each sync of a suspended spoke, and lastSyncTime does not change.
	expectRejected(t, s.sender(t, f.h), "the SpokeCluster is suspended")
	before := f.h.spokeCluster(t, clusterA).Status.LastSyncTime
	time.Sleep(3 * heartbeat)
	if after := f.h.spokeCluster(t, clusterA).Status.LastSyncTime; !after.Equal(before) {
		t.Errorf("lastSyncTime changed while suspended: %v -> %v", before, after)
	}

	// Step 2: a new agent identity. The agent keeps the old cert for now.
	newIdentity := identity("agent-rotated", clusterA)
	f.h.patchSpoke(t, clusterA, func(sp *v1.SpokeClusterSpec) { sp.Identity = newIdentity })

	// Step 3: delete the SpokePolicy objects of the spoke. This also removes labels.deny and labels.redact
	// of forged rules.
	if err := f.h.api.admin.DeleteAllOf(ctx, &v1.SpokePolicy{}, client.MatchingLabels{spokesync.ClusterLabel: clusterA}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "SpokePolicy objects of cluster-a deleted", func() (bool, string) {
		return f.h.spokePolicies(t, clusterA) == 0, "objects remain"
	})

	// Step 4: resume. The old cert no longer matches, so no grant returns.
	f.h.patchSpoke(t, clusterA, func(sp *v1.SpokeClusterSpec) { sp.Suspended = false })
	expectRejected(t, s.sender(t, f.h), "unknown identity")
	consistently(t, 4*heartbeat, "no grant before a sync with the new cert", func() (bool, string) {
		res := f.b.query(t, f.as(t, team), metrics, "")
		if res.status != http.StatusForbidden {
			return false, res.String()
		}
		if n := f.h.spokePolicies(t, clusterA); n != 0 {
			return false, "the old cert wrote SpokePolicy objects"
		}
		return !f.h.synced(t, clusterA), "the spoke has a valid sync with the old cert"
	})

	// The agent gets the new cert. Its next sync is valid, and the grants return.
	s.setIdentity(t, f.h, newIdentity)
	f.waitSpokeActive(t, clusterA)
	for _, ref := range sharedBackends {
		f.eventuallySees(t, team, ref, []rec{teamRec})
		f.sees(t, groupBoth, ref, []rec{{clusterA, nsTeam}, {clusterB, nsTeam}})
	}
}

// expectRejected sends a heartbeat with the current cert files of the agent and expects 403 with msg.
func expectRejected(t *testing.T, snd *agent.Sender, msg string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := snd.Send(ctx, syncRequest(t))
	var se *agent.StatusError
	if !errors.As(err, &se) || se.Code != http.StatusForbidden || se.Message != msg {
		t.Errorf("sync: got %v; want 403 %q", err, msg)
	}
}
