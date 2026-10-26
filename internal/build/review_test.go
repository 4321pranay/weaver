package build

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"weaver/internal/edgemap"
	"weaver/internal/weaveai"
)

type lineTools struct{}

func (lineTools) Call(string, map[string]any) (any, error) {
	return map[string]any{"result": []any{map[string]any{"n": 5, "text": "call"}}}, nil
}

// weave-ai reviews the map before it is written: an agreed edge is marked, a
// disputed one stays with call: unknown and a review question, a kept one is
// the human's.
func TestReviewMap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Messages []map[string]any }
		json.NewDecoder(r.Body).Decode(&req)
		user := req.Messages[1]["content"].(string)
		verdict := map[string]any{"verdict": "agree", "reason": "it calls it", "evidence": []string{"A.java:5"}}
		if strings.Contains(user, "/refund") {
			verdict = map[string]any{"verdict": "disagree", "reason": "the client is never called", "evidence": []string{"B.java:5"}}
		}
		args, _ := json.Marshal(verdict)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
			"tool_calls": []any{map[string]any{"id": "1", "function": map[string]any{"name": "verdict", "arguments": string(args)}}}}}}})
	}))
	defer srv.Close()
	m := edgemap.Map{Edges: []edgemap.Edge{
		{From: "web", To: "pay", Kind: "rest", Resource: "/charge", Call: "required", Callers: []edgemap.Caller{{At: "A.java:5"}}},
		{From: "web", To: "pay", Kind: "rest", Resource: "/refund", Call: "required", Callers: []edgemap.Caller{{At: "B.java:5"}}},
		{From: "web", To: "pay", Kind: "rest", Resource: "/void", Call: "optional", Callers: []edgemap.Caller{{At: "C.java:9"}}},
	}}
	qf := &edgemap.Questions{}
	keep := edgemap.ReviewQuestion(m.Edges[2], "earlier dispute", nil)
	qf.Record("run-1", 1, time.Now(), 1, []edgemap.Question{keep})
	qf.Questions[0].Answer, qf.Questions[0].Status = &edgemap.Answered{Value: "keep"}, "answered"
	r := &weaveai.Reviewer{C: weaveai.Config{URL: srv.URL, Model: "m", MaxReviews: 10, MaxSteps: 3}, Tools: lineTools{},
		Cache: filepath.Join(t.TempDir(), "v.yaml")}
	var log bytes.Buffer
	qs := reviewMap(&m, r, "shop", qf, &log)
	if m.Edges[0].ReviewedBy != "llm" {
		t.Errorf("agreed: %+v", m.Edges[0])
	}
	if e := m.Edges[1]; e.Call != "unknown" || e.Question == "" || len(qs) != 1 || qs[0].Type != "review" || qs[0].ID != e.Question {
		t.Errorf("disputed edge stays, waiting on a review question: %+v %+v", e, qs)
	}
	if m.Edges[2].ReviewedBy != "human" || m.Edges[2].Call != "optional" {
		t.Errorf("kept by a person: %+v", m.Edges[2])
	}
}
