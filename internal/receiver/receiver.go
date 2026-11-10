// Package receiver is the hub side of the policy transport (PLAN §18.5). It accepts the syncs of spoke
// agents over mTLS, stores the spoke policies as SpokePolicy objects and renews SpokeCluster.status.
//
// The receiver fails closed. Each doubt about the caller, the input or the stored state gives a rejection,
// and only a sync that completes all steps renews lastSyncTime.
package receiver

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/controller"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

// Defaults of Options.
const (
	DefaultMaxBodyBytes = 8 << 20
	DefaultMaxPolicies  = 2000
	// DefaultMaxInFlight keeps the memory below about 512 MiB: a request with the default limits uses
	// at most about 50 MiB (the body and the typed decode at the value limit).
	DefaultMaxInFlight = 8
	// DefaultRequestBurst and DefaultRequestInterval limit the requests of one identity (PLAN §18.5): a burst
	// of DefaultRequestBurst requests, then one request for each DefaultRequestInterval. An agent with the
	// lowest heartbeat interval (10 seconds) and a change at each heartbeat sends a heartbeat and a full sync
	// each 10 seconds. Its retries back off from one second up to one minute.
	DefaultRequestBurst    = 6
	DefaultRequestInterval = 5 * time.Second
	// ChangeInterval is the minimum time between two changed syncs of one cluster (PLAN §18.5).
	ChangeInterval = 10 * time.Second
)

// Options configure a Receiver.
type Options struct {
	// Client must read from the API server directly, not from a cache.
	Client client.Client
	// MaxBodyBytes is the maximum size of a request body (default DefaultMaxBodyBytes).
	MaxBodyBytes int64
	// MaxPolicies is the maximum number of policies in a full sync (default DefaultMaxPolicies).
	MaxPolicies int
	// MaxInFlight is the maximum number of requests that the receiver reads and handles at the same time
	// (default DefaultMaxInFlight). With the limit on the JSON values of one request, which depends on
	// MaxPolicies (valueLimit), it limits the memory for request bodies.
	MaxInFlight int
	// RequestBurst and RequestInterval limit the requests of one identity: heartbeats, full syncs and the
	// requests of an identity that is not registered (defaults DefaultRequestBurst and
	// DefaultRequestInterval). The limit applies before the body is read and before any read of the API
	// server.
	RequestBurst    int
	RequestInterval time.Duration
	// Logger receives one line for each rejected sync (default slog.Default()).
	Logger *slog.Logger
	// Now is the hub clock (default time.Now).
	Now func() time.Time
}

// Receiver is the http.Handler of the sync endpoint. It must run behind a TLS listener that verifies the
// client cert against the client CA of the receiver.
type Receiver struct {
	c           client.Client
	maxBody     int64
	maxPolicies int
	log         *slog.Logger
	now         func() time.Time

	slots chan struct{} // one entry for each request in progress

	burst    int
	interval time.Duration

	mu         sync.Mutex
	callers    map[string]bool      // identities with a request in progress
	running    map[string]bool      // clusters with a sync in progress
	lastChange map[string]time.Time // start of the last changed sync for each cluster
	buckets    map[string]bucket    // request tokens of each identity
	lastPrune  time.Time            // last removal of full buckets

	// afterWrites runs between the writes and the deletes of a changed sync. Only tests set it.
	afterWrites func(ctx context.Context, cluster string) error
}

// New creates a Receiver.
func New(o Options) *Receiver {
	r := &Receiver{
		c:           o.Client,
		maxBody:     o.MaxBodyBytes,
		maxPolicies: o.MaxPolicies,
		log:         o.Logger,
		now:         o.Now,
		callers:     map[string]bool{},
		running:     map[string]bool{},
		lastChange:  map[string]time.Time{},
		buckets:     map[string]bucket{},
		burst:       o.RequestBurst,
		interval:    o.RequestInterval,
	}
	if r.burst <= 0 {
		r.burst = DefaultRequestBurst
	}
	if r.interval <= 0 {
		r.interval = DefaultRequestInterval
	}
	if o.MaxInFlight <= 0 {
		o.MaxInFlight = DefaultMaxInFlight
	}
	r.slots = make(chan struct{}, o.MaxInFlight)
	if r.maxBody <= 0 {
		r.maxBody = DefaultMaxBodyBytes
	}
	if r.maxPolicies <= 0 {
		r.maxPolicies = DefaultMaxPolicies
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.now == nil {
		r.now = time.Now
	}
	return r
}

// rejection is an error with the HTTP status of the response. Its message goes to the agent, so it must not
// contain data of other spokes.
type rejection struct {
	status int
	msg    string
	// quiet logs the rejection at debug level, for repeated rejections of one cause.
	quiet bool
}

func (e *rejection) Error() string { return e.msg }

func reject(status int, msg string) error { return &rejection{status: status, msg: msg} }

// internalError hides err from the agent. The caller logs it.
func internalError(err error) error {
	return fmt.Errorf("%w: %w", reject(http.StatusInternalServerError, "internal error"), err)
}

// apiError maps an error of a write to the API server. A conflict means that the stored state or the
// SpokeCluster changed during the sync, so the agent can retry.
func apiError(err error) error {
	switch {
	case apierrors.IsConflict(err), apierrors.IsNotFound(err), apierrors.IsAlreadyExists(err):
		return fmt.Errorf("%w: %w", reject(http.StatusConflict, "the stored state changed during the sync"), err)
	case apierrors.IsInvalid(err):
		return fmt.Errorf("%w: %w", reject(http.StatusBadRequest, "the API server rejected a policy"), err)
	}
	return internalError(err)
}

// ServeHTTP implements http.Handler.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	identity, resp, err := r.handle(w, req)
	if err != nil {
		var rej *rejection
		if !errors.As(err, &rej) {
			rej = &rejection{status: http.StatusInternalServerError, msg: "internal error"}
		}
		level := slog.LevelInfo
		switch {
		case rej.status >= http.StatusInternalServerError:
			level = slog.LevelError
		case rej.quiet:
			level = slog.LevelDebug
		}
		r.log.Log(req.Context(), level, "sync rejected", "identity", identity, "status", rej.status, "error", err.Error())
		switch rej.status {
		case http.StatusMethodNotAllowed:
			w.Header().Set("Allow", http.MethodPost)
		case http.StatusTooManyRequests:
			w.Header().Set("Retry-After", strconv.Itoa(int(ChangeInterval/time.Second)))
		case http.StatusServiceUnavailable:
			w.Header().Set("Retry-After", "1")
		}
		writeJSON(w, rej.status, spokesync.ErrorResponse{Error: rej.msg})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status, b = http.StatusInternalServerError, []byte(`{"error":"internal error"}`)
	}
	w.Header().Set("Content-Type", spokesync.ContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// handle checks the request in this order: path, method, media type, identity, the limits on requests in
// progress, body, and then the sync. It returns the identity for the log.
func (r *Receiver) handle(w http.ResponseWriter, req *http.Request) (string, *spokesync.Response, error) {
	if req.URL.Path != spokesync.Path || req.URL.RawQuery != "" {
		return "", nil, reject(http.StatusNotFound, "unknown path")
	}
	if req.Method != http.MethodPost {
		return "", nil, reject(http.StatusMethodNotAllowed, "unknown method")
	}
	if !validContentType(req.Header) {
		return "", nil, reject(http.StatusUnsupportedMediaType, "unknown content type")
	}
	identity, err := Identity(req.TLS)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %w", reject(http.StatusForbidden, "client certificate not accepted"), err)
	}
	// The limits apply before the body is read, so that a cert whose identity is no longer registered
	// cannot use all the memory, and before the reads of the API server, so that one spoke cannot load the
	// API server or keep the receiver busy for the other spokes.
	if err := r.allowRequest(identity); err != nil {
		return identity, nil, err
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		return identity, nil, reject(http.StatusServiceUnavailable, "too many requests in progress")
	}
	if !r.claim(r.callers, identity) {
		return identity, nil, reject(http.StatusConflict, "a request with this identity is in progress")
	}
	defer r.release(r.callers, identity)
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, r.maxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return identity, nil, reject(http.StatusRequestEntityTooLarge, "the body is too large")
		}
		return identity, nil, fmt.Errorf("%w: %w", reject(http.StatusBadRequest, "cannot read the body"), err)
	}
	sreq, err := decodeRequest(body, valueLimit(r.maxPolicies))
	if err != nil {
		return identity, nil, reject(http.StatusBadRequest, "invalid request: "+err.Error())
	}
	if err := validateRequest(sreq, r.maxPolicies); err != nil {
		return identity, nil, err
	}
	resp, err := r.sync(req.Context(), identity, sreq)
	return identity, resp, err
}

// validContentType accepts only spokesync.ContentType, with no parameter or with charset=utf-8. It rejects
// each content coding.
func validContentType(h http.Header) bool {
	if ce := h.Values("Content-Encoding"); len(ce) > 0 && (len(ce) > 1 || ce[0] != "identity") {
		return false
	}
	ct := h.Values("Content-Type")
	if len(ct) != 1 {
		return false
	}
	mt, params, err := mime.ParseMediaType(ct[0])
	if err != nil || mt != spokesync.ContentType {
		return false
	}
	for k, v := range params {
		if k != "charset" || (v != "utf-8" && v != "UTF-8") {
			return false
		}
	}
	return true
}

// Identity returns the identity of a verified client: the only URI SAN of its leaf cert (PLAN §18.5).
func Identity(cs *tls.ConnectionState) (string, error) {
	if cs == nil || len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 {
		return "", errors.New("no verified client certificate")
	}
	uris := cs.PeerCertificates[0].URIs
	switch len(uris) {
	case 0:
		return "", errors.New("the client certificate has no URI SAN")
	case 1:
	default:
		return "", errors.New("the client certificate has more than one URI SAN")
	}
	// CertIdentity gives the same text form that spokesync.IdentityMatchesName and the compiler use.
	id := spokesync.CertIdentity(uris[0])
	if id == "" {
		return "", errors.New("the URI SAN of the client certificate is empty")
	}
	return id, nil
}

// claim marks key in set. It returns false if key is marked already. A second sync must not wait, because a
// queue could let an old state overwrite a new state (PLAN §18.5).
func (r *Receiver) claim(set map[string]bool, key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if set[key] {
		return false
	}
	set[key] = true
	return true
}

func (r *Receiver) release(set map[string]bool, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(set, key)
}

// bucket is the token bucket of one identity.
type bucket struct {
	tokens  float64
	at      time.Time // time of the last update of tokens
	limited bool      // the last request was rejected
}

// allowRequest takes one token from the bucket of identity. A bucket holds up to r.burst tokens and gets one
// token for each r.interval. It rejects the request if the bucket is empty.
func (r *Receiver) allowRequest(identity string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b, ok := r.buckets[identity]
	if !ok {
		b = bucket{tokens: float64(r.burst), at: now}
	}
	if elapsed := now.Sub(b.at); elapsed > 0 {
		b.tokens = min(float64(r.burst), b.tokens+float64(elapsed)/float64(r.interval))
	}
	b.at = now
	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	}
	repeated := !allowed && b.limited
	b.limited = !allowed
	r.buckets[identity] = b
	r.pruneBuckets(now)
	if !allowed {
		// The first rejection of a series goes to the log at info level, the others at debug level, so that
		// a spoke in a loop cannot fill the log of the hub.
		return &rejection{status: http.StatusTooManyRequests, msg: "too many requests", quiet: repeated}
	}
	return nil
}

// pruneBuckets removes the buckets that are full again at now: a new bucket has the same state. It runs at
// most once for each time that an empty bucket needs to become full, so the map holds only the identities
// of that time. The caller holds r.mu.
func (r *Receiver) pruneBuckets(now time.Time) {
	full := time.Duration(r.burst) * r.interval
	if now.Sub(r.lastPrune) < full {
		return
	}
	r.lastPrune = now
	for id, b := range r.buckets {
		if b.tokens+float64(now.Sub(b.at))/float64(r.interval) >= float64(r.burst) {
			delete(r.buckets, id)
		}
	}
}

// allowChange records the start of a changed sync. It rejects the sync if the last changed sync of the
// cluster started less than ChangeInterval ago. It records the time before apply on purpose: a sync that
// then fails, for example with 409, also delays the retry. PLAN §18.6 accepts this delay of one retry.
func (r *Receiver) allowChange(cluster string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if last, ok := r.lastChange[cluster]; ok && now.Sub(last) < ChangeInterval {
		return reject(http.StatusTooManyRequests, "too many changed syncs")
	}
	r.lastChange[cluster] = now
	return nil
}

// lookup reads the SpokeCluster objects directly from the API server and returns the one for identity. It
// rejects an unknown identity, an identity on more than one object, a name that is not the last path segment
// of the identity, a suspended cluster and a cluster with a deletion timestamp.
func (r *Receiver) lookup(ctx context.Context, identity string) (*v1.SpokeCluster, error) {
	var list v1.SpokeClusterList
	if err := r.c.List(ctx, &list); err != nil {
		return nil, internalError(err)
	}
	var match *v1.SpokeCluster
	n := 0
	for i := range list.Items {
		if list.Items[i].Spec.Identity == identity {
			match = &list.Items[i]
			n++
		}
	}
	switch {
	case n == 0:
		return nil, reject(http.StatusForbidden, "unknown identity")
	case n > 1:
		return nil, reject(http.StatusForbidden, "the identity is on more than one SpokeCluster")
	case !spokesync.IdentityMatchesName(identity, match.Name):
		return nil, reject(http.StatusForbidden, "the identity does not agree with the SpokeCluster name")
	case len(validation.IsValidLabelValue(match.Name)) > 0:
		return nil, reject(http.StatusForbidden, "the SpokeCluster name is not a valid label value")
	case match.Spec.Suspended:
		return nil, reject(http.StatusForbidden, "the SpokeCluster is suspended")
	case match.DeletionTimestamp != nil:
		return nil, reject(http.StatusForbidden, "the SpokeCluster has a deletion timestamp")
	}
	return match, nil
}

// storedState is the state of one cluster as the API server stores it.
type storedState struct {
	own    map[string]*v1.SpokePolicy // objects with spec.cluster equal to the cluster, by name
	others map[string]bool            // names of objects of other clusters
	digest string                     // empty if the stored objects have no valid digest
	clean  bool                       // all own objects have the expected name, label and owner
}

// readStored lists all SpokePolicy objects directly and selects them by spec.cluster. The label is not a
// trusted filter: an object with a wrong label must still count for its cluster.
func (r *Receiver) readStored(ctx context.Context, sc *v1.SpokeCluster) (*storedState, error) {
	var list v1.SpokePolicyList
	if err := r.c.List(ctx, &list); err != nil {
		return nil, internalError(err)
	}
	st := &storedState{own: map[string]*v1.SpokePolicy{}, others: map[string]bool{}, clean: true}
	var sources []v1.SpokePolicySource
	for i := range list.Items {
		o := &list.Items[i]
		if o.Spec.Cluster != sc.Name {
			st.others[o.Name] = true
			continue
		}
		st.own[o.Name] = o
		sources = append(sources, o.Spec.SpokePolicySource)
		if !conforms(sc, o) {
			st.clean = false
		}
	}
	if d, err := spokesync.Digest(sources); err == nil {
		st.digest = d
	} else {
		st.clean = false
	}
	return st, nil
}

// ownerRef is the owner reference of the SpokePolicy objects of sc.
func ownerRef(sc *v1.SpokeCluster) metav1.OwnerReference {
	ctl := true
	return metav1.OwnerReference{
		APIVersion: v1.GroupVersion.String(),
		Kind:       "SpokeCluster",
		Name:       sc.Name,
		UID:        sc.UID,
		Controller: &ctl,
	}
}

// conforms reports whether a stored object of sc has the form that the receiver writes.
func conforms(sc *v1.SpokeCluster, o *v1.SpokePolicy) bool {
	if o.DeletionTimestamp != nil || o.Name != spokesync.ObjectName(sc.Name, &o.Spec.SpokePolicySource) ||
		o.Labels[spokesync.ClusterLabel] != sc.Name || len(o.OwnerReferences) != 1 {
		return false
	}
	got, want := o.OwnerReferences[0], ownerRef(sc)
	return got.APIVersion == want.APIVersion && got.Kind == want.Kind && got.Name == want.Name &&
		got.UID == want.UID && got.Controller != nil && *got.Controller && got.BlockOwnerDeletion == nil
}

// sameSource reports whether two policies have the same digest, so that defaults that the API server
// applies do not count as a change.
func sameSource(a, b *v1.SpokePolicySource) bool {
	da, err1 := spokesync.Digest([]v1.SpokePolicySource{*a})
	db, err2 := spokesync.Digest([]v1.SpokePolicySource{*b})
	return err1 == nil && err2 == nil && da == db
}

// sync runs one sync of a validated request.
func (r *Receiver) sync(ctx context.Context, identity string, req *spokesync.Request) (*spokesync.Response, error) {
	// The first read selects the cluster for the lock. The second read, under the lock, is the start of
	// the sync: all checks and the final status write use it.
	first, err := r.lookup(ctx, identity)
	if err != nil {
		return nil, err
	}
	if !r.claim(r.running, first.Name) {
		return nil, reject(http.StatusConflict, "a sync for this cluster is in progress")
	}
	defer r.release(r.running, first.Name)
	sc, err := r.lookup(ctx, identity)
	if err != nil {
		return nil, err
	}
	if sc.Name != first.Name || sc.UID != first.UID {
		return nil, reject(http.StatusConflict, "the SpokeCluster changed during the sync")
	}

	st, err := r.readStored(ctx, sc)
	if err != nil {
		return nil, err
	}
	unchanged := st.clean && st.digest == req.Digest
	if !req.Full && !unchanged {
		return &spokesync.Response{Result: spokesync.ResultFullStateRequired, Conditions: conditions(st.own, nil)}, nil
	}
	keep := st.own
	if !unchanged {
		if err := r.allowChange(sc.Name); err != nil {
			return nil, err
		}
		if keep, err = r.apply(ctx, sc, st, req.Policies); err != nil {
			return nil, err
		}
	}
	if err := r.renew(ctx, sc); err != nil {
		return nil, err
	}
	return &spokesync.Response{Result: spokesync.ResultOK, Conditions: conditions(st.own, keep)}, nil
}

// apply makes the stored state of sc equal to policies: it writes new and changed objects first and then
// deletes the objects that are not in the state (PLAN §18.5). It never changes or deletes an object of a
// different cluster. Before each write it reads the SpokeCluster again, so that a change of the
// SpokeCluster stops the sync. It returns the stored objects that stay in the state.
func (r *Receiver) apply(ctx context.Context, sc *v1.SpokeCluster, st *storedState, policies []v1.SpokePolicySource) (map[string]*v1.SpokePolicy, error) {
	type item struct {
		name string
		src  *v1.SpokePolicySource
	}
	items := make([]item, 0, len(policies))
	desired := make(map[string]bool, len(policies))
	for i := range policies {
		n := spokesync.ObjectName(sc.Name, &policies[i])
		items = append(items, item{n, &policies[i]})
		desired[n] = true
		if st.others[n] {
			return nil, internalError(fmt.Errorf("object %s belongs to a different cluster", n))
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })

	keep := map[string]*v1.SpokePolicy{}
	for _, it := range items {
		cur := st.own[it.name]
		if cur != nil {
			keep[it.name] = cur
			if conforms(sc, cur) && sameSource(&cur.Spec.SpokePolicySource, it.src) {
				continue
			}
			if cur.DeletionTimestamp != nil {
				return nil, internalError(fmt.Errorf("object %s has a deletion timestamp", it.name))
			}
			if spokesync.Key(&cur.Spec.SpokePolicySource) != spokesync.Key(it.src) {
				// Only someone with write access to the hub can make such an object. The CEL rules make
				// kind, namespace and name immutable, so an update fails. The spoke cannot repair it, and
				// the fault is not in its policy, so the answer is 500 and the log names the object.
				return nil, internalError(fmt.Errorf("object %s has the name of a different policy; delete it", it.name))
			}
		}
		if err := r.guard(ctx, sc); err != nil {
			return nil, err
		}
		obj := &v1.SpokePolicy{}
		if cur != nil {
			obj = cur.DeepCopy()
		}
		obj.Name = it.name
		if obj.Labels == nil {
			obj.Labels = map[string]string{}
		}
		obj.Labels[spokesync.ClusterLabel] = sc.Name
		obj.OwnerReferences = []metav1.OwnerReference{ownerRef(sc)}
		obj.Spec = v1.SpokePolicySpec{Cluster: sc.Name, SpokePolicySource: *it.src.DeepCopy()}
		var err error
		if cur == nil {
			err = r.c.Create(ctx, obj)
		} else {
			err = r.c.Update(ctx, obj) // the resourceVersion of the read is the precondition
		}
		if err != nil {
			return nil, apiError(err)
		}
	}

	if r.afterWrites != nil {
		if err := r.afterWrites(ctx, sc.Name); err != nil {
			return nil, internalError(err)
		}
	}

	stale := make([]*v1.SpokePolicy, 0)
	for n, o := range st.own {
		if !desired[n] {
			stale = append(stale, o)
		}
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].Name < stale[j].Name })
	for _, o := range stale {
		if err := r.guard(ctx, sc); err != nil {
			return nil, err
		}
		uid, rv := o.UID, o.ResourceVersion
		err := r.c.Delete(ctx, o, client.Preconditions{UID: &uid, ResourceVersion: &rv})
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, apiError(err)
		}
	}
	return keep, nil
}

// guard rejects the sync if the SpokeCluster changed since the start of the sync.
func (r *Receiver) guard(ctx context.Context, sc *v1.SpokeCluster) error {
	var cur v1.SpokeCluster
	if err := r.c.Get(ctx, client.ObjectKey{Name: sc.Name}, &cur); err != nil {
		return apiError(err)
	}
	if cur.UID != sc.UID || cur.ResourceVersion != sc.ResourceVersion {
		return reject(http.StatusConflict, "the SpokeCluster changed during the sync")
	}
	return nil
}

// renew writes lastSyncTime with the hub clock and observedGeneration from the read at the start of the
// sync. The resourceVersion of that read is the precondition, so a sync that overlaps a change of the
// SpokeCluster validates nothing.
func (r *Receiver) renew(ctx context.Context, sc *v1.SpokeCluster) error {
	obj := sc.DeepCopy()
	t := metav1.NewTime(r.now())
	obj.Status.LastSyncTime = &t
	obj.Status.ObservedGeneration = sc.Generation
	if err := r.c.Status().Update(ctx, obj); err != nil {
		return apiError(err)
	}
	return nil
}

// conditions returns the Ready conditions of the stored objects, restricted to keep if keep is not nil. It
// replaces CrossTenantUnionUnsupported, because that condition can show the grants of a shared subject in
// other tenants (PLAN §18.5). The policy itself is active in that case.
func conditions(own, keep map[string]*v1.SpokePolicy) []spokesync.Condition {
	out := make([]spokesync.Condition, 0, len(own))
	for n, o := range own {
		if keep != nil && keep[n] == nil {
			continue
		}
		c := apimeta.FindStatusCondition(o.Status.Conditions, controller.ReadyCondition)
		if c == nil || o.Status.SourceGeneration < 1 {
			continue
		}
		ready := c.Status == metav1.ConditionTrue
		reason, msg := c.Reason, c.Message
		if reason == policy.ReasonCrossTenantUnionUnsupported {
			if !ready {
				continue
			}
			reason, msg = policy.ReasonCompiled, "policy is active"
		}
		out = append(out, spokesync.Condition{
			Kind:             o.Spec.Kind,
			Namespace:        o.Spec.Namespace,
			Name:             o.Spec.Name,
			SourceGeneration: o.Status.SourceGeneration,
			Ready:            ready,
			Reason:           reason,
			Message:          msg,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return string(a.Kind)+"\x00"+a.Namespace+"\x00"+a.Name < string(b.Kind)+"\x00"+b.Namespace+"\x00"+b.Name
	})
	return out
}
