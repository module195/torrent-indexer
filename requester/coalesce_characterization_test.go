package requester

// Caracterização da coalescência (REQUEST_COALESCE), auditoria de desempenho
// de 06/11/2026: fixa quantos downloads chegam ao site para pedidos
// simultâneos à mesma URL, com e sem a opção.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowSite conta os pedidos e segura cada um até release fechar.
func slowSite(t *testing.T, release <-chan struct{}) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-release
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, okPage)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func fetchConcurrently(t *testing.T, r *Requster, url string, n int, started *atomic.Int32) []string {
	t.Helper()
	bodies := make([]string, n)
	var wg sync.WaitGroup
	for k := 0; k < n; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started.Add(1)
			rc, err := r.GetDocument(context.Background(), url)
			if err != nil {
				t.Errorf("GetDocument: %v", err)
				return
			}
			defer rc.Close()
			b, _ := io.ReadAll(rc)
			bodies[k] = string(b)
		}()
	}
	wg.Wait()
	return bodies
}

func waitFor(cond func() bool) {
	deadline := time.Now().Add(2 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGetDocumentCoalescesConcurrentFetches(t *testing.T) {
	for _, tc := range []struct {
		coalesce bool
		wantHits int32
	}{{true, 1}, {false, 8}} {
		name := "off"
		if tc.coalesce {
			name = "on"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			srv, hits := slowSite(t, release)
			r := newTestRequester(t)
			r.coalesce = tc.coalesce

			var started atomic.Int32
			done := make(chan []string)
			go func() { done <- fetchConcurrently(t, r, srv.URL+"/busca?q=x", 8, &started) }()
			// todos entram antes do site responder
			waitFor(func() bool { return started.Load() == 8 && hits.Load() >= 1 })
			time.Sleep(50 * time.Millisecond)
			close(release)
			bodies := <-done

			if got := hits.Load(); got != tc.wantHits {
				t.Fatalf("site recebeu %d pedidos, want %d", got, tc.wantHits)
			}
			for k, b := range bodies {
				if b != okPage {
					t.Fatalf("corpo %d = %q", k, b)
				}
			}
		})
	}
}

// URLs diferentes nunca são coalescidas.
func TestGetDocumentCoalesceIsPerURL(t *testing.T) {
	release := make(chan struct{})
	close(release)
	srv, hits := slowSite(t, release)
	r := newTestRequester(t)
	r.coalesce = true
	for _, path := range []string{"/a", "/b", "/c"} {
		rc, err := r.GetDocument(context.Background(), srv.URL+path)
		if err != nil {
			t.Fatal(err)
		}
		rc.Close()
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("site recebeu %d pedidos, want 3", got)
	}
}

// Quem desiste não derruba o download compartilhado: o outro pedido recebe o corpo.
func TestGetDocumentCoalesceSurvivesCallerCancel(t *testing.T) {
	release := make(chan struct{})
	srv, hits := slowSite(t, release)
	r := newTestRequester(t)
	r.coalesce = true

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := r.GetDocument(ctx, srv.URL+"/x")
		errc <- err
	}()
	waitFor(func() bool { return hits.Load() == 1 })
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("pedido cancelado devolveu sem erro")
	}

	bodyc := make(chan string, 1)
	go func() {
		rc, err := r.GetDocument(context.Background(), srv.URL+"/x")
		if err != nil {
			bodyc <- "erro: " + err.Error()
			return
		}
		defer rc.Close()
		b, _ := io.ReadAll(rc)
		bodyc <- string(b)
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if b := <-bodyc; b != okPage {
		t.Fatalf("segundo pedido = %q", b)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("site recebeu %d pedidos, want 1", got)
	}
}
