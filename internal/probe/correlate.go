package probe

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Liveness records what the driver's own fetch of the page saw. Written by
// probe-trial; see trial/liveness.go for why it exists.
//
// CheckedAtAll distinguishes "we looked and the page was down" from "this trial
// log predates the check". Trials written before the field existed decode to the
// zero value, and reading that as a dead page would silently discard every
// earlier result.
type Liveness struct {
	OK      bool   `json:"ok"`
	Status  int    `json:"status,omitempty"`
	Marker  bool   `json:"marker,omitempty"`
	Err     string `json:"err,omitempty"`
	CheckAt string `json:"checked_at,omitempty"`
}

func (l Liveness) CheckedAtAll() bool { return l.CheckAt != "" }

// Trial is one query submitted to an assistant by the probe-trial driver.
type Trial struct {
	TrialID     string  `json:"trial_id"`
	Offset      string  `json:"offset"`
	Arm         string  `json:"arm"`
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	URL         string  `json:"url"`
	QuestionIdx int     `json:"question_idx"`
	Question    string  `json:"question"`
	SentAt      string  `json:"sent_at"`
	SentUnix    float64 `json:"sent_unix"`
	ElapsedS    float64 `json:"elapsed_s"`
	// Token, when present, was placed in the URL given to THIS provider and to
	// no other. It is the only way to attribute a fetch from a provider whose
	// fetcher forges its user-agent.
	Token    string `json:"token"`
	URLAsked string `json:"url_asked"`
	// PageLive is what the driver's own fetch of the page saw at the start of
	// the round. When the page was not reachable, "nothing arrived at the
	// server" carries no information: a provider that tried and failed leaves
	// exactly the trace of one that never tried. Such a round is reported and
	// not scored.
	PageLive    Liveness `json:"page_live"`
	FoundMarker bool     `json:"found_marker"`
	Answer      string   `json:"answer"`
	Error       string   `json:"error,omitempty"`
}

// Correlation pairs a submitted query with whatever arrived at the server
// afterwards. The window is deliberately generous at the front and tight at the
// back: a fetch caused by a query lands within seconds of submission, so a hit
// well outside that is background crawling that happened to coincide.
type Correlation struct {
	Trial   Trial
	Hits    []Record
	Fetched bool // did anything arrive that this query plausibly caused?

	// OtherHits: requests for the same page in the same window that did NOT
	// carry this trial's token.
	//
	// They are kept apart rather than discarded, and rather than counted as
	// this provider's fetch, because they have two very different causes and we
	// cannot always tell which: unrelated background traffic (this page is on
	// the public internet and gets scanned), or this provider's own fetcher
	// having normalised the query away.
	//
	// The second case is a hole in OUR measurement. Reporting it as "nothing
	// arrived" would let it masquerade as the study's headline finding, so the
	// report names it as unattributable instead. Never silently drop these.
	OtherHits []Record
}

// LoadTrials reads a trial log written by probe-trial.
func LoadTrials(path string) ([]Trial, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseTrials(f)
}

func ParseTrials(r io.Reader) ([]Trial, error) {
	var out []Trial
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var t Trial
		if err := json.Unmarshal([]byte(line), &t); err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, sc.Err()
}

// Correlate matches each trial against requests that arrived within
// windowSeconds after it was submitted, for the same page.
func Correlate(trials []Trial, records []Record, windowSeconds float64) []Correlation {
	sorted := make([]Record, len(records))
	copy(sorted, records)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].TSUnix < sorted[j].TSUnix })

	out := make([]Correlation, 0, len(trials))
	for _, t := range trials {
		c := Correlation{Trial: t}
		wantPage := pagePathOf(t.URL)
		for _, r := range sorted {
			if r.TSUnix < t.SentUnix {
				continue
			}
			if r.TSUnix > t.SentUnix+windowSeconds {
				break
			}
			if wantPage != "" && r.PageKey != wantPage {
				continue
			}
			// With a token, "this provider fetched" means a request carrying
			// that token. Everything else for this page goes to OtherHits.
			if t.Token != "" && !strings.Contains(r.URL, t.Token) {
				c.OtherHits = append(c.OtherHits, r)
				continue
			}
			c.Hits = append(c.Hits, r)
		}
		c.Fetched = len(c.Hits) > 0
		out = append(out, c)
	}
	return out
}

func pagePathOf(rawURL string) string {
	i := strings.Index(rawURL, "://")
	if i < 0 {
		return rawURL
	}
	rest := rawURL[i+3:]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		return "/"
	}
	path := rest[j:]
	if k := strings.IndexByte(path, '?'); k >= 0 {
		path = path[:k]
	}
	return path
}

// WriteCorrelationReport prints, per submitted query, whether the server
// actually saw a fetch — and flags the case that matters most: the assistant
// answered correctly without fetching anything, which means it served your
// content from a cache of its own.
func WriteCorrelationReport(w io.Writer, cors []Correlation) {
	fmt.Fprintln(w, sep)
	fmt.Fprintln(w, "  DID THE QUERY REACH THE SERVER?")
	fmt.Fprintln(w, sep)
	fmt.Fprintln(w)

	var answeredWithoutFetch, fetched, failed, needsReview, notScored int

	for _, c := range cors {
		t := c.Trial
		arm := t.Arm
		if arm == "" {
			arm = "-"
		}
		fmt.Fprintf(w, "  T+%-5s %-11s arm=%-7s q%d  %s\n", t.Offset, t.Provider, arm, t.QuestionIdx, pagePathOf(t.URL))
		fmt.Fprintf(w, "    submitted        : %s\n", t.SentAt)

		switch {
		case t.Error != "":
			fmt.Fprintf(w, "    result           : request failed (%s)\n", truncateStr(t.Error, 90))
			failed++
		case c.Fetched:
			fmt.Fprintf(w, "    server saw       : %d request(s)\n", len(c.Hits))
			for _, h := range c.Hits {
				gap := h.TSUnix - t.SentUnix
				what := "re-downloaded the whole page"
				if h.AskedIfChanged() {
					what = "asked \"has this changed?\" first"
				}
				fmt.Fprintf(w, "                       +%5.1fs  %-18s %d  %s\n", gap, h.Bot, h.StatusCode, what)
			}
			fetched++
		case t.PageLive.CheckedAtAll() && !t.PageLive.OK:
			fmt.Fprintf(w, "    result           : NOT SCORED — our own fetch of the page failed\n")
			fmt.Fprintf(w, "                       at the start of this round (%s)\n", truncateStr(t.PageLive.Err, 80))
			fmt.Fprintf(w, "    A provider that tried to fetch and could not leaves the same trace\n")
			fmt.Fprintf(w, "    as one that never tried, so this round cannot support any claim\n")
			fmt.Fprintf(w, "    about caching in either direction.\n")
			notScored++
		case len(c.OtherHits) > 0:
			// Requests for the page arrived, none of them ours to claim. This is
			// deliberately NOT counted as caching, in either direction.
			fmt.Fprintf(w, "    server saw       : %d request(s) for this page, NONE carrying this\n", len(c.OtherHits))
			fmt.Fprintf(w, "                       provider's token (%s) — UNATTRIBUTABLE\n", t.Token)
			for _, h := range c.OtherHits {
				gap := h.TSUnix - t.SentUnix
				fmt.Fprintf(w, "                       +%5.1fs  %-18s %d  %s\n", gap, h.Bot, h.StatusCode, truncateStr(h.UserAgent, 50))
			}
			fmt.Fprintf(w, "    Either background traffic, or this provider dropped the query when\n")
			fmt.Fprintf(w, "    fetching. Not scored as a cache hit and not scored as a fetch:\n")
			fmt.Fprintf(w, "    if the token never survives for this provider, that is our gap to fix.\n")
			needsReview++
		default:
			fmt.Fprintf(w, "    server saw       : NOTHING\n")
			if t.FoundMarker {
				fmt.Fprintf(w, "    the answer carried the page marker — it served your content\n")
				fmt.Fprintf(w, "    from its own cache without contacting the server.\n")
				answeredWithoutFetch++
			} else {
				// The marker only appears when the question happens to elicit it.
				// A correct answer to a question never asked before is content
				// reuse just the same, so show it rather than concluding.
				fmt.Fprintf(w, "    no page marker in the answer — INCONCLUSIVE, READ THE ANSWER.\n")
				fmt.Fprintf(w, "    A correct answer to a question it was never asked before is\n")
				fmt.Fprintf(w, "    content reuse just the same, and the marker cannot see that:\n")
				ans := strings.ReplaceAll(strings.TrimSpace(t.Answer), "\n", " ")
				if ans == "" {
					ans = "(no answer recorded)"
				}
				fmt.Fprintf(w, "      Q: %s\n", truncateStr(t.Question, 100))
				fmt.Fprintf(w, "      A: %s\n", truncateStr(ans, 220))
				needsReview++
			}
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, sep)
	fmt.Fprintln(w, "  SUMMARY")
	fmt.Fprintln(w, sep)
	fmt.Fprintf(w, "  Queries that caused a fetch            : %d\n", fetched)
	fmt.Fprintf(w, "  Answered from the page WITHOUT fetching: %d\n", answeredWithoutFetch)
	fmt.Fprintf(w, "  Requests that failed                   : %d\n", failed)
	if notScored > 0 {
		fmt.Fprintf(w, "  NOT SCORED (our page was unreachable)  : %d\n", notScored)
		fmt.Fprintf(w, "    Those rounds are excluded from every count above. A dead page\n")
		fmt.Fprintf(w, "    makes a provider that tried to fetch look identical to one that\n")
		fmt.Fprintf(w, "    never asked, which is the finding's own shape.\n")
	}
	if needsReview > 0 {
		fmt.Fprintf(w, "  No fetch, no marker — READ THE ANSWERS  : %d\n", needsReview)
		fmt.Fprintf(w, "    A correct answer there is content reuse the marker missed.\n")
	}
	if answeredWithoutFetch > 0 {
		fmt.Fprintf(w, "\n  The second line is the finding: content was reused without the\n")
		fmt.Fprintf(w, "  server being contacted at all.\n")
	}
	fmt.Fprintln(w)
}

func truncateStr(s string, n int) string { return truncate(s, n) }
