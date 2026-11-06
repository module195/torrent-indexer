package utils_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/module195/torrent-indexer/utils"
)

// A panic in one item must become that item's error, not end the process,
// and the other items' results must still come back.
func TestParallelFlatMapRecoversPanic(t *testing.T) {
	var errs []error
	got := utils.ParallelFlatMap([]int{1, 2, 3}, func(i int) ([]int, error) {
		if i == 2 {
			panic("boom")
		}
		return []int{i * 10}, nil
	}, func(err error) { errs = append(errs, err) })

	if len(got) != 2 {
		t.Fatalf("results = %v, want the two items that did not panic", got)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "boom") {
		t.Fatalf("errors = %v, want one error carrying the panic", errs)
	}
}

func TestRecoverAsErrorWithoutPanicKeepsError(t *testing.T) {
	want := errors.New("normal")
	f := func() (err error) {
		defer utils.RecoverAsError(&err, "test")
		return want
	}
	if err := f(); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestRecoverAsErrorDeferredDirectly(t *testing.T) {
	f := func() (err error) {
		defer utils.RecoverAsError(&err, "test")
		panic("boom")
	}
	if err := f(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the panic as an error", err)
	}
}
