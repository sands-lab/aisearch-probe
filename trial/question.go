package main

import "fmt"

// standardQuestion is THE question every runner asks, inside the shared prompt
// wrapper (buildPrompt): it names one of the page's depth codes, so "the answer
// carries that code" proves the model read THAT page. Only the depth varies.
//
// THE RULE (DECISIONS.md § D14, the user, 2026-10-25): every runner uses this
// wording. A runner that needs to ask anything else must get the user's approval
// first, with its reason, recorded in DECISIONS.md — a code comment is not enough.
// TestEveryQuestionIsRegistered (question_test.go) lists every question declared in
// the runners with its status and fails on one that is not listed, so a new wording
// cannot slip in unreviewed.
func standardQuestion(depth int) string {
	return fmt.Sprintf("Fetch this document and tell me its id and the code beginning with TRUNC- for the section at depth=%d%% in it. Quote them exactly.", depth)
}
