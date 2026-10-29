package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"claude2api/config"
	"claude2api/models"
)

func testTool(name string) models.AnthropicTool {
	return models.AnthropicTool{Name: name, InputSchema: map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{"value": map[string]interface{}{"type": "string"}},
		"required": []string{"value"}, "additionalProperties": false,
	}}
}

func TestClientToolParsing(t *testing.T) {
	for _, tc := range []struct {
		name, output, choice string
		parallel, wantError  bool
	}{
		{"nested-and-marker-in-string", `before[TOOL_CALL]{"name":"Read","input":{"value":"nested } and [/TOOL_CALL]"}}[/TOOL_CALL]after`, "auto", false, false},
		{"missing-argument", `[TOOL_CALL]{"name":"Read","input":{}}[/TOOL_CALL]`, "auto", false, true},
		{"wrong-type", `[TOOL_CALL]{"name":"Read","input":{"value":3}}[/TOOL_CALL]`, "auto", false, true},
		{"unknown-tool", `[TOOL_CALL]{"name":"Bash","input":{"value":"x"}}[/TOOL_CALL]`, "auto", false, true},
		{"missing-close", `[TOOL_CALL]{"name":"Read","input":{"value":"x"}}`, "auto", false, true},
		{"invalid-json", `[TOOL_CALL]{broken}[/TOOL_CALL]`, "auto", false, true},
		{"array-input", `[TOOL_CALL]{"name":"Read","input":[]}[/TOOL_CALL]`, "auto", false, true},
		{"null-input", `[TOOL_CALL]{"name":"Read","input":null}[/TOOL_CALL]`, "auto", false, true},
		{"tools-disabled", `[TOOL_CALL]{"name":"Read","input":{"value":"x"}}[/TOOL_CALL]`, "none", false, true},
		{"required-call", `plain text`, "any", false, true},
		{"forced-tool", `[TOOL_CALL]{"name":"Read","input":{"value":"x"}}[/TOOL_CALL]`, "tool", false, false},
		{"unmatched-close", `text[/TOOL_CALL]`, "auto", false, true},
		{"parallel-disabled", strings.Repeat(`[TOOL_CALL]{"name":"Read","input":{"value":"x"}}[/TOOL_CALL]`, 2), "auto", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools, err := prepareClientTools(models.AnthropicRequest{ToolDefs: []models.AnthropicTool{testTool("Read")}, ToolChoice: models.AnthropicToolChoice{Type: tc.choice, Name: "Read", DisableParallelToolUse: tc.parallel}})
			if err != nil {
				t.Fatal(err)
			}
			blocks, stop, err := tools.parse(tc.output)
			if (err != nil) != tc.wantError {
				t.Fatalf("parse error=%v, blocks=%+v", err, blocks)
			}
			if !tc.wantError && stop != "tool_use" {
				t.Fatalf("stop=%s", stop)
			}
		})
	}
}

func toolUpstream(t *testing.T, completion func(http.ResponseWriter, models.ClaudeCompletionRequest)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/organizations":
			io.WriteString(w, `[{"uuid":"org"}]`)
		case strings.HasSuffix(r.URL.Path, "/completion"):
			var req models.ClaudeCompletionRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			completion(w, req)
		case r.Method == "DELETE":
			w.WriteHeader(204)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/chat_conversations"):
			io.WriteString(w, `{"uuid":"conversation"}`)
		default:
			w.WriteHeader(404)
		}
	}))
}

func webText(w http.ResponseWriter, text string, stop bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	// Deliberately split markers and JSON across upstream deltas.
	for _, part := range []string{text[:len(text)/2], text[len(text)/2:]} {
		data, _ := json.Marshal(map[string]interface{}{"type": "content_block_delta", "delta": map[string]string{"type": "text_delta", "text": part}})
		fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", data)
	}
	if stop {
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}
}

func postToolRequest(t *testing.T, router http.Handler, req models.AnthropicRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}

func readToolResponse(t *testing.T, w *httptest.ResponseRecorder, stream bool) models.AnthropicResponse {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	var response models.AnthropicResponse
	if !stream {
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
	var event string
	var arguments strings.Builder
	started, stopped := false, false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var payload struct {
			Type         string                       `json:"type"`
			Index        int                          `json:"index"`
			ContentBlock models.AnthropicContentBlock `json:"content_block"`
			Delta        struct {
				Text, Type  string
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			t.Fatal(err)
		}
		if event != payload.Type {
			t.Fatalf("event %q disagrees with type %q", event, payload.Type)
		}
		switch event {
		case "message_start":
			started = true
		case "content_block_start":
			if payload.Index != len(response.Content) {
				t.Fatal("non-contiguous block indices")
			}
			if payload.ContentBlock.Type == "tool_use" && len(payload.ContentBlock.Input) != 0 {
				t.Fatal("tool input repeated in block_start")
			}
			response.Content = append(response.Content, payload.ContentBlock)
			arguments.Reset()
		case "content_block_delta":
			if payload.Delta.Type == "input_json_delta" {
				arguments.WriteString(payload.Delta.PartialJSON)
			} else {
				response.Content[payload.Index].Text += payload.Delta.Text
			}
		case "content_block_stop":
			if response.Content[payload.Index].Type == "tool_use" {
				if err := json.Unmarshal([]byte(arguments.String()), &response.Content[payload.Index].Input); err != nil {
					t.Fatalf("invalid streamed arguments: %s: %v", arguments.String(), err)
				}
			}
		case "message_delta":
			response.StopReason = payload.Delta.StopReason
		case "message_stop":
			stopped = true
		case "error":
			t.Fatalf("stream error: %s", line)
		}
	}
	if scanner.Err() != nil || !started || !stopped {
		t.Fatal("incomplete SSE lifecycle")
	}
	return response
}

func TestClientToolRoundTripNeverExecutesOnServer(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "client-owned.txt")
			bashPath := filepath.Join(t.TempDir(), "must-not-exist")
			var count atomic.Int32
			upstream := toolUpstream(t, func(w http.ResponseWriter, req models.ClaudeCompletionRequest) {
				if string(req.Tools) != "[]" {
					t.Error("web tools must be disabled")
				}
				switch count.Add(1) {
				case 1:
					input, _ := json.Marshal(map[string]string{"file_path": path, "content": "written by client"})
					bash, _ := json.Marshal(map[string]string{"command": "touch " + bashPath})
					webText(w, "before[TOOL_CALL]{\"name\":\"Write\",\"id\":\"reused\",\"input\":"+string(input)+"}[/TOOL_CALL]between[TOOL_CALL]{\"name\":\"Bash\",\"id\":\"reused\",\"input\":"+string(bash)+"}[/TOOL_CALL]after", true)
				case 2:
					if !strings.Contains(req.Prompt, "tool_use_id") || !strings.Contains(req.Prompt, "LOCAL_PERMISSION_DENIED") || !strings.Contains(req.Prompt, `"is_error":true`) {
						t.Error("lost local tool results")
					}
					input, _ := json.Marshal(map[string]string{"file_path": path})
					webText(w, "[TOOL_CALL]{\"name\":\"Read\",\"input\":"+string(input)+"}[/TOOL_CALL]", true)
				case 3:
					if !strings.Contains(req.Prompt, "written by client") {
						t.Error("lost result from second local tool round")
					}
					webText(w, "Local results received.", true)
				default:
					t.Error("proxy attempted an internal tool loop")
				}
			})
			defer upstream.Close()
			router := modelsTestRouter(upstream.URL, []config.Account{{SessionKey: "test"}})
			schema := map[string]interface{}{"type": "object"}
			req := models.AnthropicRequest{Model: "claude-future", Stream: stream,
				ToolDefs: []models.AnthropicTool{{Name: "Write", InputSchema: schema}, {Name: "Bash", InputSchema: schema}, {Name: "Read", InputSchema: schema}},
				Messages: []models.AnthropicMessage{{Role: "user", Content: "Create my file locally."}},
			}
			response := readToolResponse(t, postToolRequest(t, router, req), stream)
			if response.StopReason != "tool_use" || len(response.Content) != 5 || count.Load() != 1 {
				t.Fatalf("wrong response: %+v, calls=%d", response, count.Load())
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("proxy wrote client file: %v", err)
			}
			if _, err := os.Stat(bashPath); !os.IsNotExist(err) {
				t.Fatalf("proxy executed Bash: %v", err)
			}
			if response.Content[1].ID == response.Content[3].ID {
				t.Fatal("tool IDs collided")
			}
			// The client executes its selected tool after receiving the response.
			if err := os.WriteFile(path, []byte("written by client"), 0600); err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(response.Content)
			var assistant []interface{}
			json.Unmarshal(encoded, &assistant)
			req.Messages = append(req.Messages, models.AnthropicMessage{Role: "assistant", Content: assistant}, models.AnthropicMessage{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": response.Content[1].ID, "content": []interface{}{map[string]interface{}{"type": "text", "text": "local write completed"}}},
				map[string]interface{}{"type": "tool_result", "tool_use_id": response.Content[3].ID, "is_error": true, "content": "LOCAL_PERMISSION_DENIED"},
			}})
			response = readToolResponse(t, postToolRequest(t, router, req), stream)
			if response.StopReason != "tool_use" || response.Content[0].Name != "Read" || count.Load() != 2 {
				t.Fatalf("second tool round failed: %+v", response)
			}
			encoded, _ = json.Marshal(response.Content)
			assistant = nil
			json.Unmarshal(encoded, &assistant)
			localContent, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			req.Messages = append(req.Messages, models.AnthropicMessage{Role: "assistant", Content: assistant}, models.AnthropicMessage{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": response.Content[0].ID, "content": string(localContent)},
			}})
			// A final turn may omit tool definitions while retaining tool history.
			req.ToolDefs = nil
			response = readToolResponse(t, postToolRequest(t, router, req), stream)
			if response.StopReason != "end_turn" || response.Content[0].Text != "Local results received." || count.Load() != 3 {
				t.Fatalf("continuation failed: %+v", response)
			}
		})
	}
}

func TestClientToolUpstreamFailures(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, output := range []string{`[TOOL_CALL]{broken}[/TOOL_CALL]`, `[TOOL_CALL]{"name":"Other","input":{}}[/TOOL_CALL]`, "TRUNCATED"} {
			t.Run(fmt.Sprintf("%t/%s", stream, output), func(t *testing.T) {
				upstream := toolUpstream(t, func(w http.ResponseWriter, _ models.ClaudeCompletionRequest) {
					webText(w, output, output != "TRUNCATED")
				})
				defer upstream.Close()
				router := modelsTestRouter(upstream.URL, []config.Account{{SessionKey: "test"}})
				w := postToolRequest(t, router, models.AnthropicRequest{Stream: stream, ToolDefs: []models.AnthropicTool{testTool("Read")}, Messages: []models.AnthropicMessage{{Role: "user", Content: "test"}}})
				if stream {
					if !strings.Contains(w.Body.String(), "event: error\n") || strings.Contains(w.Body.String(), "event: message_stop\n") || strings.Contains(w.Body.String(), "event: content_block_start\n") {
						t.Fatalf("unsafe stream: %s", w.Body.String())
					}
				} else if w.Code != 502 {
					t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestClientToolBadRequests(t *testing.T) {
	for _, body := range []string{
		`{"tools":[{"name":"Read","input_schema":{"$ref":"http://127.0.0.1/schema"}}]}`,
		`{"tool_choice":{"type":"tool","name":"Missing"}}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"missing","content":"x"}]}]}`,
		`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"Read","input":{}}]},{"role":"user","content":"missing result"}]}`,
	} {
		var req models.AnthropicRequest
		json.Unmarshal([]byte(body), &req)
		if len(req.Messages) == 0 {
			req.Messages = []models.AnthropicMessage{{Role: "user", Content: "test"}}
		}
		router := modelsTestRouter("http://127.0.0.1:1", []config.Account{{SessionKey: "test"}})
		w := postToolRequest(t, router, req)
		if w.Code != 400 {
			t.Fatalf("invalid request reached upstream: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestClientToolHistoryValidation(t *testing.T) {
	for _, history := range []string{
		`[{"role":"assistant","content":[{"type":"tool_use","id":"a"},{"type":"tool_use","id":"a"}]}]`,
		`[{"role":"assistant","content":[{"type":"tool_use","id":"a"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a"},{"type":"tool_result","tool_use_id":"a"}]}]`,
		`[{"role":"assistant","content":[{"type":"tool_use","id":"a"}]},{"role":"assistant","content":"invented result"}]`,
		`[{"role":"user","content":[{"type":"tool_use","id":"a"}]}]`,
	} {
		var messages []models.AnthropicMessage
		if err := json.Unmarshal([]byte(history), &messages); err != nil {
			t.Fatal(err)
		}
		if err := validateToolHistory(messages); err == nil {
			t.Fatalf("accepted invalid history: %s", history)
		}
	}
}

func TestClientToolLocalSchemaRefAndEmptyInput(t *testing.T) {
	var schema map[string]interface{}
	json.Unmarshal([]byte(`{"type":"object","properties":{"nested":{"$ref":"#/$defs/nested"}},"$defs":{"nested":{"type":"object","required":["value"]}}}`), &schema)
	tools, err := prepareClientTools(models.AnthropicRequest{ToolDefs: []models.AnthropicTool{{Name: "Read", InputSchema: schema}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tools.parse(`[TOOL_CALL]{"name":"Read","input":{"nested":{}}}[/TOOL_CALL]`); err == nil {
		t.Fatal("local schema ref was not validated")
	}
	blocks, _, err := tools.parse(`[TOOL_CALL]{"name":"Read","input":{}}[/TOOL_CALL]`)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(blocks[0])
	if !strings.Contains(string(data), `"input":{}`) {
		t.Fatalf("empty tool input lost: %s", data)
	}
	data, _ = json.Marshal(models.AnthropicContentBlock{Type: "text"})
	if !strings.Contains(string(data), `"text":""`) {
		t.Fatalf("empty text lost: %s", data)
	}
}

func TestClientToolCancellation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			entered, upstreamDone := make(chan struct{}), make(chan struct{})
			upstream := toolUpstream(t, func(w http.ResponseWriter, req models.ClaudeCompletionRequest) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
				w.(http.Flusher).Flush()
				close(entered)
				<-upstreamDone
			})
			defer upstream.Close()
			defer close(upstreamDone)
			router := modelsTestRouter(upstream.URL, []config.Account{{SessionKey: "test"}})
			body, _ := json.Marshal(models.AnthropicRequest{Stream: stream, ToolDefs: []models.AnthropicTool{testTool("Read")}, Messages: []models.AnthropicMessage{{Role: "user", Content: "test"}}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(body))).WithContext(ctx)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { router.ServeHTTP(w, r); close(done) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream did not start")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("canceled request did not terminate")
			}
			if strings.Contains(w.Body.String(), `"type":"tool_use"`) || strings.Contains(w.Body.String(), "event: message_stop\n") {
				t.Fatalf("canceled request succeeded: %s", w.Body.String())
			}
		})
	}
}
