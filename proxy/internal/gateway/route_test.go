package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/JuliusBrussee/caveman/proxy/providers"
	"github.com/JuliusBrussee/caveman/proxy/providers/anthropic"
)

func TestSetModelSwapsOnlyTheTopLevelModel(t *testing.T) {
	body := []byte(`{ "messages": [{"role":"user","content":"model"}], "model" : "claude-opus-5-5", "metadata": {"model": "x"} }`)
	out, ok := setModel(body, "claude-sonnet-5-5")
	if !ok {
		t.Fatal("setModel refused a body with a top-level model")
	}
	want := `{ "messages": [{"role":"user","content":"model"}], "model" : "claude-sonnet-5-5", "metadata": {"model": "x"} }`
	if string(out) != want {
		t.Fatalf("setModel changed more than the model value:\n got %s\nwant %s", out, want)
	}
	for _, refused := range []string{`{"messages":[],"metadata":{"model":"x"}}`, `{"model":7}`, `[{"model":"a"}]`, `not json`} {
		if out, ok := setModel([]byte(refused), "m"); ok || string(out) != refused {
			t.Errorf("setModel(%s) = %s, %v; want the body unchanged and false", refused, out, ok)
		}
	}
}

// fakeCloud decides on its own goroutine, reading the body while the handler
// compresses, as cloudlink.Link does, so -race sees the overlap. rejects
// records each Reject call's refused flag.
type fakeCloud struct {
	mu       sync.Mutex
	answer   RouteAnswer
	asks     []RouteAsk
	observed []RequestRecord
	rejects  []bool
}

func (f *fakeCloud) Ask(_ context.Context, ask RouteAsk) func() RouteAnswer {
	f.mu.Lock()
	f.asks = append(f.asks, ask)
	answer := f.answer
	f.mu.Unlock()
	done := make(chan RouteAnswer, 1)
	go func() {
		_ = LatestHumanText(ask.Provider, ask.Endpoint, ask.Body)
		if answer.Model != "" {
			answer.Reject = func(refused bool) {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.rejects = append(f.rejects, refused)
			}
		}
		done <- answer
	}()
	return func() RouteAnswer { return <-done }
}

func (f *fakeCloud) Observe(rec RequestRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observed = append(f.observed, rec)
}

// routeServer is an Anthropic proxy whose upstream records each model it was
// sent and answers rejectModel with a 400.
func routeServer(t *testing.T, cloud CloudLink, rejectModel string) (*Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	models := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &req)
		mu.Lock()
		models = append(models, req.Model)
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		if req.Model == rejectModel {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"no"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"m","type":"message","model":"`+req.Model+`","content":[],"usage":{"input_tokens":10,"output_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)
	return New(Config{
		// The provider's own origin (routing skips any other), served by the stub.
		Adapters:   []providers.Adapter{anthropic.New("https://api.anthropic.com")},
		Auth:       stubAuth{rc: RequestContext{Label: "local", RuntimeMode: "record"}},
		Creds:      stubCreds{key: "sk-byok"},
		Sink:       &captureSink{},
		HTTPClient: &http.Client{Transport: toStub(upstream.URL)},
		Cloud:      cloud,
	}), &models
}

// toStub sends every upstream request to the stub server instead.
func toStub(stub string) http.RoundTripper {
	target, _ := url.Parse(stub)
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(r)
	})
}

func sendMessages(t *testing.T, srv *Server, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":5,"messages":[{"role":"user","content":"fix the bug"}]}`))
	req.Header.Set("x-api-key", "sk-ant-api-key")
	req.Header.Set("x-cave-agent", "claude")
	for name, value := range header {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	return rec
}

func TestRouteStageMovesTheModelAndRecordsBoth(t *testing.T) {
	cloud := &fakeCloud{answer: RouteAnswer{Model: "claude-sonnet-5-5", Outcome: "routed", Reason: "ranked", DecisionID: "0b9f6e4e-3b1a-4c7e-9a4e-1d2c3b4a5f60"}}
	srv, models := routeServer(t, cloud, "")
	rec := sendMessages(t, srv, nil)
	if got := *models; len(got) != 1 || got[0] != "claude-sonnet-5-5" {
		t.Fatalf("upstream models = %v, want [claude-sonnet-5-5]", got)
	}
	if rec.Header().Get("x-caveman-routed-from") != "claude-opus-5-5" {
		t.Errorf("x-caveman-routed-from = %q", rec.Header().Get("x-caveman-routed-from"))
	}
	if len(cloud.asks) != 1 || cloud.asks[0].Model != "claude-opus-5-5" || cloud.asks[0].Agent != "claude" {
		t.Fatalf("asks = %+v", cloud.asks)
	}
	row := cloud.observed[0]
	if row.RouteFrom != "claude-opus-5-5" || row.RouteTo != "claude-sonnet-5-5" || row.Model != "claude-sonnet-5-5" || row.RouteOutcome != "routed" {
		t.Errorf("row route = from %q to %q model %q outcome %q", row.RouteFrom, row.RouteTo, row.Model, row.RouteOutcome)
	}
}

func TestRouteStageFailsOpenToTheAskedModel(t *testing.T) {
	for _, answer := range []RouteAnswer{
		{Outcome: "degraded", Reason: "timeout"},
		{Outcome: "paused", Reason: "allowance"},
		{Outcome: "off"},
		{Model: "claude-opus-5-5", Outcome: "kept"},
	} {
		cloud := &fakeCloud{answer: answer}
		srv, models := routeServer(t, cloud, "")
		rec := sendMessages(t, srv, nil)
		if got := *models; len(got) != 1 || got[0] != "claude-opus-5-5" {
			t.Errorf("%s: upstream models = %v, want the asked model", answer.Outcome, got)
		}
		if rec.Header().Get("x-caveman-routed-from") != "" {
			t.Errorf("%s: routed-from header set", answer.Outcome)
		}
		if cloud.observed[0].RouteOutcome != answer.Outcome {
			t.Errorf("%s: recorded outcome %q", answer.Outcome, cloud.observed[0].RouteOutcome)
		}
	}
}

// A provider that rejects the routed model gets the original request, on the
// model the agent asked for, through the existing original-bytes retry.
func TestRouteStageRejectedModelRetriesTheAskedOne(t *testing.T) {
	cloud := &fakeCloud{answer: RouteAnswer{Model: "claude-sonnet-5-5", Outcome: "routed"}}
	srv, models := routeServer(t, cloud, "claude-sonnet-5-5")
	rec := sendMessages(t, srv, nil)
	if got := *models; len(got) != 2 || got[0] != "claude-sonnet-5-5" || got[1] != "claude-opus-5-5" {
		t.Fatalf("upstream models = %v, want the routed one then the asked one", got)
	}
	if rec.Header().Get("x-caveman-routed-from") != "" {
		t.Error("routed-from header survived the retry on the asked model")
	}
	if row := cloud.observed[0]; row.Model != "claude-opus-5-5" || row.RouteOutcome != "degraded" {
		t.Errorf("row model %q outcome %q, want the asked model and degraded", row.Model, row.RouteOutcome)
	}
}

// The request-wide opt-out never asks.
func TestRouteStageSkipsPassThrough(t *testing.T) {
	cloud := &fakeCloud{answer: RouteAnswer{Model: "claude-sonnet-5-5", Outcome: "routed"}}
	srv, models := routeServer(t, cloud, "")
	sendMessages(t, srv, map[string]string{"x-cave-transforms": "caveman.pass-through.v1"})
	if len(cloud.asks) != 0 {
		t.Fatalf("asks = %d, want none", len(cloud.asks))
	}
	if got := *models; len(got) != 1 || got[0] != "claude-opus-5-5" {
		t.Fatalf("upstream models = %v, want only the asked model", got)
	}
	if len(cloud.observed) != 1 || cloud.observed[0].RouteOutcome != "" {
		t.Errorf("observed = %d rows, outcome %q", len(cloud.observed), cloud.observed[0].RouteOutcome)
	}
}

// A subscription (Pro/Max OAuth) turn asks like an API-key one, in record and
// compress mode alike, and the routed model goes upstream. Its row stays
// tokens-only: the model swap is not a measured saving and carries no dollars.
func TestRouteStageRoutesSubscriptionTurns(t *testing.T) {
	for _, mode := range []string{"record", "compress"} {
		cloud := &fakeCloud{answer: RouteAnswer{Model: "claude-sonnet-5-5", Outcome: "routed"}}
		srv, models := routeServer(t, cloud, "")
		srv.auth = stubAuth{rc: RequestContext{Label: "local", RuntimeMode: mode}}
		srv.compressor = &liveZoneCompressor{} // compress mode takes the subscription pass-through break
		rec := sendMessages(t, srv, map[string]string{"x-api-key": "", "authorization": "Bearer sk-ant-oat01-subscription", "user-agent": "claude-cli/2.1.0"})
		if len(cloud.asks) != 1 {
			t.Fatalf("%s: asks = %d, want one", mode, len(cloud.asks))
		}
		if got := *models; len(got) != 1 || got[0] != "claude-sonnet-5-5" {
			t.Fatalf("%s: upstream models = %v, want the routed one", mode, got)
		}
		if rec.Header().Get("x-caveman-routed-from") != "claude-opus-5-5" {
			t.Errorf("%s: x-caveman-routed-from = %q", mode, rec.Header().Get("x-caveman-routed-from"))
		}
		row := cloud.observed[0]
		if row.AuthMode != "subscription" || row.RouteOutcome != "routed" || row.RouteFrom != "claude-opus-5-5" || row.Model != "claude-sonnet-5-5" {
			t.Errorf("%s: row auth %q outcome %q from %q model %q", mode, row.AuthMode, row.RouteOutcome, row.RouteFrom, row.Model)
		}
		if row.TotalCostUSD != 0 || row.SavingsUSD != 0 || row.RequestTokensBefore != row.RequestTokensAfter ||
			row.RequestEstimatedInputDeltaUSD != nil && *row.RequestEstimatedInputDeltaUSD != 0 {
			t.Errorf("%s: subscription row claims a saving: cost %v savings %v tokens %d->%d delta %v", mode,
				row.TotalCostUSD, row.SavingsUSD, row.RequestTokensBefore, row.RequestTokensAfter, row.RequestEstimatedInputDeltaUSD)
		}
	}
}

// A custom upstream origin (a proxy, Azure, a local server) is never routed.
func TestRouteStageSkipsCustomOrigins(t *testing.T) {
	cloud := &fakeCloud{answer: RouteAnswer{Model: "claude-sonnet-5-5", Outcome: "routed"}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	srv := New(Config{
		Adapters: []providers.Adapter{anthropic.New(upstream.URL)}, Auth: stubAuth{rc: RequestContext{Label: "local", RuntimeMode: "record"}},
		Creds: stubCreds{key: "sk-byok"}, Sink: &captureSink{}, HTTPClient: &http.Client{}, Cloud: cloud,
	})
	sendMessages(t, srv, nil)
	if len(cloud.asks) != 0 || cloud.observed[0].RouteReason != "custom_provider_origin" || cloud.observed[0].ProviderOriginKnown {
		t.Fatalf("asks %d, row %+v", len(cloud.asks), cloud.observed[0])
	}
}

// routedFailures is what an upstream does to a routed request and what must
// follow, on either path. Only a refusal the original bytes then get past
// blames the routed model (Reject(true)); a replay that fails too blames
// nothing; a 401/403 is never replayed; a 5xx or transport error moves the ask
// back without a replay (Reject(false)).
var routedFailures = []struct {
	name     string
	statuses []int // per attempt; nil is a transport error
	calls    int
	rejects  string // fmt of the Reject flags
	code     int
	outcome  string
	reason   string
	asked    bool // the row names the asked model
}{
	{"refused, then served", []int{400, 200}, 2, "[true]", 200, "degraded", "provider_rejected_routed_model", true},
	{"replay fails too", []int{429, 429}, 2, "[]", 429, "degraded", "replay_failed", true},
	{"auth failure", []int{401}, 1, "[]", 401, "routed", "", false},
	{"server error", []int{503}, 1, "[false]", 503, "routed", "", false},
	{"transport error", nil, 1, "[false]", 502, "routed", "", false},
}

// failingUpstream answers each attempt with the next status, or fails the
// transport when statuses is nil.
func failingUpstream(statuses []int, body string) (*captureTransport, http.RoundTripper) {
	rt := &captureTransport{statuses: statuses, responses: []string{body, body}}
	if statuses != nil {
		return rt, rt
	}
	return rt, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		_, _ = rt.RoundTrip(r)
		return nil, errors.New("connection reset")
	})
}

func TestRouteStageFailuresOnTheRoutedModel(t *testing.T) {
	for _, tc := range routedFailures {
		t.Run(tc.name, func(t *testing.T) {
			cloud := &fakeCloud{answer: RouteAnswer{Model: "claude-sonnet-5-5", Outcome: "routed"}}
			srv, _ := routeServer(t, cloud, "")
			rt, transport := failingUpstream(tc.statuses, `{"id":"m","type":"message","content":[],"usage":{"input_tokens":10,"output_tokens":2}}`)
			srv.httpClient = &http.Client{Transport: transport}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":5,"messages":[{"role":"user","content":"fix the bug"}]}`))
			req.Header.Set("authorization", "Bearer sk-ant-oat01-subscription")
			req.Header.Set("user-agent", "claude-cli/2.1.0")
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			row := cloud.observed[0]
			if rec.Code != tc.code || len(rt.bodies) != tc.calls || fmt.Sprint(cloud.rejects) != tc.rejects ||
				row.RouteOutcome != tc.outcome || row.RouteReason != tc.reason || (row.Model == "claude-opus-5-5") != tc.asked {
				t.Fatalf("code %d calls %d rejects %v outcome %q reason %q model %q", rec.Code, len(rt.bodies), cloud.rejects, row.RouteOutcome, row.RouteReason, row.Model)
			}
			if tc.calls == 2 && string(rt.bodies[1]) != `{"model":"claude-opus-5-5","max_tokens":5,"messages":[{"role":"user","content":"fix the bug"}]}` {
				t.Fatalf("replay was not the original bytes: %s", rt.bodies[1])
			}
		})
	}
}

// streamEnds is how a 200 stream from the routed model ends. parts name events
// each path spells in its own protocol; clientStop is how many parts the client
// reads before it goes away (0: all of them); upstreamBreaks fails the upstream
// read after the parts. Only the model failing moves the ask back: a client
// leaving is the user stopping, and a completed answer is no failure however long.
var streamEnds = []struct {
	name           string
	parts          []string
	upstreamBreaks bool
	clientStop     int
	rejects        string
}{
	{"stream breaks before usage", []string{"start", "delta"}, true, 0, "[false]"},
	{"client closes after a complete answer", []string{"start", "delta", "done", "keepalive"}, false, 3, "[]"},
	{"client cancels mid-stream", []string{"start", "delta", "delta", "done"}, false, 2, "[]"},
	{"provider error event with usage", []string{"start", "delta", "failed"}, false, 0, "[false]"},
	{"answer over the capture limit completes, then the stream breaks", []string{"start", "big", "done"}, true, 0, "[]"},
}

// stoppingWriter is a client that goes away after limit bytes: its context is
// canceled and the next write fails, as when the user presses Esc.
type stoppingWriter struct {
	header http.Header
	n      int
	limit  int
	cancel func()
}

func (w *stoppingWriter) Header() http.Header { return w.header }
func (w *stoppingWriter) WriteHeader(int)     {}
func (w *stoppingWriter) Flush()              {}
func (w *stoppingWriter) Write(p []byte) (int, error) {
	if w.limit > 0 && w.n >= w.limit {
		w.cancel()
		return 0, errors.New("broken pipe")
	}
	w.n += len(p)
	return len(p), nil
}

// breaksAtEOF turns the upstream's clean end into a reset.
type breaksAtEOF struct{ r io.Reader }

func (b breaksAtEOF) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		return n, errors.New("upstream reset")
	}
	return n, err
}

// serveStream sends req through srv with the upstream streaming the named parts.
func serveStream(srv *Server, req *http.Request, events map[string]string, parts []string, upstreamBreaks bool, clientStop int) {
	limit := 0
	for i, part := range parts {
		if i < clientStop {
			limit += len(events[part])
		}
	}
	srv.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		_, _ = io.ReadAll(r.Body)
		readers := make([]io.Reader, len(parts))
		for i, part := range parts {
			readers[i] = strings.NewReader(events[part])
		}
		var body io.Reader = io.MultiReader(readers...)
		if upstreamBreaks {
			body = breaksAtEOF{body}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(body), Request: r}, nil
	})}
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	func() {
		defer func() { _ = recover() }() // the handler aborts a broken stream
		srv.Handler().ServeHTTP(&stoppingWriter{header: http.Header{}, limit: limit, cancel: cancel}, req.WithContext(ctx))
	}()
}

var anthropicStream = map[string]string{
	"start":     "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n",
	"delta":     "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
	"big":       "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"" + strings.Repeat("x", chatGPTCaptureLimit) + "\"}}\n\n",
	"done":      "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	"failed":    "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":null},\"usage\":{\"input_tokens\":10,\"output_tokens\":3}}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"x\"}}\n\n",
	"keepalive": ": keepalive\n\n",
}

func TestRouteStageStreamEndsOnTheRoutedModel(t *testing.T) {
	for _, tc := range streamEnds {
		t.Run(tc.name, func(t *testing.T) {
			cloud := &fakeCloud{answer: RouteAnswer{Model: "claude-sonnet-5-5", Outcome: "routed"}}
			srv, _ := routeServer(t, cloud, "")
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":5,"stream":true,"messages":[{"role":"user","content":"fix the bug"}]}`))
			req.Header.Set("authorization", "Bearer sk-ant-oat01-subscription")
			req.Header.Set("user-agent", "claude-cli/2.1.0")
			serveStream(srv, req, anthropicStream, tc.parts, tc.upstreamBreaks, tc.clientStop)
			if fmt.Sprint(cloud.rejects) != tc.rejects {
				t.Fatalf("rejects %v, want %s", cloud.rejects, tc.rejects)
			}
		})
	}
}

// The user stopping before any answer (Esc while the request is in flight) is
// no failure of the routed model, on either path.
func TestRouteStageClientCancelBeforeTheAnswerBlamesNoModel(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/chatgpt/responses"} {
		t.Run(path, func(t *testing.T) {
			var cloud *fakeCloud
			var srv *Server
			body := chatGPTRouteBody
			if path == "/v1/messages" {
				cloud = &fakeCloud{answer: RouteAnswer{Model: "claude-sonnet-5-5", Outcome: "routed"}}
				srv, _ = routeServer(t, cloud, "")
				body = `{"model":"claude-opus-5-5","max_tokens":5,"messages":[{"role":"user","content":"fix the bug"}]}`
			} else {
				cloud = &fakeCloud{answer: RouteAnswer{Model: "gpt-6-sol", Outcome: "routed"}}
				srv = chatGPTRouteServer(cloud, nil, &captureSink{}, DefaultChatGPTUpstream)
			}
			ctx, cancel := context.WithCancel(context.Background())
			srv.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				cancel()
				return nil, context.Canceled
			})}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
			req.Header.Set("authorization", "Bearer sk-ant-oat01-subscription")
			req.Header.Set("user-agent", "claude-cli/2.1.0")
			srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
			if len(cloud.asks) != 1 || len(cloud.rejects) != 0 {
				t.Fatalf("asks %d rejects %v, want one ask and no reject", len(cloud.asks), cloud.rejects)
			}
		})
	}
}

// Compressed and routed, refused: the original bytes getting through proves
// nothing about the model (compression changed the bytes too), so the ask
// moves back without a refusal on record.
func TestRouteStageCompressedAndRoutedRefusalBlamesNoModel(t *testing.T) {
	cloud := &fakeCloud{answer: RouteAnswer{Model: "claude-sonnet-5-5", Outcome: "routed"}}
	srv, _ := routeServer(t, cloud, "")
	rt := &captureTransport{statuses: []int{http.StatusBadRequest, http.StatusOK}, responses: []string{"", `{"id":"m","type":"message","content":[],"usage":{"input_tokens":10,"output_tokens":2}}`}}
	srv.httpClient = &http.Client{Transport: rt}
	srv.auth = stubAuth{rc: RequestContext{Label: "local", RuntimeMode: "compress"}}
	srv.compressor = &liveZoneCompressor{}
	srv.recoveryViaMCP = true
	srv.prefixCache = newTestPrefixCache()
	body := `{"model":"claude-opus-5-5","max_tokens":5,"messages":[{"role":"user","content":"fix it"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + strings.Repeat("claude live tool output ", 60) + `"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer sk-ant-oat01-subscription")
	req.Header.Set("user-agent", "claude-cli/2.1.0")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || len(rt.bodies) != 2 || !strings.Contains(string(rt.bodies[0]), "<<ccr:") || string(rt.bodies[1]) != body {
		t.Fatalf("status %d calls %d, first compressed %v, replay exact %v", rec.Code, len(rt.bodies), len(rt.bodies) > 0 && strings.Contains(string(rt.bodies[0]), "<<ccr:"), len(rt.bodies) == 2 && string(rt.bodies[1]) == body)
	}
	if row := cloud.observed[0]; fmt.Sprint(cloud.rejects) != "[false]" || row.RouteOutcome != "degraded" || row.RouteReason != "replayed_original" || row.Model != "claude-opus-5-5" {
		t.Fatalf("rejects %v, row outcome %q reason %q model %q", cloud.rejects, row.RouteOutcome, row.RouteReason, row.Model)
	}
}

// On the subscription path the routed request differs from the agent's only
// in the model, in every runtime mode.
func TestRouteStageSubscriptionChangesOnlyTheModel(t *testing.T) {
	asked := `{"model":"claude-opus-5-5","max_tokens":5,  "system":[{"type":"text","text":"You are Claude Code"}],"messages":[{"role":"user","content":"fix the bug é"}]}`
	want := strings.Replace(asked, `"claude-opus-5-5"`, `"claude-sonnet-5-5"`, 1)
	for _, mode := range []string{"record", "compress", "pixel", "observe"} {
		cloud := &fakeCloud{answer: RouteAnswer{Model: "claude-sonnet-5-5", Outcome: "routed"}}
		srv, _ := routeServer(t, cloud, "")
		rt := &captureTransport{responses: []string{`{"id":"m","type":"message","content":[],"usage":{"input_tokens":10,"output_tokens":2}}`}}
		srv.httpClient = &http.Client{Transport: rt}
		srv.compressor = &liveZoneCompressor{}
		srv.auth = stubAuth{rc: RequestContext{Label: "local", RuntimeMode: mode}}
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(asked))
		req.Header.Set("authorization", "Bearer sk-ant-oat01-subscription")
		req.Header.Set("user-agent", "claude-cli/2.1.0")
		srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
		if len(rt.bodies) != 1 || string(rt.bodies[0]) != want {
			t.Errorf("%s: upstream body differs beyond the model:\n got %s\nwant %s", mode, rt.bodies, want)
		}
	}
}
