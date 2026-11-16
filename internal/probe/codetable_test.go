package probe

import (
	"strings"
	"testing"
)

// A table built from a secret must give the same codes as the secret itself, and a
// page missing from the table must have no code.
func TestCodeTableMatchesTheSecret(t *testing.T) {
	const secret = "test-secret"
	var b strings.Builder
	b.WriteString("origin,page_id,version,depth,code\n")
	for _, v := range []int{1, 2} {
		for _, d := range DepthPercents {
			b.WriteString("origin-a,pg-1," + itoaDepth(v) + "," + itoaDepth(d) + "," + DepthMarkerPrefix + DepthCode(secret, "pg-1", d, v) + "\n")
		}
	}
	defer func() { codeTable = nil }()
	n, err := readCodeTable(strings.NewReader(b.String()))
	if err != nil || n != 10 {
		t.Fatalf("read %d codes, err %v; want 10", n, err)
	}
	for _, v := range []int{1, 2} {
		for _, d := range DepthPercents {
			if got, want := DepthCode("", "pg-1", d, v), CpageCode(secret, "pg-1", DepthField(d), v); got != want {
				t.Errorf("depth %d version %d: table %q, secret %q", d, v, got, want)
			}
		}
	}
	answer := "the code is " + strings.ToUpper(DepthCode("", "pg-1", 75, 1))
	if got := DeepestDepth(answer, "", "pg-1", 1); got != 75 {
		t.Errorf("DeepestDepth from the table = %d, want 75", got)
	}
	if DepthCode("", "not-in-table", 0, 1) != "" {
		t.Error("a page missing from the table must have no code")
	}
	// With a secret given, the secret wins over the table.
	if DepthCode("other", "pg-1", 0, 1) == DepthCode("", "pg-1", 0, 1) {
		t.Error("a given secret must be used, not the table")
	}
}
