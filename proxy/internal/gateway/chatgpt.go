package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/JuliusBrussee/caveman/proxy/providers"
	"github.com/JuliusBrussee/caveman/proxy/providers/openai"
	"github.com/JuliusBrussee/caveman/shared/platform/httpx"
	"github.com/JuliusBrussee/caveman/shared/platform/id"
)

// DefaultChatGPTUpstream is the ChatGPT-subscription Codex backend. A wrapped
// Codex CLI on a ChatGPT login reaches it through this proxy via a custom
// model_provider with requires_openai_auth=true and base_url .../chatgpt.
const DefaultChatGPTUpstream = "https://chatgpt.com/backend-api/codex"

// chatGPTCaptureLimit caps request transformation and opportunistic response
// parsing. Bigger bodies stream through byte-exact and record no invented counts.
const chatGPTCaptureLimit = 4 << 20

// chatgpt is the ChatGPT-subscription Codex route. It preserves the agent's OAuth
// headers and streams responses unchanged. An entitled local compress run with
// agent-owned MCP recovery may apply the same schema-aware, deterministic live-zone
// compression as the OpenAI Responses adapter; every other case is pass-through.
// Invariants:
//   - no credential resolution, no env fallback — the agent's own OAuth
//     Authorization + ChatGPT-Account-ID headers ride through untouched, and
//     they are never logged, cached, or substituted. A missing credential is
//     the upstream's 401, not ours.
//   - only /responses request content may be transformed, only through the
//     account+MCP+prefix-stability gate, and only its model through the route
//     stage. Any parse/store/shrink/route failure forwards original bytes; a
//     transformed 4xx retries once with original bytes.
//   - response bodies remain byte-identical and SSE streams stay unbuffered.
//   - metering is opportunistic and honest: parseable usage records token
//     counts with TotalCostUSD 0 — subscription traffic has no per-token
//     price, and pricing it at API rates would be a fake number.
func (s *Server) chatgpt(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := id.NewUUIDv7()
	traceID := traceIDFrom(r)
	w.Header().Set("x-cave-request-id", requestID)
	w.Header().Set("x-cave-trace-id", traceID)

	rc, err := s.auth.Authenticate(r.Context(), r)
	if err != nil {
		s.rejectUnauthorized(w, r)
		return
	}
	rc.AgentSlug = labelOrDefault(r.Header.Get("x-cave-agent"), "unlabeled-agent")
	evidence := requestEvidenceFromHeaders(r.Header)
	lockedRoutes, compiledPlanAllowed := compiledPlanRoutes(r.Header)

	suffix := strings.TrimPrefix(r.URL.Path, "/chatgpt")
	if suffix == "" {
		suffix = "/"
	}
	upstreamURL := s.chatGPTUpstream + suffix
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	// Compression needs a complete request. Keep the existing bounded behavior:
	// over-limit bodies stream through unchanged rather than being rejected or held
	// unbounded in memory.
	reqCapture := &cappedBuffer{limit: chatGPTCaptureLimit}
	reqHash := sha256.New()
	var reqBody io.Reader
	var originalBody []byte
	var requestBodyFullyRead bool
	var requestBodyTracker *eofTrackingReader
	transform := providers.TransformResult{OptimizerIDs: []string{}}
	var comp *compressionOutcome
	adapter := openai.New(s.chatGPTUpstream)
	compressEligible := r.Method == http.MethodPost && rc.RuntimeMode == "compress" && suffix == "/responses" &&
		// nil body: this route exists only for subscription wrap, which proves
		// recovery out of band before starting its dedicated proxy.
		s.compressor != nil && s.liveZoneCompressionAllowed(adapter, nil) && compiledPlanAllowed
	// The route stage (proxy.go) with the openai pool, which the ChatGPT plan
	// serves. Same opt-outs: the request-wide pass-through and an encoded body.
	reqContentEncoding := strings.TrimSpace(r.Header.Get("Content-Encoding"))
	routeEligible := s.cloud != nil && r.Method == http.MethodPost && suffix == "/responses" &&
		strings.TrimSpace(r.Header.Get("x-cave-transforms")) != "caveman.pass-through.v1" &&
		(reqContentEncoding == "" || strings.EqualFold(reqContentEncoding, "identity"))
	var route RouteAnswer
	if compressEligible || routeEligible {
		captured, readErr := io.ReadAll(io.LimitReader(r.Body, chatGPTCaptureLimit+1))
		if readErr == nil && len(captured) <= chatGPTCaptureLimit {
			requestBodyFullyRead = true
			originalBody = captured
			_, _ = reqHash.Write(originalBody)
			_, _ = reqCapture.Write(originalBody)
			transform.Body = originalBody
			headersForInspect := r.Header.Clone()
			headersForInspect.Set("x-cave-route-path", suffix)
			meta, inspectErr := adapter.InspectRequest(r.Context(), bytes.NewReader(originalBody), headersForInspect)
			if inspectErr == nil {
				meta.Endpoint = suffix
				meta.SessionID = evidence.SessionID
				var awaitRoute func() RouteAnswer
				switch {
				case !routeEligible:
				case s.chatGPTUpstream != DefaultChatGPTUpstream:
					route = RouteAnswer{Outcome: "off", Reason: "custom_provider_origin"}
				default:
					awaitRoute = s.cloud.Ask(r.Context(), RouteAsk{
						Provider: "openai", Endpoint: suffix, Model: meta.Model, Agent: rc.AgentSlug,
						SessionID: evidence.SessionID, ToolsCount: meta.ToolsCount, InputBytes: len(originalBody), Body: originalBody,
					})
				}
				if compressEligible && s.cacheEpochAllows(r, adapter, meta, originalBody, evidence.SessionID) {
					comp = s.compressRequest(adapter, originalBody, meta, &transform, requestID, lockedRoutes)
				}
				if awaitRoute != nil {
					route = awaitRoute()
					if route.Model != "" && route.Model != meta.Model {
						if routed, ok := setModel(transform.Body, route.Model); ok {
							transform.Body = routed
							w.Header().Set("x-caveman-routed-from", meta.Model)
						} else {
							route = RouteAnswer{Outcome: "degraded", Reason: "model_field_unreadable"}
						}
					}
				}
			}
			reqBody = bytes.NewReader(transform.Body)
		} else {
			// Reconstruct the consumed prefix and continue the old streaming path.
			source := io.MultiReader(bytes.NewReader(captured), r.Body)
			requestBodyTracker = &eofTrackingReader{reader: source}
			reqBody = io.TeeReader(io.TeeReader(requestBodyTracker, reqHash), reqCapture)
			transform.Body = nil
		}
	} else {
		requestBodyTracker = &eofTrackingReader{reader: r.Body}
		reqBody = io.TeeReader(io.TeeReader(requestBodyTracker, reqHash), reqCapture)
	}
	// Compressed, routed or both: the upstream gets other bytes than the agent sent.
	bodyChanged := originalBody != nil && !bytes.Equal(transform.Body, originalBody)
	if r.ContentLength == 0 && (r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodDelete) {
		// A bodyless method with a non-nil reader would go out chunked; keep
		// the wire shape identical to what the agent sent.
		reqBody = nil
	}

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, reqBody)
	if err != nil {
		httpx.Error(w, r, http.StatusBadRequest, "cave_provider_request_invalid", "ChatGPT route could not build the upstream request.")
		return
	}
	if transform.Body != nil {
		upReq.ContentLength = int64(len(transform.Body))
	} else {
		upReq.ContentLength = r.ContentLength
	}
	// Preserve OAuth/account/application headers, but never forward proxy-private
	// metadata or hop-by-hop fields to the subscription backend.
	upReq.Header = chatGPTRequestHeaders(r.Header)
	if bodyChanged {
		upReq.Header.Del("Content-Length")
	}

	// Capture both sides of the transform before the send (see capture.go), off
	// unless CAVE_CAPTURE_DIR is set. Only the fully-read path can do it here; the
	// streaming path has no complete bytes or hash until the body has been
	// forwarded, so it captures after the response instead.
	if originalBody != nil {
		s.capture.record(captureMeta{
			RequestID:   requestID,
			Provider:    "chatgpt-subscription",
			Endpoint:    suffix,
			RuntimeMode: rc.RuntimeMode,
			Optimizers:  strings.Join(transform.OptimizerIDs, ","),
		}, wholeBody(originalBody), wholeBody(transform.Body))
	}

	// The fully-buffered path can rebuild its body per attempt, so transient
	// transport failures retry instead of surfacing a terminal 502. The
	// streaming path has a partially-consumed body and cannot replay.
	var resp *http.Response
	if transform.Body != nil {
		resp, err = s.doUpstream(r.Context(), func() (*http.Request, error) {
			req, buildErr := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(transform.Body))
			if buildErr != nil {
				return nil, buildErr
			}
			req.ContentLength = int64(len(transform.Body))
			req.Header = chatGPTRequestHeaders(r.Header)
			if bodyChanged {
				req.Header.Del("Content-Length")
			}
			return req, nil
		})
	} else {
		resp, err = s.httpClient.Do(upReq)
	}
	if err != nil {
		if route.Outcome == "routed" && r.Context().Err() == nil {
			route.reject(false) // the agent's own retry goes to the asked model
		}
		httpx.Error(w, r, http.StatusBadGateway, "cave_upstream_unreachable", "ChatGPT upstream is unreachable.")
		requestHashComplete := chatGPTRequestHashComplete(requestBodyFullyRead, r.ContentLength, reqCapture, requestBodyTracker)
		s.recordChatGPT(rc, r, requestID, traceID, suffix, start, 0, "cave_upstream_unreachable", reqCapture, reqHash.Sum(nil), transformedChatGPTHash(reqHash.Sum(nil), transform.Body), requestHashComplete, nil, 0, false, transform.OptimizerIDs, comp, compressEligible, transform.Body, route)
		return
	}
	// OAuth backends can reject byte-modified requests for undocumented reasons.
	// Retry once with exact original bytes, then disclose/record no optimization.
	// A 401/403 is about the credential, not the bytes: no replay.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusUnauthorized &&
		resp.StatusCode != http.StatusForbidden && bodyChanged {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		routedAnswer := route
		if route.Outcome == "routed" {
			// Only the original bytes' success proves the routed model was
			// refused; a replay that fails too (an account rate limit) blames nothing.
			w.Header().Del("x-caveman-routed-from")
			route = RouteAnswer{Outcome: "degraded", Reason: "replay_failed"}
		}
		retryResp, doErr := s.doUpstream(r.Context(), func() (*http.Request, error) {
			retryReq, retryErr := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(originalBody))
			if retryErr != nil {
				return nil, retryErr
			}
			retryReq.Header = chatGPTRequestHeaders(r.Header)
			retryReq.Header.Del("Content-Length")
			return retryReq, nil
		})
		if doErr != nil {
			httpx.Error(w, r, http.StatusBadGateway, "cave_upstream_unreachable", "ChatGPT upstream is unreachable.")
			s.recordChatGPT(rc, r, requestID, traceID, suffix, start, 0, "cave_upstream_unreachable", reqCapture, reqHash.Sum(nil), reqHash.Sum(nil), true, nil, 0, false, nil, nil, compressEligible, originalBody, route)
			return
		}
		resp = retryResp
		if routedAnswer.Outcome == "routed" && resp.StatusCode < 400 {
			// transform.Body is still the refused request.
			refused := onlyModelChanged(transform.Body, originalBody, routedAnswer.Model)
			routedAnswer.reject(refused)
			route = RouteAnswer{Outcome: "degraded", Reason: "replayed_original"}
			if refused {
				route.Reason = "provider_rejected_routed_model"
			}
		}
		transform = providers.TransformResult{Body: originalBody, OptimizerIDs: []string{}}
		comp = nil
		// The original bytes served the request; the capture written before
		// the first attempt describes bytes the upstream rejected. Both
		// attempts stay on disk, and this one says which served.
		s.capture.record(captureMeta{
			RequestID:     requestID,
			Provider:      "chatgpt-subscription",
			Endpoint:      suffix,
			RuntimeMode:   rc.RuntimeMode,
			RetryOriginal: true,
		}, wholeBody(originalBody), wholeBody(originalBody))
	}
	defer resp.Body.Close()

	copySafeResponseHeaders(w.Header(), resp.Header)
	if comp != nil {
		w.Header().Set("x-cave-mode", rc.RuntimeMode)
		w.Header().Set("x-cave-optimization", strings.Join(transform.OptimizerIDs, ","))
		w.Header().Set("x-caveman-compression-ratio", strconv.FormatFloat(comp.ratio, 'f', 4, 64))
		w.Header().Set("x-caveman-recovery-handle", comp.handle)
		w.Header().Set("x-caveman-tokens-before", strconv.Itoa(comp.before))
		w.Header().Set("x-caveman-tokens-after", strconv.Itoa(comp.after))
		w.Header().Set("x-caveman-token-count-basis", "estimated_engine_o200k")
	}
	w.WriteHeader(resp.StatusCode)

	respCapture := &cappedBuffer{limit: chatGPTCaptureLimit}
	stream := streamingResponse(resp.Header)
	src := io.TeeReader(resp.Body, respCapture)
	var scanner *providers.UsageScanner
	if route.Outcome == "routed" {
		// The adapter's scanner keeps the tail, so an answer past the capture
		// limit that completed still reads as complete.
		scanner = adapter.NewUsageScanner(resp.Header)
		src = io.TeeReader(src, scanner)
	}
	counter, errCode := s.streamResponse(w, r, src, stream, requestID)
	respBytes := counter.n
	if scanner != nil {
		usage := scanner.Usage()
		if routedAttemptFailed(resp.StatusCode, usage.ProviderError, errCode == "cave_upstream_body_read_failed", usage) {
			route.reject(false) // the agent's own retry goes to the asked model
		}
	}

	// The streaming path forwarded the request without ever holding it whole, so it
	// captures only what it can state truthfully: the whole body's length and hash,
	// plus the bytes themselves when the bounded buffer happened to hold all of
	// them. This path never transforms, so both sides are the same body.
	if s.capture != nil && originalBody == nil {
		sent := streamedBody(reqCapture.total, hex.EncodeToString(reqHash.Sum(nil)))
		if !reqCapture.truncated {
			sent = wholeBody(append([]byte(nil), reqCapture.buf.Bytes()...))
		}
		s.capture.record(captureMeta{
			RequestID:   requestID,
			Provider:    "chatgpt-subscription",
			Endpoint:    suffix,
			RuntimeMode: rc.RuntimeMode,
		}, sent, sent)
	}

	requestHashComplete := chatGPTRequestHashComplete(requestBodyFullyRead, r.ContentLength, reqCapture, requestBodyTracker)
	s.recordChatGPT(rc, r, requestID, traceID, suffix, start, resp.StatusCode, errCode, reqCapture, reqHash.Sum(nil), transformedChatGPTHash(reqHash.Sum(nil), transform.Body), requestHashComplete, respCapture, respBytes, stream, transform.OptimizerIDs, comp, compressEligible, transform.Body, route)

	// Path, status, and timing only — request headers carry the operator's
	// OAuth credential and are never logged on this route.
	if s.logger != nil {
		s.logger.Info("chatgpt_proxy",
			"path", suffix, "status", resp.StatusCode,
			"latency_ms", time.Since(start).Milliseconds(), "stream", stream, "compressed", comp != nil, "error_code", errCode)
	}
	if errCode != "" {
		// Same contract as the provider proxy: the row is recorded, then framing
		// is aborted so a partial body is never a clean EOF.
		panic(http.ErrAbortHandler)
	}
}

func transformedChatGPTHash(raw []byte, transformed []byte) []byte {
	if transformed == nil {
		return raw
	}
	sum := sha256.Sum256(transformed)
	return sum[:]
}

func (s *Server) recordChatGPT(rc RequestContext, r *http.Request, requestID, traceID, endpoint string, start time.Time, status int, errCode string, reqCapture *cappedBuffer, reqHash, transformedHash []byte, requestHashComplete bool, respCapture *cappedBuffer, respBytes int64, stream bool, optimizers []string, comp *compressionOutcome, compressionEligible bool, acceptedBody []byte, route RouteAnswer) {
	if s.sink == nil {
		return
	}
	var usage providers.UsageObservation
	// Opportunistic and honest: a truncated capture is never parsed — partial
	// SSE could yield partial counters, and no number beats a wrong number.
	if respCapture != nil && !respCapture.truncated {
		providers.ParseUsageBytes("openai", respCapture.buf.Bytes(), &usage)
	}
	// Empty when unread (over the capture limit, or not JSON): Cloud never gets
	// "unknown" as a model id; the local row still says unknown.
	model := ""
	if reqCapture != nil && !reqCapture.truncated {
		var body struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(reqCapture.buf.Bytes(), &body) == nil && body.Model != "" {
			model = body.Model
		}
	}
	modelRequested := model
	if route.Outcome == "routed" && route.Model != "" {
		model = route.Model // the routed model served it; a retry records degraded instead
	}
	rawHash, transformedHashHex := "", ""
	if requestHashComplete {
		rawHash = hex.EncodeToString(reqHash)
		transformedHashHex = hex.EncodeToString(transformedHash)
	}
	var compRatio float64
	var compBefore, compAfter int
	var compHandle, compBasis string
	if comp != nil && status >= 200 && status < 300 && errCode == "" && !usage.ProviderError {
		compRatio, compBefore, compAfter, compHandle = comp.ratio, comp.before, comp.after, comp.handle
		if comp.before > comp.after {
			compBasis = "estimated_engine_o200k"
		}
	}
	row := RequestRecord{
		// storeTSLayout, not RFC3339Nano: the store's `ts` column is space-separated
		// and compared/ordered as text. 'T' sorts after ' ', so RFC3339 rows landed
		// on the wrong side of every `--since` bound and mis-sorted under ORDER BY ts.
		Timestamp: start.UTC().Format("2006-01-02 15:04:05.000"),
		// The route reached the compression candidate path; counted whether or not
		// bytes shrank, so the CLI can tell "routing never applied" from "nothing
		// to compress" (the generic route records the same denominator).
		CompressionEligible:      compressionEligible,
		RequestID:                requestID,
		TraceID:                  traceID,
		Label:                    labelOrDefault(rc.Label, "local"),
		AgentSlug:                rc.AgentSlug,
		Provider:                 "chatgpt-subscription",
		Model:                    labelOrDefault(model, "unknown"),
		RouteFrom:                modelRequested,
		RouteTo:                  model,
		RouteOutcome:             route.Outcome,
		RouteReason:              route.Reason,
		RouteDecisionID:          route.DecisionID,
		ProviderOriginKnown:      s.chatGPTUpstream == DefaultChatGPTUpstream && model != "",
		Endpoint:                 endpoint,
		Stream:                   stream,
		StatusCode:               status,
		ErrorCode:                errCode,
		LatencyMS:                time.Since(start).Milliseconds(),
		RequestBytes:             reqCapture.total,
		ResponseBytes:            respBytes,
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		CachedInputTokens:        usage.CachedInputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheCreation1hTokens:    usage.CacheCreation1hTokens,
		ReasoningTokens:          usage.ReasoningTokens,
		// Subscription traffic is unpriced: zero dollars, never an API-rate
		// guess (no-fake-savings). Token counts above are the honest meter.
		TotalCostUSD:               0,
		SavingsUSD:                 0,
		Basis:                      "inferred",
		TokenUsageBasis:            standaloneUsageBasis(usage),
		AuthMode:                   string(AuthModeSubscription),
		RuntimeMode:                rc.RuntimeMode,
		OptimizationIDs:            optimizers,
		RawRequestSHA256:           rawHash,
		TransformedRequestSHA256:   transformedHashHex,
		RequestHashComplete:        requestHashComplete,
		CompressionRatio:           compRatio,
		CompressionTokensBefore:    compBefore,
		CompressionTokensAfter:     compAfter,
		CompressionTokenCountBasis: compBasis,
		RecoveryHandle:             compHandle,
	}
	var originalBody []byte
	meta := providers.RequestMetadata{Provider: "chatgpt-subscription", Model: row.Model}
	if requestHashComplete && reqCapture != nil && !reqCapture.truncated {
		originalBody = reqCapture.buf.Bytes()
		if inspected, err := openai.New("").InspectRequest(r.Context(), bytes.NewReader(originalBody), r.Header); err == nil {
			meta = inspected
			meta.Provider = "chatgpt-subscription"
		}
	}
	if model != modelRequested {
		meta.Model = model
	}
	if s.chatGPTUpstream != DefaultChatGPTUpstream {
		meta.PricingUnsupportedReason = "custom_subscription_origin"
	}
	if acceptedBody == nil {
		acceptedBody = originalBody
	}
	requestAccounting(&row, meta, usage, originalBody, acceptedBody, false)
	s.sink.Record(row)
	if s.cloud != nil {
		s.cloud.Observe(row)
	}
}

type eofTrackingReader struct {
	reader io.Reader
	sawEOF bool
}

func (r *eofTrackingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF {
		r.sawEOF = true
	}
	return n, err
}

func chatGPTRequestHashComplete(fullyRead bool, contentLength int64, capture *cappedBuffer, tracker *eofTrackingReader) bool {
	if fullyRead {
		return true
	}
	if capture != nil && contentLength >= 0 && int64(capture.total) == contentLength {
		return true
	}
	return tracker != nil && tracker.sawEOF
}

// cappedBuffer retains up to limit bytes and records the true total; past the
// limit it flags truncation instead of growing (bounded memory, no partial
// parses downstream).
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	total     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.total += len(p)
	if c.truncated {
		return len(p), nil
	}
	room := c.limit - c.buf.Len()
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}
