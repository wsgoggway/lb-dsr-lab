// Package health probes table members (http/tcp/icmp), keeps per-member
// UP/DOWN state machines and combines local and peer views.
//
// Pure library: no logging, no goroutines — the caller (daemon) schedules
// probes and logs results.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/itchyny/gojq"

	"github.com/wsgoggway/lb-dsr-lab/pfkeeper/internal/state"
)

// Member states as exposed through the gRPC API.
const (
	StateUP      = "UP"
	StateDOWN    = "DOWN"
	StateUnknown = "INACTIVE" // no consensus yet (before rise/fall settles)
)

// maxBodyExcerpt bounds the body kept for inspection (4 KiB per the API).
const maxBodyExcerpt = 4096

// maxJQOutput bounds the marshalled jq output kept in Result.
const maxJQOutput = 1024

// Failure reasons (Reason field; "" on pass).
const (
	ReasonStatus     = "status_code"
	ReasonJSONInvald = "json_invalid"
	ReasonJQFalse    = "jq_false"
	ReasonJQError    = "jq_error"
	ReasonTimeout    = "timeout"
	ReasonConnect    = "connect"
)

// Result is the outcome of a single probe. Failures are encoded in the
// struct (Passed=false + Reason); Probe implementations do not return
// operational errors.
type Result struct {
	State       string // StateUP | StateDOWN after this probe (raw verdict, pre-monitor)
	Passed      bool
	Reason      string // "" on pass; see Reason* constants
	StatusCode  int    // http: last response status; 0 otherwise
	BodyExcerpt string // http: up to 4096 bytes of the body
	JQOutput    string // http: json-marshalled jq output, truncated to 1 KiB
	CheckedAt   time.Time
	Duration    time.Duration
}

// Prober checks one address against a spec.
type Prober interface {
	Probe(ctx context.Context, addr string, spec state.HealthSpec) Result
}

// jqCache memoises compiled gojq queries by expression string. gojq.Query is
// immutable after Parse, so sharing compiled values across probes is safe.
var jqCache sync.Map // string -> *gojq.Query

func compileJQ(expr string) (*gojq.Query, error) {
	if v, ok := jqCache.Load(expr); ok {
		q, _ := v.(*gojq.Query)
		return q, nil
	}
	q, err := gojq.Parse(expr)
	if err != nil {
		return nil, err
	}
	jqCache.Store(expr, q)
	return q, nil
}

// MatchStatus reports whether code matches any entry of codes. Entries:
//
//	"200"     — exact code;
//	"200-399" — inclusive range;
//	"2xx"     — hundred class, case-insensitive.
//
// An empty list defaults to "2xx". Unparsable entries never match.
func MatchStatus(codes []string, code int) bool {
	entries := codes
	if len(entries) == 0 {
		entries = []string{"2xx"}
	}
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		switch {
		case strings.HasSuffix(strings.ToLower(entry), "xx"):
			if class, err := strconv.Atoi(entry[:len(entry)-2]); err == nil {
				if code/100 == class {
					return true
				}
			}
		case strings.Contains(entry, "-"):
			lo, hi, _ := strings.Cut(entry, "-")
			low, errLo := strconv.Atoi(lo)
			high, errHi := strconv.Atoi(hi)
			if errLo == nil && errHi == nil && code >= low && code <= high {
				return true
			}
		default:
			if want, err := strconv.Atoi(entry); err == nil && code == want {
				return true
			}
		}
	}
	return false
}

// HTTPProber probes http://addr:port/path and evaluates status + optional jq.
type HTTPProber struct {
	// Timeout caps each request when > 0; otherwise spec.Timeout is used
	// (5s fallback when both are unset/unparsable).
	Timeout time.Duration
}

// Probe runs one http health check. Operational failures are encoded in the
// returned Result (see Reason* constants), never as errors.
func (p HTTPProber) Probe(ctx context.Context, addr string, spec state.HealthSpec) Result {
	res := Result{CheckedAt: time.Now()}

	timeout := p.Timeout
	if timeout <= 0 {
		if d, err := time.ParseDuration(spec.Timeout); err == nil && d > 0 {
			timeout = d
		} else {
			timeout = 5 * time.Second
		}
	}
	ctrCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	path := spec.Path
	if path == "" || !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	url := fmt.Sprintf("http://%s:%d%s", addr, spec.Port, path)

	req, err := http.NewRequestWithContext(ctrCtx, http.MethodGet, url, nil)
	if err != nil {
		res.Reason = ReasonConnect
		res.Duration = time.Since(res.CheckedAt)
		return res
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	res.Duration = time.Since(res.CheckedAt)
	if err != nil {
		res.Reason = classifyTransportErr(err)
		return res
	}
	defer resp.Body.Close() //nolint:errcheck // read-side cleanup

	res.StatusCode = resp.StatusCode
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyExcerpt))
	res.BodyExcerpt = string(body)

	if !MatchStatus(spec.Expect.Status, resp.StatusCode) {
		res.Reason = ReasonStatus
		return res
	}

	if spec.Expect.JQ == "" {
		res.Passed = true
		res.State = StateUP
		return res
	}
	return applyJQ(&res, spec.Expect.JQ, body)
}

// applyJQ evaluates the jq expression over the body and folds the verdict
// into res (jq truthiness: pass iff the LAST output value is non-nil, non-false).
func applyJQ(res *Result, expr string, body []byte) Result {
	var input any
	if err := json.Unmarshal(body, &input); err != nil {
		res.Reason = ReasonJSONInvald
		return *res
	}
	query, err := compileJQ(expr)
	if err != nil {
		res.Reason = ReasonJQError
		return *res
	}
	var last any
	iter := query.Run(input)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, isErr := v.(error); isErr {
			res.Reason = ReasonJQError
			_ = err // jq runtime error (e.g. type mismatch); reason is enough
			return *res
		}
		last = v
	}
	if last == nil {
		res.Reason = ReasonJQFalse
		return *res
	}
	if b, ok := last.(bool); ok && !b {
		res.Reason = ReasonJQFalse
		return *res
	}
	out, err := json.Marshal(last)
	if err != nil {
		out = []byte(fmt.Sprintf("%v", last))
	}
	res.JQOutput = truncate(string(out), maxJQOutput)
	res.Passed = true
	res.State = StateUP
	return *res
}

// classifyTransportErr maps an http.Client error to timeout|connect.
func classifyTransportErr(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ReasonTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ReasonTimeout
	}
	return ReasonConnect
}

// truncate cuts s to at most limit bytes without splitting a UTF-8 rune.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && (s[cut]&0xC0) == 0x80 { // back off onto a rune start
		cut--
	}
	return s[:cut]
}

// TCPProber checks TCP connectivity to addr:port.
type TCPProber struct {
	// Timeout caps each dial when > 0; otherwise spec.Timeout (5s fallback).
	Timeout time.Duration
}

// Probe dials addr:port; success passes, any failure is reason "connect" or
// "timeout".
func (p TCPProber) Probe(ctx context.Context, addr string, spec state.HealthSpec) Result {
	res := Result{CheckedAt: time.Now()}

	timeout := p.Timeout
	if timeout <= 0 {
		if d, err := time.ParseDuration(spec.Timeout); err == nil && d > 0 {
			timeout = d
		} else {
			timeout = 5 * time.Second
		}
	}
	dialer := &net.Dialer{Timeout: timeout}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := dialer.DialContext(dialCtx, "tcp", net.JoinHostPort(addr, strconv.Itoa(spec.Port)))
	res.Duration = time.Since(res.CheckedAt)
	if err != nil {
		res.Reason = classifyTransportErr(err)
		return res
	}
	conn.Close() //nolint:errcheck // probe-only connection
	res.Passed = true
	res.State = StateUP
	return res
}

// ICMPProber is the v1 stub: it shells out to `ping -c1 -W <timeout> addr`
// and passes when the command exits 0.
//
// Limitation: this is the FreeBSD ping syntax; on FreeBSD -W is wait time in
// MILLISECONDS, so a spec timeout of "2s" here actually waits 2ms per reply
// unless the caller scales it — v1 keeps the spec's literal seconds argument
// as pinned by the design doc; revisit in M4 with a sysctl/priv-portal
// implementation.
type ICMPProber struct {
	// Bin overrides the ping binary (default "ping").
	Bin string
}

// Probe pings addr once; success passes, failure is reason "connect" (icmp
// has no richer failure taxonomy in v1).
func (p ICMPProber) Probe(ctx context.Context, addr string, spec state.HealthSpec) Result {
	res := Result{CheckedAt: time.Now()}
	bin := p.Bin
	if bin == "" {
		bin = "ping"
	}
	wait := spec.Timeout
	if wait == "" {
		wait = "1"
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, "-c", "1", "-W", wait, addr).CombinedOutput()
	res.Duration = time.Since(res.CheckedAt)
	if err != nil {
		res.Reason = ReasonConnect
		return res
	}
	_ = out
	res.Passed = true
	res.State = StateUP
	return res
}

// Pick returns the prober for a spec type, nil for unknown types.
func Pick(spec state.HealthSpec) Prober {
	switch spec.Type {
	case "http":
		return HTTPProber{}
	case "tcp":
		return TCPProber{}
	case "icmp":
		return ICMPProber{}
	default:
		return nil
	}
}
