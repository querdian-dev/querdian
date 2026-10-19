package main

import (
	"math"
	"time"
)

// The demo data is produced by one small model of two services. Everything is a function of the
// AGE of a point in time (how long before the end of the history it is), never of the wall clock,
// so a run with the same seed always has the same shape: the same diurnal wave, and the incidents
// at the same offsets before "now".

// step is one child span of a request: a database call, a cache call or a downstream service.
type step struct {
	Name   string  // span name
	Dep    string  // db, cache, payment or provider; incidents act on dependencies
	Median float64 // median duration in milliseconds
	P      float64 // probability that the step runs at all (cache misses and the like)
	Attrs  [][2]string
}

type route struct {
	Method, Path string
	OKStatus     int
	RPS          float64 // requests per second at age 0
	NotFound     bool    // client errors are 404 instead of 400
	Steps        []step
}

func (r *route) key() string { return r.Method + " " + r.Path }

// hasDep reports whether the route calls the dependency.
func (r *route) hasDep(dep string) bool {
	for _, s := range r.Steps {
		if s.Dep == dep {
			return true
		}
	}
	return false
}

// incident degrades dependencies for a while. All offsets are ages (time before the end).
type incident struct {
	Name       string
	Start, End time.Duration // Start is the older edge
	Ramp       time.Duration
	Lat        map[string]float64 // latency multiplier per dependency at full strength
	Extra5xx   float64            // extra probability of a server error at full strength
	Downstream bool               // the errors are 503s from a downstream call, otherwise 500s
	AllRoutes  bool
	Sat        float64 // saturation (pool utilisation, connections, queue) at full strength
	Message    string  // what the service logs when its circuit breaker opens
}

// weight is 0 outside the incident, 1 in its core and ramps linearly in between.
func (i *incident) weight(age time.Duration) float64 {
	if age >= i.Start || age <= i.End {
		return 0
	}
	w := 1.0
	if d := i.Start - age; d < i.Ramp {
		w = float64(d) / float64(i.Ramp)
	}
	if d := age - i.End; d < i.Ramp {
		w = math.Min(w, float64(d)/float64(i.Ramp))
	}
	return w
}

func (i *incident) applies(r *route) bool {
	if i.AllRoutes {
		return true
	}
	for dep := range i.Lat {
		if r.hasDep(dep) {
			return true
		}
	}
	return false
}

type service struct {
	Name, Namespace, Pod, Instance string
	Format                         string // "json" or "logfmt"
	Routes                         []route
	Incidents                      []incident
	Customers                      [3]float64 // share of requests per customer_id
}

// Incident offsets before the end of the history (documented in docs/quickstart.md).
const (
	hour   = time.Hour
	minute = time.Minute
)

func services() []*service {
	order := &service{
		Name: "order-service", Namespace: "order-service", Pod: "order-service-6d9c8b7f5-k4x2p",
		Instance: "10.244.0.31:8080", Format: "json", Customers: [3]float64{0.5, 0.3, 0.2},
		Routes: []route{
			{Method: "GET", Path: "/orders", OKStatus: 200, RPS: 1.5, Steps: []step{
				{Name: "redis GET orders:recent", Dep: "cache", Median: 1.5, P: 1, Attrs: kv("db.system", "redis", "db.operation", "GET")},
				{Name: "SELECT orders", Dep: "db", Median: 34, P: 0.35, Attrs: kv("db.system", "postgresql", "db.name", "orders", "db.operation", "SELECT", "db.statement", "SELECT id, status, total FROM orders WHERE customer = $1 ORDER BY created DESC LIMIT 20")},
			}},
			{Method: "POST", Path: "/orders", OKStatus: 201, RPS: 0.7, Steps: []step{
				{Name: "SELECT stock", Dep: "db", Median: 8, P: 1, Attrs: kv("db.system", "postgresql", "db.name", "orders", "db.operation", "SELECT", "db.statement", "SELECT sku, available FROM stock WHERE sku = ANY($1)")},
				{Name: "POST payment-service /payments", Dep: "payment", Median: 120, P: 1, Attrs: kv("peer.service", "payment-service", "http.request.method", "POST", "url.full", "http://payment-service.payment-service:8080/payments")},
				{Name: "INSERT orders", Dep: "db", Median: 22, P: 1, Attrs: kv("db.system", "postgresql", "db.name", "orders", "db.operation", "INSERT", "db.statement", "INSERT INTO orders (customer, total, status) VALUES ($1, $2, 'paid')")},
				{Name: "redis DEL cart", Dep: "cache", Median: 1.5, P: 1, Attrs: kv("db.system", "redis", "db.operation", "DEL")},
			}},
			{Method: "GET", Path: "/orders/{id}", OKStatus: 200, RPS: 1.3, NotFound: true, Steps: []step{
				{Name: "redis GET order", Dep: "cache", Median: 1.5, P: 1, Attrs: kv("db.system", "redis", "db.operation", "GET")},
				{Name: "SELECT orders by id", Dep: "db", Median: 14, P: 0.3, Attrs: kv("db.system", "postgresql", "db.name", "orders", "db.operation", "SELECT", "db.statement", "SELECT * FROM orders WHERE id = $1")},
			}},
			{Method: "POST", Path: "/orders/{id}/cancel", OKStatus: 200, RPS: 0.12, NotFound: true, Steps: []step{
				{Name: "UPDATE orders", Dep: "db", Median: 20, P: 1, Attrs: kv("db.system", "postgresql", "db.name", "orders", "db.operation", "UPDATE", "db.statement", "UPDATE orders SET status = 'cancelled' WHERE id = $1")},
				{Name: "POST payment-service /payments/{id}/refund", Dep: "payment", Median: 150, P: 1, Attrs: kv("peer.service", "payment-service", "http.request.method", "POST", "url.full", "http://payment-service.payment-service:8080/payments/{id}/refund")},
			}},
			{Method: "GET", Path: "/cart", OKStatus: 200, RPS: 1.0, Steps: []step{
				{Name: "redis GET cart", Dep: "cache", Median: 2, P: 1, Attrs: kv("db.system", "redis", "db.operation", "GET")},
				{Name: "SELECT cart_items", Dep: "db", Median: 9, P: 0.1, Attrs: kv("db.system", "postgresql", "db.name", "orders", "db.operation", "SELECT", "db.statement", "SELECT * FROM cart_items WHERE cart = $1")},
			}},
		},
		Incidents: []incident{
			{Name: "payment-service dependency", Start: 7*hour + 30*minute, End: 6*hour + 30*minute, Ramp: 10 * minute,
				Lat: map[string]float64{"payment": 6}, Extra5xx: 0.10, Downstream: true, Sat: 0.5,
				Message: "payment-service is slow or failing"},
			{Name: "bad deploy", Start: 3*hour + 40*minute, End: 3*hour + 10*minute, Ramp: 5 * minute,
				Lat: map[string]float64{"db": 3}, Extra5xx: 0.18, AllRoutes: true, Sat: 0.8,
				Message: "database connection pool exhausted"},
		},
	}
	pay := &service{
		Name: "payment-service", Namespace: "payment-service", Pod: "payment-service-7b5d9c6f8-q9m7w",
		Instance: "10.244.0.32:8080", Format: "logfmt", Customers: [3]float64{0.4, 0.4, 0.2},
		Routes: []route{
			{Method: "POST", Path: "/payments", OKStatus: 201, RPS: 0.85, Steps: []step{
				{Name: "SELECT payment_methods", Dep: "db", Median: 6, P: 1, Attrs: kv("db.system", "postgresql", "db.name", "payments", "db.operation", "SELECT", "db.statement", "SELECT * FROM payment_methods WHERE customer = $1")},
				{Name: "POST acme-pay /v1/charges", Dep: "provider", Median: 160, P: 1, Attrs: kv("peer.service", "acme-pay", "http.request.method", "POST", "url.full", "https://api.acme-pay.example/v1/charges")},
				{Name: "INSERT payments", Dep: "db", Median: 18, P: 1, Attrs: kv("db.system", "postgresql", "db.name", "payments", "db.operation", "INSERT", "db.statement", "INSERT INTO payments (order_ref, amount, state) VALUES ($1, $2, 'captured')")},
				{Name: "redis SET idempotency", Dep: "cache", Median: 1.5, P: 1, Attrs: kv("db.system", "redis", "db.operation", "SET")},
			}},
			{Method: "GET", Path: "/payments/{id}", OKStatus: 200, RPS: 0.7, NotFound: true, Steps: []step{
				{Name: "redis GET payment", Dep: "cache", Median: 1.5, P: 1, Attrs: kv("db.system", "redis", "db.operation", "GET")},
				{Name: "SELECT payments by id", Dep: "db", Median: 12, P: 0.4, Attrs: kv("db.system", "postgresql", "db.name", "payments", "db.operation", "SELECT", "db.statement", "SELECT * FROM payments WHERE id = $1")},
			}},
			{Method: "POST", Path: "/payments/{id}/refund", OKStatus: 200, RPS: 0.1, NotFound: true, Steps: []step{
				{Name: "SELECT payments by id", Dep: "db", Median: 10, P: 1, Attrs: kv("db.system", "postgresql", "db.name", "payments", "db.operation", "SELECT", "db.statement", "SELECT * FROM payments WHERE id = $1")},
				{Name: "POST acme-pay /v1/refunds", Dep: "provider", Median: 190, P: 1, Attrs: kv("peer.service", "acme-pay", "http.request.method", "POST", "url.full", "https://api.acme-pay.example/v1/refunds")},
				{Name: "UPDATE payments", Dep: "db", Median: 16, P: 1, Attrs: kv("db.system", "postgresql", "db.name", "payments", "db.operation", "UPDATE", "db.statement", "UPDATE payments SET state = 'refunded' WHERE id = $1")},
			}},
			{Method: "GET", Path: "/payment-methods", OKStatus: 200, RPS: 0.5, Steps: []step{
				{Name: "redis GET methods", Dep: "cache", Median: 1.5, P: 1, Attrs: kv("db.system", "redis", "db.operation", "GET")},
				{Name: "SELECT payment_methods", Dep: "db", Median: 8, P: 0.15, Attrs: kv("db.system", "postgresql", "db.name", "payments", "db.operation", "SELECT", "db.statement", "SELECT * FROM payment_methods WHERE customer = $1")},
			}},
			{Method: "POST", Path: "/webhooks/provider", OKStatus: 202, RPS: 0.4, Steps: []step{
				{Name: "INSERT webhook_events", Dep: "db", Median: 14, P: 1, Attrs: kv("db.system", "postgresql", "db.name", "payments", "db.operation", "INSERT", "db.statement", "INSERT INTO webhook_events (kind, body) VALUES ($1, $2)")},
				{Name: "UPDATE payments", Dep: "db", Median: 12, P: 1, Attrs: kv("db.system", "postgresql", "db.name", "payments", "db.operation", "UPDATE", "db.statement", "UPDATE payments SET state = $1 WHERE provider_ref = $2")},
			}},
		},
		Incidents: []incident{
			{Name: "acme-pay slowdown", Start: 7*hour + 30*minute, End: 6*hour + 30*minute, Ramp: 10 * minute,
				Lat: map[string]float64{"provider": 9}, Extra5xx: 0.14, Downstream: true, Sat: 0.9,
				Message: "acme-pay is slow or failing"},
			{Name: "acme-pay blip", Start: 1 * hour, End: 50 * minute, Ramp: 3 * minute,
				Lat: map[string]float64{"provider": 3}, Extra5xx: 0.03, Downstream: true, Sat: 0.3,
				Message: "acme-pay is slow or failing"},
		},
	}
	return []*service{order, pay}
}

func kv(pairs ...string) [][2]string {
	out := make([][2]string, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, [2]string{pairs[i], pairs[i+1]})
	}
	return out
}

// diurnal is the load factor; it is exactly 1 at age 0 so that the live workloads (constant rate)
// continue the history without a step.
func diurnal(age time.Duration) float64 {
	return 1 + 0.3*math.Sin(2*math.Pi*age.Hours()/24)
}

// cond is the state of a route at some age.
type cond struct {
	lat              map[string]float64 // latency multiplier per dependency
	p400, p500, p503 float64
}

func (c cond) mult(dep string) float64 {
	if m, ok := c.lat[dep]; ok {
		return m
	}
	return 1
}

func (s *service) cond(r *route, age time.Duration) cond {
	c := cond{lat: map[string]float64{}, p400: 0.015, p500: 0.003, p503: 0.001}
	for i := range s.Incidents {
		in := &s.Incidents[i]
		w := in.weight(age)
		if w == 0 || !in.applies(r) {
			continue
		}
		for dep, m := range in.Lat {
			c.lat[dep] = c.mult(dep) + w*(m-1)
		}
		if in.Downstream {
			c.p503 += w * in.Extra5xx
		} else {
			c.p500 += w * in.Extra5xx
		}
	}
	return c
}

// median is the median latency of a route in milliseconds under the given conditions.
func (r *route) median(c cond) float64 {
	m := 3.0
	for _, st := range r.Steps {
		m += st.Median * st.P * c.mult(st.Dep)
	}
	return m
}

// saturation is the strongest incident weight times its saturation, in [0, 1].
func (s *service) saturation(age time.Duration) float64 {
	v := 0.0
	for i := range s.Incidents {
		in := &s.Incidents[i]
		v = math.Max(v, in.weight(age)*in.Sat)
	}
	return v
}

// logRate is the rate of sampled access-log lines per second at age 0.
const logRate = 0.30
