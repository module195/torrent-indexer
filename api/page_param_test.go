package handler

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func Test_pageParam(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"page=2":          "2",
		"page=123456":     "123456",
		"page=1234567":    "",
		"page=%7F":        "",
		"page=%01":        "",
		"page=2%2F..%2Fx": "",
		"page=-1":         "",
		"page=abc":        "",
	}
	for query, want := range cases {
		r := httptest.NewRequest("GET", "/indexers/bludv_ru?"+query, nil)
		if got := pageParam(r); got != want {
			t.Errorf("%q: pageParam = %q, want %q", query, got, want)
		}
	}
}

func Test_multiIndexerNamesCapsAndDedups(t *testing.T) {
	var names []string
	for i := 0; i < 10000; i++ {
		names = append(names, fmt.Sprintf("nope%d", i))
	}
	got := multiIndexerNames("bludv_ru, BLUDV_RU,betor," + strings.Join(names, ","))
	if len(got) != maxMultiIndexers {
		t.Fatalf("len = %d, want %d", len(got), maxMultiIndexers)
	}
	if got[0] != "bludv_ru" || got[1] != "betor" {
		t.Fatalf("order/dedup broken: %v", got[:3])
	}
}
