package handler

// Harness de latência do MISS (auditoria M3, etapa 2a). Só mede: roda o
// handler do rede_torrent contra um site falso com latência fixa por
// requisição, sem tocar em nenhum site, tracker ou API de terceiros.
//
//	M3_HARNESS=1 go test ./api -run TestM3Harness -v -count=1
//
// Knobs: M3_DELAY_MS (latência do site, padrão 300), M3_PEERS_MS (scrape de
// peers por release, padrão 575 = janela do goscrape), M3_N (repetições, 5).

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/module195/torrent-indexer/cache"
	"github.com/module195/torrent-indexer/monitoring"
)

func m3Env(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v >= 0 {
		return v
	}
	return def
}

type m3Site struct {
	srv       *httptest.Server
	requests  atomic.Int32
	inflight  atomic.Int32
	maxFlight atomic.Int32
}

func newM3Site(t *testing.T, delay time.Duration) *m3Site {
	t.Helper()
	read := func(name string) string {
		b, err := os.ReadFile("testdata/rede_torrent/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	listing := read("busca-homem.html")
	// só páginas com link do IMDb: sem elas o handler consulta o OMDb
	titles := []string{read("moana-2026.html"), read("os-bad-boys-colecao.html")}
	s := &m3Site{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		n := s.inflight.Add(1)
		defer s.inflight.Add(-1)
		for {
			m := s.maxFlight.Load()
			if n <= m || s.maxFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(delay)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/index.php" {
			if r.URL.Query().Get("busca") == "homem" {
				_, _ = w.Write([]byte(strings.ReplaceAll(listing, "https://redestorrents.com", s.srv.URL)))
			} else {
				_, _ = w.Write([]byte("<html><body><p>Nada encontrado</p></body></html>"))
			}
			return
		}
		_, _ = w.Write([]byte(titles[len(r.URL.Path)%len(titles)]))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// startM3Redis sobe um Redis falso que sempre erra o cache (GET → nil,
// escrita → OK). Sem Redis nenhum, o go-redis gasta ≈200 ms por operação
// em retentativas e isso mascara a medida. A porta 6379 é fixa no
// cache.NewRedis.
func startM3Redis(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:6379")
	if err != nil {
		t.Skipf("porta 6379 ocupada (%v); o harness precisa dela para o Redis falso", err)
	}
	t.Setenv("REDIS_HOST", "127.0.0.1")
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				rd := bufio.NewReader(c)
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					argc, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
					var cmd string
					for a := 0; a < argc; a++ {
						hdr, err := rd.ReadString('\n')
						if err != nil {
							return
						}
						size, _ := strconv.Atoi(strings.TrimSpace(hdr[1:]))
						buf := make([]byte, size+2)
						if _, err := io.ReadFull(rd, buf); err != nil {
							return
						}
						if a == 0 {
							cmd = strings.ToUpper(string(buf[:size]))
						}
					}
					var reply string
					switch cmd {
					case "GET":
						reply = "$-1\r\n"
					case "SET", "SETEX":
						reply = "+OK\r\n"
					case "DEL", "EXISTS", "EXPIRE":
						reply = ":0\r\n"
					case "PING":
						reply = "+PONG\r\n"
					default: // HELLO, CLIENT SETINFO…: o go-redis cai para RESP2
						reply = "-ERR unknown command\r\n"
					}
					if _, err := c.Write([]byte(reply)); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
}

func TestM3Harness(t *testing.T) {
	if os.Getenv("M3_HARNESS") != "1" {
		t.Skip("harness de medição; rode com M3_HARNESS=1")
	}
	startM3Redis(t)
	delay := time.Duration(m3Env("M3_DELAY_MS", 300)) * time.Millisecond
	peersDelay := time.Duration(m3Env("M3_PEERS_MS", 575)) * time.Millisecond
	n := m3Env("M3_N", 5)

	oldPeers := redeTorrentPeers
	redeTorrentPeers = func(ctx context.Context, _ *cache.Redis, _ *monitoring.Metrics, _ string, _ []string) (int, int, error) {
		select {
		case <-time.After(peersDelay):
			return 1, 1, nil
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		}
	}
	t.Cleanup(func() { redeTorrentPeers = oldPeers })

	cases := []struct{ name, q string }{
		{"listagem 20 títulos", "homem"},
		{"listagem vazia + fallbacks", "zzqx wwkv yyjp s01e01"},
	}
	t.Logf("site D=%v, peers P=%v, concorrência de páginas=%d, fallbacks de %q=%d",
		delay, peersDelay, redeTorrentPageConcurrency, cases[1].q, len(buildRedeTorrentFallbackQueries(cases[1].q)))
	for _, c := range cases {
		var times []time.Duration
		var reqs, maxFlight int32
		var count string
		for k := 0; k < n; k++ {
			site := newM3Site(t, delay)
			old := rede_torrent.URL
			rede_torrent.URL = site.srv.URL + "/"
			i := newTfhdsTestIndexer(t, site.srv)
			i.postProcessors = nil

			w := httptest.NewRecorder()
			start := time.Now()
			i.HandlerRedeTorrentIndexer(w, httptest.NewRequest(http.MethodGet, "/indexers/rede_torrent?q="+strings.ReplaceAll(c.q, " ", "+"), nil))
			times = append(times, time.Since(start))
			rede_torrent.URL = old
			if w.Code != http.StatusOK {
				t.Fatalf("%s: status %d: %s", c.name, w.Code, w.Body.String())
			}
			reqs, maxFlight = site.requests.Load(), site.maxFlight.Load()
			body := w.Body.String()
			if idx := strings.LastIndex(body, `"count":`); idx >= 0 {
				count = strings.SplitN(body[idx+8:], ",", 2)[0]
			}
		}
		sorted := append([]time.Duration(nil), times...)
		sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })
		ms := func(d time.Duration) string { return fmt.Sprintf("%.0f", float64(d)/float64(time.Millisecond)) }
		t.Logf("%-28s n=%d mediana=%sms faixa=%s–%sms req_ao_site=%d pico_concorrente=%d count=%s",
			c.name, n, ms(sorted[len(sorted)/2]), ms(sorted[0]), ms(sorted[len(sorted)-1]), reqs, maxFlight, count)
	}
}
