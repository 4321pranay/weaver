package edgemap

import (
	"path/filepath"
	"testing"
	"time"
)

func TestConfigQuestion(t *testing.T) {
	dir := t.TempDir()
	qp, op := filepath.Join(dir, "questions.yaml"), filepath.Join(dir, "edges.override.yaml")
	q := ConfigQuestion("shop", ".", "cache.type", "condition", []string{"local", "redis"}, []string{"a.LocalCache"})
	if q.ID != ConfigQuestion("shop", ".", "cache.type", "condition", nil, nil).ID {
		t.Fatal("id must depend on the subject only")
	}
	qf := &Questions{}
	qf.Record("run-1", 0, time.Now(), 1, []Question{q})
	if err := qf.Write(qp); err != nil {
		t.Fatal(err)
	}
	if _, err := Answer(qp, op, q.ID, "redis", "me", time.Now()); err != nil {
		t.Fatal(err)
	}
	qf, _ = LoadQuestions(qp)
	if a := qf.ConfigAnswers("shop"); a["cache.type"].Value != "redis" || len(qf.ConfigAnswers("other")) != 0 {
		t.Fatalf("answers: %+v", a)
	}
	if mod := ConfigQuestion("shop", "api", "cache.type", "condition", nil, nil); mod.ID == q.ID || mod.Subject["app"] != "api" {
		t.Fatal("a module's question is its own")
	}
}
