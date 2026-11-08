package main

import (
	"encoding/csv"
	"net/http"
	"strings"
	"testing"
)

// /csvpage/ is the CSV arm (tabular data). Same facts, served as text/csv: a header
// row plus one data row carrying the per-page code and id. Stamped, 304, realistic
// header, deterministic bytes, and parseable as CSV.
func TestCSVPageIsStampedParseableAndCarriesMarker(t *testing.T) {
	srv, read, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")

	const id = "r0915-csv-a"
	resp := get(t, srv.URL+"/csvpage/"+id, nil)
	got := body(t, resp)

	if resp.Header.Get("ETag") == "" {
		t.Error("CSV page has no ETag — a crawler could not revalidate it")
	}
	if _, present := resp.Header["Cache-Control"]; present {
		t.Errorf("CSV page sent Cache-Control %q, want it omitted (realistic header)", resp.Header.Get("Cache-Control"))
	}
	// Parse as CSV: a header row and at least one data row, and the per-page code + id
	// must appear in a cell (not just somewhere in the bytes).
	rows, err := csv.NewReader(strings.NewReader(got)).ReadAll()
	if err != nil {
		t.Fatalf("CSV page body does not parse as CSV: %v\n%s", err, got)
	}
	if len(rows) < 2 {
		t.Fatalf("CSV page needs a header row plus a data row, got %d rows:\n%s", len(rows), got)
	}
	code := fmtRefCode(id)
	var sawCode, sawID bool
	for _, row := range rows[1:] {
		for _, cell := range row {
			if cell == code {
				sawCode = true
			}
			if cell == id {
				sawID = true
			}
		}
	}
	if !sawCode {
		t.Errorf("CSV page has no cell holding its per-page code %q:\n%s", code, got)
	}
	if !sawID {
		t.Errorf("CSV page has no cell holding its id:\n%s", got)
	}
	recs := read()
	if len(recs) != 1 || !recs[0].Stamped() {
		t.Fatalf("the log must show the CSV page as stamped: %+v", recs)
	}
}

func TestCSVPageAnswers304(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")
	first := get(t, srv.URL+"/csvpage/r0915-csv-b", nil)
	first.Body.Close()
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on first fetch")
	}
	second := get(t, srv.URL+"/csvpage/r0915-csv-b", map[string]string{"If-None-Match": etag})
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation returned %d, want 304", second.StatusCode)
	}
}

func TestCSVPageBareIs404(t *testing.T) {
	srv, _, _ := newTestProxy(t, cpageServer(t, fmtTestSecret), "")
	resp := get(t, srv.URL+"/csvpage/", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/csvpage/ with no id returned %d, want 404", resp.StatusCode)
	}
}

func TestCSVPageDeterministic(t *testing.T) {
	if string(renderCSVPage("same-id", "code123")) != string(renderCSVPage("same-id", "code123")) {
		t.Error("renderCSVPage is not deterministic")
	}
}
