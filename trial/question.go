package main

import "fmt"

// standardQuestion is THE question every runner asks, inside the shared prompt
// wrapper (buildPrompt): it names one of the page's depth codes, so "the answer
// carries that code" proves the model read THAT page. Only the depth varies.
//
// THE STANDARD-QUESTION RULE: every runner uses this wording. A runner that asks
// anything else is an exception that must be listed, with its reason, in the
// registry in question_test.go.
// TestEveryQuestionIsRegistered (question_test.go) lists every question declared in
// the runners with its status and fails on one that is not listed, so a new wording
// cannot slip in unreviewed.
func standardQuestion(depth int) string {
	return fmt.Sprintf("Fetch this document and tell me its id and the code beginning with TRUNC- for the section at depth=%d%% in it. Quote them exactly.", depth)
}
