package main

import (
	"bytes"
	"encoding/xml"
	"io"
	"testing"
)

// cpageServer is a static server with a page-code secret set, as the depth
// pages need. The Cache-Control given here must never reach a generated page.
func cpageServer(t *testing.T, secret string) *staticServer {
	t.Helper()
	st := newStaticServer(t.TempDir(), "public, max-age=3600")
	st.pageCodeSecret = secret
	return st
}

// wellFormedXML reports whether b parses cleanly as XML, by streaming every token
// to EOF.
func wellFormedXML(b []byte) error {
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
