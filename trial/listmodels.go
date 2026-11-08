package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Model availability and quota are properties of YOUR account, not of the
// documentation. A model can be listed and still refuse the call (retired for
// new users), or be callable and out of quota. The only reliable answer is to
// ask, so this does: list what the account can see, then make the smallest
// possible real call to each candidate and report what came back.
//
// The probe calls request one output token on a two-word prompt, so the cost is
// a rounding error and the quota consumed is negligible.

// geminiCandidates: the cheap models worth using for this study, cheapest first.
// Capability is irrelevant here — we need a model that fetches a page and quotes
// a code, not one that reasons.
var geminiCandidates = []string{
	"gemini-2.5-flash-lite",
	"gemini-3.1-flash-lite",
	"gemini-3.5-flash-lite",
	"gemini-3-flash-preview",
	"gemini-3.5-flash",
	"gemini-3.6-flash",
	"gemini-3.7-flash",
	"gemini-2.5-flash",
	"gemini-3.1-pro-preview",
}

type geminiModelInfo struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName"`
	Methods     []string `json:"supportedGenerationMethods"`
	InputLimit  int      `json:"inputTokenLimit"`
}

// listGeminiModels asks the account what it can see.
func listGeminiModels(ctx context.Context, key string) ([]geminiModelInfo, error) {
	raw, err := httpGet(ctx, geminiDirectBase+"/models?pageSize=200",
		map[string]string{"x-goog-api-key": key})
	if err != nil {
		return nil, err
	}
	var doc struct {
		Models []geminiModelInfo `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc.Models, nil
}

// probeGeminiModel makes the smallest real call possible and classifies the
// outcome. This is what distinguishes "listed" from "usable right now".
func probeGeminiModel(parent context.Context, key, model string) (status, detail string) {
	// Bounded per probe. Inheriting the 5-minute overall deadline meant one slow
	// model looked indistinguishable from a hung program.
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	body := map[string]any{
		"contents":         []map[string]any{{"parts": []map[string]any{{"text": "hi"}}}},
		"generationConfig": map[string]any{"maxOutputTokens": 1},
	}
	_, err := httpPost(ctx, fmt.Sprintf("%s/models/%s:generateContent", geminiDirectBase, model),
		map[string]string{"x-goog-api-key": key}, body)
	if err == nil {
		return "USABLE", ""
	}
	e := err.Error()
	switch {
	case ctx.Err() != nil:
		return "too slow (>45s)", "not usable for this study regardless"
	case strings.Contains(e, "429"):
		return "OUT OF QUOTA", firstSentence(e)
	case strings.Contains(e, "404"):
		return "not available", firstSentence(e)
	case strings.Contains(e, "403"):
		return "forbidden", firstSentence(e)
	case strings.Contains(e, "400"):
		return "rejected", firstSentence(e)
	default:
		return "error", truncate(e, 120)
	}
}

func firstSentence(s string) string {
	// Error bodies are JSON; pull out the human message if there is one.
	if i := strings.Index(s, `"message": "`); i >= 0 {
		rest := s[i+len(`"message": "`):]
		if j := strings.Index(rest, `"`); j > 0 {
			return truncate(rest[:j], 150)
		}
	}
	return truncate(s, 150)
}

// probeAllModels keeps probing after the first success. Off by default: the
// candidates are ordered cheapest first, so the first hit answers the question
// and every further call is money spent on a model we will not use.
var probeAllModels = false

// runListModels prints what each provider offers and, for Gemini, what is
// actually callable on this account right now.
func runListModels() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		fmt.Println("GEMINI (Developer API) — what this key can see")
		fmt.Println(strings.Repeat("-", 70))
		models, err := listGeminiModels(ctx, key)
		if err != nil {
			fmt.Printf("  could not list models: %s\n", truncate(err.Error(), 200))
		} else {
			var ids []string
			for _, m := range models {
				id := strings.TrimPrefix(m.Name, "models/")
				for _, meth := range m.Methods {
					if meth == "generateContent" {
						ids = append(ids, id)
						break
					}
				}
			}
			sort.Strings(ids)
			fmt.Printf("  %d models support generateContent:\n", len(ids))
			for _, id := range ids {
				fmt.Printf("    %s\n", id)
			}
		}

		fmt.Println()
		fmt.Println("  Which of the cheap ones actually work RIGHT NOW")
		fmt.Println("  (one real call each, 1 output token — costs a rounding error)")
		fmt.Println(strings.Repeat("-", 70))
		anyUsable := ""
		for _, m := range geminiCandidates {
			// Name it before the call, so a slow probe is attributable rather
			// than looking like the program has stopped.
			fmt.Printf("     %-26s ", m)
			os.Stdout.Sync()
			status, detail := probeGeminiModel(ctx, key, m)
			fmt.Printf("%s\n", status)
			if detail != "" {
				fmt.Printf("       %s\n", detail)
			}
			if status == "USABLE" {
				anyUsable = m
				if !probeAllModels {
					fmt.Printf("\n  Stopping here — candidates are ordered cheapest first, so this is\n")
					fmt.Printf("  the cheapest usable one. Use -all-models to probe every candidate.\n")
					break
				}
			}
		}
		fmt.Println()
		if anyUsable != "" {
			fmt.Printf("  Cheapest usable model: %s\n", anyUsable)
			fmt.Printf("  Use it with:  -google-model %s\n", anyUsable)
		} else {
			fmt.Println("  Nothing is callable. If everything says OUT OF QUOTA, the cap is")
			fmt.Println("  per-project per-day rather than per-model — wait for the reset or")
			fmt.Println("  enable billing. Run the other providers meanwhile.")
		}
		fmt.Println()
	}

	if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
		fmt.Println("OPENROUTER — models by vendor (listing only, not a callability test)")
		fmt.Println(strings.Repeat("-", 70))
		ids, err := listModels(ctx, "or", key)
		if err != nil {
			fmt.Printf("  could not list: %s\n", truncate(err.Error(), 150))
		} else {
			byVendor := map[string][]string{}
			for _, id := range ids {
				byVendor[vendorOf(id)] = append(byVendor[vendorOf(id)], id)
			}
			for _, v := range []string{"openai", "anthropic", "google", "perplexity", "x-ai"} {
				list := byVendor[v]
				sort.Strings(list)
				fmt.Printf("  %s (%d)\n", v, len(list))
				for i, id := range list {
					if i == 8 {
						fmt.Printf("      … and %d more\n", len(list)-8)
						break
					}
					fmt.Printf("      %s\n", id)
				}
			}
		}
		fmt.Println()
	}
}
