package weaveai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

type stubTools struct{}

func (stubTools) Call(name string, a map[string]any) (any, error) {
	return map[string]any{"build": 1, "result": []any{map[string]any{"n": 12, "text": "client.GetRates(ctx, req)"}}}, nil
}

// fake: an OpenAI-compatible endpoint that reads once, then answers verdict.
func fake(t *testing.T, verdict map[string]any, requests *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests++
		var req struct{ Messages []map[string]any }
		_ = json.NewDecoder(r.Body).Decode(&req)
		call := map[string]any{"id": "1", "type": "function", "function": map[string]any{"name": "read",
			"arguments": `{"service":"search","path":"search.go","from":10,"to":14}`}}
		if len(req.Messages) > 2 { // the read came back: conclude
			args, _ := json.Marshal(verdict)
			call = map[string]any{"id": "2", "type": "function", "function": map[string]any{"name": "verdict", "arguments": string(args)}}
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{call}}}}})
	}))
}

func TestReview(t *testing.T) {
	claim := Claim{Ref: "hotel-search → hotel-rate grpc rate.Rate/GetRates", Sites: []string{"hotel-search:search.go:12"}, Domain: "d"}
	for name, c := range map[string]struct {
		verdict map[string]any
		agree   bool
	}{
		"agree, citing the call site":     {map[string]any{"verdict": "agree", "reason": "calls GetRates", "evidence": []string{"search.go:12"}}, true},
		"agree, citing an unread line":    {map[string]any{"verdict": "agree", "reason": "x", "evidence": []string{"search.go:12", "other.go:3"}}, false},
		"agree, not citing the call site": {map[string]any{"verdict": "agree", "reason": "x", "evidence": []string{"search.go:13"}}, false},
	} {
		n := 0
		srv := fake(t, c.verdict, &n)
		r := &Reviewer{C: Config{URL: srv.URL, Model: "m", MaxSteps: 5}, Tools: stubTools{}, Cache: filepath.Join(t.TempDir(), "cache.yaml")}
		v, err := r.Review(claim)
		srv.Close()
		if err != nil || v.Agree != c.agree {
			t.Errorf("%s: agree=%v, want %v (%+v %v)", name, v.Agree, c.agree, v, err)
		}
	}
	// the same edge, sites, model and prompt: the cached verdict, no LLM call
	n := 0
	srv := fake(t, map[string]any{"verdict": "agree", "reason": "r", "evidence": []string{"search.go:12"}}, &n)
	defer srv.Close()
	cache := filepath.Join(t.TempDir(), "cache.yaml")
	r := &Reviewer{C: Config{URL: srv.URL, Model: "m", MaxSteps: 5}, Tools: stubTools{}, Cache: cache}
	r.Review(claim)
	before := n
	r2 := &Reviewer{C: Config{URL: srv.URL, Model: "m", MaxSteps: 5}, Tools: stubTools{}, Cache: cache}
	if v, _ := r2.Review(claim); !v.Agree || n != before {
		t.Errorf("cached verdict, no new request: %+v, %d requests", v, n-before)
	}
}
