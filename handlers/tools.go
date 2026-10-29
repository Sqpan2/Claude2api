package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"claude2api/models"
	"claude2api/utils"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// Tool execution belongs to the API client. Only validate and translate here.
type clientTools struct {
	schemas map[string]*jsonschema.Schema
	choice  models.AnthropicToolChoice
}

func prepareClientTools(req models.AnthropicRequest) (*clientTools, error) {
	t := &clientTools{schemas: make(map[string]*jsonschema.Schema), choice: req.ToolChoice}
	for i, tool := range req.ToolDefs {
		if tool.Name == "" {
			return nil, fmt.Errorf("tools[%d].name is required", i)
		}
		if _, exists := t.schemas[tool.Name]; exists {
			return nil, fmt.Errorf("duplicate tool name %q", tool.Name)
		}
		if tool.InputSchema == nil {
			return nil, fmt.Errorf("tool %q requires input_schema", tool.Name)
		}
		data, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("invalid input_schema for tool %q", tool.Name)
		}
		compiler := jsonschema.NewCompiler()
		// Client-supplied schemas must never cause external network requests.
		compiler.LoadURL = func(string) (io.ReadCloser, error) {
			return nil, fmt.Errorf("external schema references are not supported")
		}
		url := "https://claude2api.invalid/tool.json"
		if err := compiler.AddResource(url, strings.NewReader(string(data))); err != nil {
			return nil, fmt.Errorf("invalid input_schema for tool %q: %w", tool.Name, err)
		}
		schema, err := compiler.Compile(url)
		if err != nil {
			return nil, fmt.Errorf("invalid input_schema for tool %q: %w", tool.Name, err)
		}
		t.schemas[tool.Name] = schema
	}
	switch t.choice.Type {
	case "", "auto", "none":
	case "any":
		if len(t.schemas) == 0 {
			return nil, fmt.Errorf("tool_choice any requires tools")
		}
	case "tool":
		if _, exists := t.schemas[t.choice.Name]; !exists {
			return nil, fmt.Errorf("tool_choice names an undefined tool")
		}
	default:
		return nil, fmt.Errorf("unsupported tool_choice %q", t.choice.Type)
	}
	return t, validateToolHistory(req.Messages)
}

func validateToolHistory(messages []models.AnthropicMessage) error {
	pending := make(map[string]bool)
	seen := make(map[string]bool)
	for _, message := range messages {
		blocks, _ := message.Content.([]interface{})
		if len(pending) > 0 && message.Role != "user" {
			return fmt.Errorf("tool_use must be followed by user tool_result blocks")
		}
		for _, item := range blocks {
			block, _ := item.(map[string]interface{})
			switch block["type"] {
			case "tool_use":
				id, _ := block["id"].(string)
				if message.Role != "assistant" || id == "" || seen[id] {
					return fmt.Errorf("tool_use requires an assistant message and a unique id")
				}
				seen[id], pending[id] = true, true
			case "tool_result":
				id, _ := block["tool_use_id"].(string)
				if message.Role != "user" || !pending[id] {
					return fmt.Errorf("tool_result must reference a pending tool_use_id")
				}
				delete(pending, id)
			}
		}
		if message.Role == "user" && len(pending) > 0 {
			return fmt.Errorf("user message must include results for all pending tool calls")
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("missing tool_result for pending tool calls")
	}
	return nil
}

func hasToolHistory(messages []models.AnthropicMessage) bool {
	for _, message := range messages {
		blocks, _ := message.Content.([]interface{})
		for _, item := range blocks {
			block, _ := item.(map[string]interface{})
			if block["type"] == "tool_use" || block["type"] == "tool_result" {
				return true
			}
		}
	}
	return false
}

func (t *clientTools) parse(text string) ([]models.AnthropicContentBlock, string, error) {
	blocks := make([]models.AnthropicContentBlock, 0)
	calls := 0
	for {
		start := strings.Index(text, "[TOOL_CALL]")
		if start < 0 {
			if strings.Contains(text, "[/TOOL_CALL]") {
				return nil, "", fmt.Errorf("unmatched tool-call closing marker")
			}
			if text != "" {
				blocks = append(blocks, models.AnthropicContentBlock{Type: "text", Text: text})
			}
			break
		}
		if start > 0 {
			blocks = append(blocks, models.AnthropicContentBlock{Type: "text", Text: text[:start]})
		}
		rest := text[start+len("[TOOL_CALL]"):]
		var call struct {
			Name  string                 `json:"name"`
			Input map[string]interface{} `json:"input"`
		}
		decoder := json.NewDecoder(strings.NewReader(rest))
		if err := decoder.Decode(&call); err != nil {
			return nil, "", fmt.Errorf("invalid tool-call JSON")
		}
		rest = strings.TrimLeft(rest[decoder.InputOffset():], " \t\r\n")
		if !strings.HasPrefix(rest, "[/TOOL_CALL]") {
			return nil, "", fmt.Errorf("missing tool-call closing marker")
		}
		schema, exists := t.schemas[call.Name]
		if !exists || t.choice.Type == "none" {
			return nil, "", fmt.Errorf("model requested an unavailable tool")
		}
		if t.choice.Type == "tool" && call.Name != t.choice.Name {
			return nil, "", fmt.Errorf("model did not follow tool_choice")
		}
		if call.Input == nil {
			return nil, "", fmt.Errorf("tool input must be a JSON object")
		}
		if err := schema.Validate(call.Input); err != nil {
			return nil, "", fmt.Errorf("model tool input does not match input_schema for %q", call.Name)
		}
		calls++
		if calls > 64 || (t.choice.DisableParallelToolUse && calls > 1) {
			return nil, "", fmt.Errorf("model returned too many tool calls")
		}
		// Proxy-generated IDs cannot collide with model IDs reused on later turns.
		blocks = append(blocks, models.AnthropicContentBlock{
			Type: "tool_use", ID: "toolu_" + utils.GenerateUUID(), Name: call.Name, Input: call.Input,
		})
		text = rest[len("[/TOOL_CALL]"):]
	}
	if calls > 0 {
		return blocks, "tool_use", nil
	}
	if t.choice.Type == "any" || t.choice.Type == "tool" {
		return nil, "", fmt.Errorf("model did not return the required tool call")
	}
	if len(blocks) == 0 {
		blocks = append(blocks, models.AnthropicContentBlock{Type: "text", Text: ""})
	}
	return blocks, "end_turn", nil
}

func buildToolDefsPrompt(req models.AnthropicRequest) string {
	tools, _ := json.Marshal(req.ToolDefs)
	choice, _ := json.Marshal(req.ToolChoice)
	return "\n\n[Client Tools]\n" +
		"The tools below run on the user's client, never on this web server. " +
		"To request a tool, output [TOOL_CALL]{\"name\":\"tool_name\",\"input\":{...}}[/TOOL_CALL]. " +
		"Input must match that tool's input_schema. Only request listed tools. " +
		"You may include explanatory text and multiple calls. After requesting tools, stop and wait for the user's tool results. " +
		"Do not invent results or execute client tasks using web tools. " +
		"If no tools are listed or tool_choice is none, respond with text only. " +
		"tool_choice any requires a call; tool_choice tool requires its named tool. " +
		"If disable_parallel_tool_use is true, request at most one tool.\n" +
		"tools: " + string(tools) + "\ntool_choice: " + string(choice) + "\n[/Client Tools]\n"
}

func buildToolPrompt(req models.AnthropicRequest) string {
	parts := []string{"[System]\n" + systemToText(req.System) + buildToolDefsPrompt(req)}
	for _, message := range req.Messages {
		parts = append(parts, "["+message.Role+"]\n"+anthropicContentToToolText(message.Content))
	}
	parts = append(parts, "[assistant]\n")
	return strings.Join(parts, "\n\n")
}

func anthropicContentToToolText(content interface{}) string {
	if text, ok := content.(string); ok {
		return text
	}
	// Preserve tool_use_id, nested results, is_error and arguments verbatim.
	data, _ := json.Marshal(content)
	return string(data)
}
