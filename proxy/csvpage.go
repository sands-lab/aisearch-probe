package main

import (
	"bytes"
	"encoding/csv"
	"net/http"

	"aisearch-probe/internal/probe"
)

// CSV pages: the CSV arm of the object-type study (tabular data). Same facts as
// /page/, served as text/csv: a header row plus one data row carrying the id and the
// marker in their own cells. Body is a pure function of the id (stable ETag);
// realistic header (Cache-Control omitted, NOT no-store).

const csvPagePrefix = "/csvpage/"

type csvPageRoute struct{}

func (csvPageRoute) prefix() string    { return csvPagePrefix }
func (csvPageRoute) needsSecret() bool { return true }

// renderCSVPage builds the CSV body for one id: a fixed header row and one data row,
// the per-page code in its own cell. encoding/csv writes deterministic bytes, so the
// result is a pure function of (id, code).
func renderCSVPage(id, code string) []byte {
	var buf bytes.Buffer
	wtr := csv.NewWriter(&buf)
	// Errors from a bytes.Buffer writer cannot occur; ignore them.
	_ = wtr.Write([]string{"status", "page_id", "reference_code"})
	_ = wtr.Write([]string{"live", id, code})
	wtr.Flush()
	return buf.Bytes()
}

func (csvPageRoute) build(s *staticServer, w http.ResponseWriter, r *http.Request) ([]byte, string, string, pageHeaders, bool) {
	id, ok := parseGenID(w, r, csvPagePrefix)
	if !ok {
		return nil, "", "", pageHeaders{}, false
	}
	code := probe.CpageCode(s.pageCodeSecret, id, probe.CpageFieldRef, probe.FormatPageRevision)
	return renderCSVPage(id, code), "page.csv", "text/csv; charset=utf-8", pageHeaders{}, true
}
