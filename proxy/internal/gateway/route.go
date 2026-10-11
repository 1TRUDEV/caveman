package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/JuliusBrussee/caveman/proxy/providers"
	"github.com/JuliusBrussee/caveman/proxy/providers/jsonsplice"
)

// The route stage (ADR 0083 §4: parse, ask, compress, route). The ask starts
// before compression so the Cloud round trip overlaps it; the answer is applied
// to the bytes compression produced. Same provider only in this cut: an answer
// names another model of the provider the agent already talks to, swapped into
// the body's top-level "model". Every failure keeps the model the agent asked
// for, and the request is never held past the link's budget.

// RouteAsk is what the route stage knows about one request. Body is read-only
// and never leaves the machine: the link derives counts and a local cache key
// from it.
type RouteAsk struct {
	Provider   string
	Endpoint   string
	Model      string
	Agent      string
	SessionID  string
	ToolsCount int
	InputBytes int
	Body       []byte
}

// RouteAnswer is the decision for one request. Model is set only when the
// request moves to it. Outcome is the runtime/v1 route outcome: routed, kept,
// degraded, paused or off.
type RouteAnswer struct {
	Model      string
	Outcome    string
	Reason     string
	DecisionID string
	// Reject, when set, moves the rest of this ask back to the asked model.
	// refused says the provider refused the routed model (the original bytes
	// then succeeded), which the link also remembers for the session and model
	// pair; false is any other failure on the routed model (5xx, transport
	// error, a broken stream).
	Reject func(refused bool)
}

func (a RouteAnswer) reject(refused bool) {
	if a.Reject != nil {
		a.Reject(refused)
	}
}

// routedAttemptFailed: the routed model answered 5xx, reported a provider
// error event, or its stream broke upstream before the provider reported usage.
// A 4xx is the replay's to judge. broken is an upstream read failure only: a
// client cancel (the user pressing Esc, Codex closing on response.completed) is
// the user stopping, never the model failing.
func routedAttemptFailed(status int, providerErr, broken bool, usage providers.UsageObservation) bool {
	return status >= 500 || status < 400 && (providerErr || broken && !usage.Complete())
}

// onlyModelChanged: sent is original with nothing but the model swapped, so a
// refusal the original bytes then got past is the routed model's. With
// compression (or any other transform) in the mix the refusal proves nothing.
func onlyModelChanged(sent, original []byte, model string) bool {
	swapped, ok := setModel(original, model)
	return ok && bytes.Equal(sent, swapped)
}

// CloudLink is the optional signed-in Cloud connection: the route stage asks
// it for a model, and every recorded request is offered to its runtime/v1
// sender. Both fail open and never block a request. Nil means a local-only
// proxy, exactly as before.
type CloudLink interface {
	// Ask starts the decision and returns the wait for it.
	Ask(ctx context.Context, ask RouteAsk) func() RouteAnswer
	Observe(rec RequestRecord)
}

// routable: Anthropic Messages or OpenAI chat/responses. API-key, subscription
// and OAuth traffic to the provider's own API is asked about alike; a
// subscription row still records tokens only, never dollars.
func routable(provider, endpoint string) bool {
	switch provider {
	case "anthropic":
		return strings.HasSuffix(endpoint, "/messages")
	case "openai":
		return strings.HasSuffix(endpoint, "/chat/completions") || strings.HasSuffix(endpoint, "/responses")
	}
	return false
}

// LatestHumanText is the newest human-written text in a provider request (tool
// results skipped). The route stage hashes it into a local cache key so every
// tool-loop turn of one ask reuses one decision; the text itself goes nowhere.
func LatestHumanText(provider, endpoint string, body []byte) string {
	return extractCompressionQuery(provider, endpoint, body)
}

// setModel replaces the value of the top-level "model" string and leaves every
// other byte as it was. ok is false when there is no such string field.
func setModel(body []byte, model string) ([]byte, bool) {
	root, ok := jsonsplice.Root(body)
	if !ok {
		return body, false
	}
	span, ok := jsonsplice.Field(body, root, "model")
	if _, isString := jsonsplice.String(body, span); !ok || !isString {
		return body, false
	}
	encoded, _ := json.Marshal(model)
	out, err := jsonsplice.ReplaceRaw(body, span, encoded)
	if err != nil {
		return body, false
	}
	return out, true
}
