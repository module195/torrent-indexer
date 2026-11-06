package handler

import "net/http"

// maxPageDigits bounds the page number: no listing has a billion pages.
const maxPageDigits = 6

// pageParam returns the "page" query parameter when it is a plain page
// number, and "" (the first page) otherwise. Handlers splice the value into
// the upstream URL, so a raw value such as %7F made the request build fail,
// the handler answer 500 and the indexer count a health failure: three of
// those from any anonymous client marked a primary indexer unhealthy.
func pageParam(r *http.Request) string {
	page := r.URL.Query().Get("page")
	if page == "" || len(page) > maxPageDigits {
		return ""
	}
	for _, c := range page {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return page
}
