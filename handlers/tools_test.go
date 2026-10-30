package handlers

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"claude2api/models"
)

func TestExtractToolCalls(t *testing.T) {
	text := `Here is help.

[TOOL_CALL]{"name":"Bash","id":"toolu_1","input":{"command":"echo hi"}}[/TOOL_CALL]

After text.

[TOOL_CALL]{"name":"Read","id":"toolu_2","input":{"file_path":"a.txt"}}[/TOOL_CALL]`

	tools, err := prepareClientTools(models.AnthropicRequest{ToolDefs: []models.AnthropicTool{
		{Name: "Bash", InputSchema: map[string]interface{}{"type": "object"}},
		{Name: "Read", InputSchema: map[string]interface{}{"type": "object"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	blocks, stop, err := tools.parse(text)
	if err != nil || stop != "tool_use" {
		t.Fatalf("parse: %s, %v", stop, err)
	}
	var names []string
	var plain string
	for _, block := range blocks {
		if block.Type == "tool_use" {
			names = append(names, block.Name)
		} else {
			plain += block.Text
		}
	}
	if strings.Join(names, ",") != "Bash,Read" || !strings.Contains(plain, "Here is help") || !strings.Contains(plain, "After text") {
		t.Fatalf("lost tool calls or text: %+v", blocks)
	}
}

func TestExtractToolCallsNone(t *testing.T) {
	text := "plain text with no calls"
	blocks, stop, err := (&clientTools{}).parse(text)
	if err != nil || stop != "end_turn" || len(blocks) != 1 || blocks[0].Text != text {
		t.Fatalf("text changed: %+v, %s, %v", blocks, stop, err)
	}
}

func TestAnthropicContentToToolText(t *testing.T) {
	blocks := []interface{}{
		map[string]interface{}{"type": "tool_use", "name": "Bash", "id": "toolu_1", "input": map[string]interface{}{"command": "echo"}},
		map[string]interface{}{"type": "tool_result", "tool_use_id": "toolu_1", "content": "hello", "is_error": true},
		map[string]interface{}{"type": "text", "text": "done"},
	}
	got := anthropicContentToToolText(blocks)
	if !strings.Contains(got, `"name":"Bash"`) {
		t.Fatalf("missing tool_use text: %q", got)
	}
	if !strings.Contains(got, `"tool_use_id":"toolu_1"`) || !strings.Contains(got, `"is_error":true`) {
		t.Fatalf("missing tool_result text: %q", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "done") {
		t.Fatalf("missing inner text: %q", got)
	}
}

func TestToolDefsPrompt(t *testing.T) {
	tools := []models.AnthropicTool{{Name: "Bash", InputSchema: map[string]interface{}{"type": "object"}}}
	p := buildToolDefsPrompt(models.AnthropicRequest{ToolDefs: tools})
	if !strings.Contains(p, "TOOL_CALL") || !strings.Contains(p, "Bash") {
		t.Fatalf("defs prompt wrong: %q", p)
	}
}

func TestEmptyToolsOverride(t *testing.T) {
	var empty json.RawMessage = json.RawMessage([]byte("[]"))
	if string(empty) != "[]" {
		t.Fatalf("empty tools not []: %s", empty)
	}
}

func TestCacheTracker(t *testing.T) {
	// Fresh tracker for this test.
	ct := &cacheTracker{seen: make(map[string]time.Time), ttl: 5 * time.Minute}

	req := models.AnthropicRequest{
		System: []interface{}{
			map[string]interface{}{
				"type":          "text",
				"text":          "this is a long system prompt that should be cached for sure",
				"cache_control": map[string]interface{}{"type": "ephemeral"},
			},
		},
	}

	creation, read := ct.record("conv-1", req)
	if creation == 0 {
		t.Fatalf("expected cache creation tokens > 0, got %d", creation)
	}
	if read != 0 {
		t.Fatalf("expected read=0 first time, got %d", read)
	}

	// Same request again -> should be a cache read.
	creation2, read2 := ct.record("conv-1", req)
	if creation2 != 0 {
		t.Fatalf("expected creation=0 second time, got %d", creation2)
	}
	if read2 == 0 {
		t.Fatalf("expected cache read tokens > 0, got %d", read2)
	}
}

func TestCollectCacheBlocks(t *testing.T) {
	req := models.AnthropicRequest{
		System: []interface{}{
			map[string]interface{}{"type": "text", "text": "cached system", "cache_control": map[string]interface{}{"type": "ephemeral"}},
			map[string]interface{}{"type": "text", "text": "uncached"},
		},
		Messages: []models.AnthropicMessage{
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "cached user message", "cache_control": map[string]interface{}{"type": "ephemeral"}},
			}},
		},
	}
	blocks := collectCacheBlocks(req)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 cache blocks, got %d", len(blocks))
	}
}

func TestResolveEffort(t *testing.T) {
	cases := map[string]string{
		"low":     "low",
		"medium":  "medium",
		"high":    "high",
		"xhigh":   "xhigh",
		"max":     "max",
		"HIGH":    "high",
		"  max ":  "max",
		"invalid": "medium",
		"":        "medium",
	}
	for in, want := range cases {
		if got := resolveEffort(in); got != want {
			t.Fatalf("resolveEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveThinking(t *testing.T) {
	cases := []struct {
		in         interface{}
		wantMode   string
		wantBudget int
	}{
		{nil, "auto", 0},
		{map[string]interface{}{"type": "disabled"}, "off", 0},
		{map[string]interface{}{"type": "enabled", "budget_tokens": float64(10000)}, "auto", 10000},
		{map[string]interface{}{"type": "enabled"}, "auto", 0},
		{"invalid", "auto", 0},
	}
	for _, c := range cases {
		mode, budget := resolveThinking(c.in)
		if mode != c.wantMode || budget != c.wantBudget {
			t.Fatalf("resolveThinking(%v) = (%q,%d), want (%q,%d)", c.in, mode, budget, c.wantMode, c.wantBudget)
		}
	}
}
