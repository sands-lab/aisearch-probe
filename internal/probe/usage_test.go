package probe

import "testing"

func TestUsageResponses(t *testing.T) {
	u, ok := UsageResponses([]byte(`{"output_text":"hi","usage":{"input_tokens":1200,"output_tokens":34}}`))
	if !ok || u.InputTokens != 1200 || u.OutputTokens != 34 {
		t.Errorf("got %+v ok=%v, want {1200,34} true", u, ok)
	}
	if _, ok := UsageResponses([]byte(`{"output_text":"hi"}`)); ok {
		t.Error("no usage object should report found=false")
	}
}

func TestUsageChat(t *testing.T) {
	u, ok := UsageChat([]byte(`{"choices":[],"usage":{"prompt_tokens":900,"completion_tokens":20}}`))
	if !ok || u.InputTokens != 900 || u.OutputTokens != 20 {
		t.Errorf("got %+v ok=%v, want {900,20} true", u, ok)
	}
	// A responses-shaped body has no prompt_tokens, so chat parser finds nothing.
	if u, ok := UsageChat([]byte(`{"usage":{"input_tokens":5,"output_tokens":6}}`)); ok || u.InputTokens != 0 {
		t.Errorf("chat parser must not read input_tokens: got %+v ok=%v", u, ok)
	}
}

func TestUsageGemini(t *testing.T) {
	u, ok := UsageGemini([]byte(`{"usageMetadata":{"promptTokenCount":700,"candidatesTokenCount":15}}`))
	if !ok || u.InputTokens != 700 || u.OutputTokens != 15 {
		t.Errorf("got %+v ok=%v, want {700,15} true", u, ok)
	}
	if _, ok := UsageGemini([]byte(`{"candidates":[]}`)); ok {
		t.Error("no usageMetadata should report found=false")
	}
}

func TestUsageAdd(t *testing.T) {
	got := Usage{InputTokens: 10, OutputTokens: 2}.Add(Usage{InputTokens: 5, OutputTokens: 3})
	if got != (Usage{InputTokens: 15, OutputTokens: 5}) {
		t.Errorf("Add = %+v, want {15,5}", got)
	}
	// Rounds sums too (both zero here from per-response parses; the driver sets it).
	if r := (Usage{Rounds: 2}).Add(Usage{Rounds: 1}).Rounds; r != 3 {
		t.Errorf("Add Rounds = %d, want 3", r)
	}
}
