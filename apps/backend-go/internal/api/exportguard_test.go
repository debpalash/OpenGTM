package api

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestExportLimiterGlobalAndPerWorkspace(t *testing.T) {
	l := newExportLimiter(3, 2)
	r1, ok := l.tryAcquire("a")
	if !ok {
		t.Fatal("first")
	}
	r2, ok := l.tryAcquire("a")
	if !ok {
		t.Fatal("second")
	}
	if _, ok := l.tryAcquire("a"); ok {
		t.Fatal("per-workspace cap not enforced")
	}
	r3, ok := l.tryAcquire("b")
	if !ok {
		t.Fatal("other workspace must still fit")
	}
	if _, ok := l.tryAcquire("c"); ok {
		t.Fatal("global cap not enforced")
	}
	r1()
	r1() // idempotent
	if _, ok := l.tryAcquire("c"); !ok {
		t.Fatal("slot not returned")
	}
	r2()
	r3()
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

func TestDeadlineWriterExtendsDeadlinePerWrite(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := newDeadlineWriter(rec, 10*time.Second)
	for i := 0; i < 3; i++ {
		before := time.Now()
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		d := rec.deadlines[len(rec.deadlines)-1]
		if d.Before(before.Add(9*time.Second)) || d.After(time.Now().Add(11*time.Second)) {
			t.Fatalf("deadline %v not ~10s ahead", d)
		}
	}
	if len(rec.deadlines) != 3 || rec.Body.String() != "xxx" {
		t.Fatalf("deadlines %d body %q", len(rec.deadlines), rec.Body.String())
	}
}
