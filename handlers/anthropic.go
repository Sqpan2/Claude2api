package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"claude2api/claude"
	"claude2api/models"

	"github.com/gin-gonic/gin"
)

func (h *Handler) AnthropicMessages(c *gin.Context) {
	var req models.AnthropicRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "invalid request body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		badRequest(c, "messages must contain at least one message")
		return
	}
	model, err := resolveModel(req.Model, h.cfg.DefaultModel)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	var tools *clientTools
	if len(req.ToolDefs) > 0 || hasToolHistory(req.Messages) || req.ToolChoice.Type != "" {
		tools, err = prepareClientTools(req)
		if err != nil {
			badRequest(c, err.Error())
			return
		}
	}
	lease, err := h.acquireClient(c, req.ConversationID)
	if err != nil {
		internalError(c, "create client: "+err.Error())
		return
	}
	defer lease.release()
	effort := resolveEffort(h.cfg.Effort)
	if req.Stream {
		if tools == nil {
			h.anthropicTextStream(c, lease.client, req, model, effort, lease.accountID)
		} else {
			h.anthropicToolStream(c, lease.client, req, model, effort, lease.accountID, tools)
		}
		return
	}
	response, err := h.anthropicTurn(c.Request.Context(), lease.client, req, model, effort, lease.accountID, tools)
	if err != nil {
		upstreamError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, response)
}

func systemToText(system interface{}) string {
	if text, ok := system.(string); ok {
		return text
	}
	var parts []string
	if blocks, ok := system.([]interface{}); ok {
		for _, item := range blocks {
			if block, ok := item.(map[string]interface{}); ok && block["type"] == "text" {
				if text, ok := block["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

func buildAnthropicPrompt(req models.AnthropicRequest) string {
	parts := []string{"[System]\n" + systemToText(req.System)}
	for _, message := range req.Messages {
		parts = append(parts, "["+message.Role+"]\n"+anthropicContentToToolText(message.Content))
	}
	return strings.Join(append(parts, "[assistant]\n"), "\n\n")
}

func (h *Handler) anthropicTurn(ctx context.Context, client *claude.Client, req models.AnthropicRequest, model, effort, accountID string, tools *clientTools) (models.AnthropicResponse, error) {
	prompt := buildAnthropicPrompt(req)
	var webTools []json.RawMessage
	if tools != nil {
		prompt = buildToolPrompt(req)
		webTools = []json.RawMessage{json.RawMessage("[]")}
	}
	thinking, _ := resolveThinking(h.cfg.Thinking)
	if req.Thinking != nil {
		thinking, _ = resolveThinking(req.Thinking)
	}
	_, text, err := h.runCompletion(ctx, client, prompt, model, effort, req.ConversationID, accountID, nil, webTools, thinking)
	if err != nil {
		return models.AnthropicResponse{}, err
	}
	blocks := []models.AnthropicContentBlock{{Type: "text", Text: text}}
	stop := "end_turn"
	if tools != nil {
		blocks, stop, err = tools.parse(text)
		if err != nil {
			return models.AnthropicResponse{}, fmt.Errorf("invalid upstream tool response: %w", err)
		}
	}
	// Web thinking has no API signature. Do not fabricate signed thinking blocks.
	return models.AnthropicResponse{
		ID: genID("msg_"), Type: "message", Role: "assistant", Model: model,
		Content: blocks, StopReason: stop,
		Usage: models.AnthropicUsage{InputTokens: len(prompt) / 4, OutputTokens: len(text) / 4},
	}, nil
}

func startAnthropicStream(c *gin.Context, model string) string {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	id := genID("msg_")
	writeAnthropicSSE(c.Writer, models.AnthropicStreamMessageStart{
		Type: "message_start", Message: models.AnthropicStartMsg{
			ID: id, Type: "message", Role: "assistant", Model: model,
			Content: []models.AnthropicContentBlock{},
		},
	})
	c.Writer.Flush()
	return id
}

func finishAnthropicStream(c *gin.Context, stop string, usage models.AnthropicUsage) {
	writeAnthropicSSE(c.Writer, models.AnthropicStreamMessageDelta{
		Type: "message_delta", Delta: models.AnthropicStopDelta{StopReason: stop}, Usage: usage,
	})
	writeAnthropicSSE(c.Writer, models.AnthropicStreamMessageStop{Type: "message_stop"})
	c.Writer.Flush()
}

func anthropicStreamError(c *gin.Context, err error) {
	writeAnthropicSSE(c.Writer, gin.H{
		"type": "error", "error": gin.H{"type": "api_error", "message": err.Error()},
	})
	c.Writer.Flush()
}

func (h *Handler) anthropicToolStream(c *gin.Context, client *claude.Client, req models.AnthropicRequest, model, effort, accountID string, tools *clientTools) {
	startAnthropicStream(c, model)
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	type result struct {
		response models.AnthropicResponse
		err      error
	}
	completed := make(chan result, 1)
	go func() {
		response, err := h.anthropicTurn(ctx, client, req, model, effort, accountID, tools)
		completed <- result{response, err}
	}()
	// Validate the entire tool turn before exposing executable arguments. Named
	// pings keep gateways alive while the web model completes its text response.
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			writeAnthropicSSE(c.Writer, gin.H{"type": "ping"})
			c.Writer.Flush()
		case result := <-completed:
			if result.err != nil {
				anthropicStreamError(c, result.err)
				return
			}
			for index, block := range result.response.Content {
				start := block
				delta := models.AnthropicStreamDelta{Type: "text_delta", Text: block.Text}
				if block.Type == "tool_use" {
					start.Input = map[string]interface{}{}
					input, _ := json.Marshal(block.Input)
					delta = models.AnthropicStreamDelta{Type: "input_json_delta", PartialJSON: string(input)}
				} else {
					start.Text = ""
				}
				writeAnthropicSSE(c.Writer, models.AnthropicStreamContentBlockStart{Type: "content_block_start", Index: index, ContentBlock: start})
				if block.Type == "tool_use" || block.Text != "" {
					writeAnthropicSSE(c.Writer, models.AnthropicStreamContentBlockDelta{Type: "content_block_delta", Index: index, Delta: delta})
				}
				writeAnthropicSSE(c.Writer, models.AnthropicStreamContentBlockStop{Type: "content_block_stop", Index: index})
				c.Writer.Flush()
			}
			finishAnthropicStream(c, result.response.StopReason, result.response.Usage)
			return
		}
	}
}

func (h *Handler) anthropicTextStream(c *gin.Context, client *claude.Client, req models.AnthropicRequest, model, effort, accountID string) {
	startAnthropicStream(c, model)
	writeAnthropicSSE(c.Writer, models.AnthropicStreamContentBlockStart{
		Type: "content_block_start", Index: 0, ContentBlock: models.AnthropicContentBlock{Type: "text"},
	})
	prompt := buildAnthropicPrompt(req)
	output := 0
	thinking, _ := resolveThinking(h.cfg.Thinking)
	if req.Thinking != nil {
		thinking, _ = resolveThinking(req.Thinking)
	}
	_, _, err := h.runCompletion(c.Request.Context(), client, prompt, model, effort, req.ConversationID, accountID, func(text string) {
		output += len(text)
		writeAnthropicSSE(c.Writer, models.AnthropicStreamContentBlockDelta{
			Type: "content_block_delta", Index: 0, Delta: models.AnthropicStreamDelta{Type: "text_delta", Text: text},
		})
		c.Writer.Flush()
	}, nil, thinking)
	if err != nil {
		anthropicStreamError(c, err)
		return
	}
	writeAnthropicSSE(c.Writer, models.AnthropicStreamContentBlockStop{Type: "content_block_stop", Index: 0})
	finishAnthropicStream(c, "end_turn", models.AnthropicUsage{InputTokens: len(prompt) / 4, OutputTokens: output / 4})
}

func writeAnthropicSSE(w io.Writer, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &event) != nil || event.Type == "" {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data)
}
