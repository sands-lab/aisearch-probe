package probe

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// A page-code table lets the runner's reports check answers without the page-code
// secret. The released data ships one (page-codes.csv): every test page with the five
// codes it showed, one row per page, version and depth:
//
//	origin,page_id,version,depth,code
//	origin-a,<page id>,1,0,TRUNC-81a68f3bd9e4
//
// When a table is loaded and no secret is given, DepthCode looks the code up instead
// of computing it. A page not in the table has no code (""), so nothing can match it.

var codeTable map[string]string

func codeKey(id string, pct, revision int) string {
	return id + "|" + strconv.Itoa(pct) + "|" + strconv.Itoa(revision)
}

// LoadCodeTable reads a page-code table and returns how many codes it holds.
func LoadCodeTable(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return readCodeTable(f)
}

func readCodeTable(r io.Reader) (int, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, fmt.Errorf("empty page-code table")
	}
	col := map[string]int{}
	for i, name := range rows[0] {
		col[name] = i
	}
	for _, name := range []string{"page_id", "version", "depth", "code"} {
		if _, ok := col[name]; !ok {
			return 0, fmt.Errorf("page-code table has no %q column", name)
		}
	}
	t := map[string]string{}
	for n, row := range rows[1:] {
		v, err1 := strconv.Atoi(row[col["version"]])
		d, err2 := strconv.Atoi(row[col["depth"]])
		if err1 != nil || err2 != nil {
			return 0, fmt.Errorf("page-code table line %d: version and depth must be numbers", n+2)
		}
		code := strings.ToLower(strings.TrimPrefix(row[col["code"]], DepthMarkerPrefix))
		t[codeKey(row[col["page_id"]], d, v)] = code
	}
	codeTable = t
	return len(t), nil
}
