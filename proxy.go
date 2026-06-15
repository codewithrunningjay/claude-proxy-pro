package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

// ── Anthropic types (what Claude Code sends) ──────────────────────────────────

// AnthropicRequest represents the Anthropic /v1/messages request format.
type AnthropicRequest struct {
	Model      string          `json:"model"`
	Messages   []AnthropicMsg  `json:"messages"`
	MaxTokens  *int            `json:"max_tokens,omitempty"`
	Stream     bool            `json:"stream"`
	System     json.RawMessage `json:"system,omitempty"`
	Temp       *float64        `json:"temperature,omitempty"`
	TopP       *float64        `json:"top_p,omitempty"`
	StopSeqs   []string        `json:"stop_sequences,omitempty"`
	Tools      []AnthropicTool `json:"tools,omitempty"`
	ToolChoice *json.RawMessage `json:"tool_choice,omitempty"`
}

// AnthropicTool represents a tool in Anthropic format.
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// AnthropicMsg represents a message in the Anthropic format.
type AnthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ── OpenAI types (what we send upstream) ──────────────────────────────────────

// OpenAIRequest represents the OpenAI /chat/completions request format.
type OpenAIRequest struct {
	Model               string          `json:"model"`
	Messages            []OAIMsg        `json:"messages"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Stream              bool            `json:"stream"`
	Temp                *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                []string        `json:"stop,omitempty"`
	Tools               []OAITool       `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
}

type OAITool struct {
	Type     string      `json:"type"`
	Function OAIFunction `json:"function"`
}

type OAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// StreamOptions tells the API to include usage in streaming responses.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// OAIMsg represents a message in the OpenAI format.
type OAIMsg struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCalls  []OAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type OAIToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function OAICallFunc  `json:"function"`
}

type OAICallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ── Translation helpers ───────────────────────────────────────────────────────

// extractText extracts plain text from an Anthropic content field.
func extractText(raw json.RawMessage) string {
	// Try plain string first
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Try array of content blocks
	var blocks []map[string]interface{}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b["type"] == "text" {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return string(raw)
}

// PendingAfterTools represents assistant content that appears after tool_use in an Anthropic message.
// OpenAI chat.completions cannot place assistant text after tool_calls in the same message,
// so it is deferred until the corresponding tool results have been replayed.
type PendingAfterTools struct {
	RemainingToolIDs map[string]bool
	DeferredBlocks   []json.RawMessage
}

func getBlockType(bRaw json.RawMessage) string {
	var block map[string]interface{}
	if err := json.Unmarshal(bRaw, &block); err == nil {
		if typ, ok := block["type"].(string); ok {
			return typ
		}
	}
	return ""
}

func convertAssistantBlocks(blocks []json.RawMessage) OAIMsg {
	var textParts []string
	var toolCalls []OAIToolCall

	for _, bRaw := range blocks {
		var block map[string]interface{}
		if err := json.Unmarshal(bRaw, &block); err != nil {
			continue
		}
		typ, _ := block["type"].(string)
		switch typ {
		case "text":
			if txt, ok := block["text"].(string); ok {
				textParts = append(textParts, txt)
			}
		case "thinking":
			if th, ok := block["thinking"].(string); ok {
				textParts = append(textParts, fmt.Sprintf("<think>\n%s\n</think>", th))
			}
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			inputRaw, _ := json.Marshal(block["input"])
			toolCalls = append(toolCalls, OAIToolCall{
				ID:   id,
				Type: "function",
				Function: OAICallFunc{
					Name:      name,
					Arguments: string(inputRaw),
				},
			})
		}
	}
	contentStr := strings.Join(textParts, "\n\n")
	if contentStr == "" && len(toolCalls) == 0 {
		contentStr = " "
	}
	return OAIMsg{
		Role:      "assistant",
		Content:   contentStr,
		ToolCalls: toolCalls,
	}
}

func splitAssistantBlocks(blocks []json.RawMessage) (preBlocks []json.RawMessage, toolCalls []OAIToolCall, deferredBlocks []json.RawMessage) {
	firstToolIndex := -1
	for i, bRaw := range blocks {
		if getBlockType(bRaw) == "tool_use" {
			firstToolIndex = i
			break
		}
	}

	if firstToolIndex == -1 {
		return blocks, nil, nil
	}

	preBlocks = blocks[:firstToolIndex]

	for i, bRaw := range blocks {
		var block map[string]interface{}
		if err := json.Unmarshal(bRaw, &block); err != nil {
			continue
		}
		typ, _ := block["type"].(string)
		if typ == "tool_use" {
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			inputRaw, _ := json.Marshal(block["input"])
			toolCalls = append(toolCalls, OAIToolCall{
				ID:   id,
				Type: "function",
				Function: OAICallFunc{
					Name:      name,
					Arguments: string(inputRaw),
				},
			})
		} else if i > firstToolIndex {
			deferredBlocks = append(deferredBlocks, bRaw)
		}
	}

	return preBlocks, toolCalls, deferredBlocks
}

// translateMessages translates Anthropic messages to OpenAI format, mapping text blocks, tool uses, and tool results, and deferring assistant text trailing after tool calls.
func translateMessages(amsgs []AnthropicMsg) []OAIMsg {
	var omsgs []OAIMsg
	var pending *PendingAfterTools

	flushPending := func() {
		if pending != nil && len(pending.DeferredBlocks) > 0 {
			msg := convertAssistantBlocks(pending.DeferredBlocks)
			omsgs = append(omsgs, msg)
		}
		pending = nil
	}

	for _, am := range amsgs {
		role := am.Role

		if role == "assistant" {
			if pending != nil {
				flushPending()
			}

			// Try unmarshaling as an array of content blocks
			var blocks []json.RawMessage
			if json.Unmarshal(am.Content, &blocks) == nil {
				pre, toolCalls, deferred := splitAssistantBlocks(blocks)
				preMsg := convertAssistantBlocks(pre)
				preMsg.ToolCalls = toolCalls
				omsgs = append(omsgs, preMsg)

				if len(deferred) > 0 {
					remaining := make(map[string]bool)
					for _, tc := range toolCalls {
						if tc.ID != "" {
							remaining[tc.ID] = true
						}
					}
					pending = &PendingAfterTools{
						RemainingToolIDs: remaining,
						DeferredBlocks:   deferred,
					}
				}
				continue
			}

			// Fallback: Content is plain string
			var s string
			if json.Unmarshal(am.Content, &s) == nil {
				omsgs = append(omsgs, OAIMsg{Role: role, Content: s})
			} else {
				omsgs = append(omsgs, OAIMsg{Role: role, Content: string(am.Content)})
			}
			continue
		}

		if role == "user" {
			var blocks []json.RawMessage
			if json.Unmarshal(am.Content, &blocks) == nil {
				var textParts []string
				flushText := func() {
					if len(textParts) > 0 {
						omsgs = append(omsgs, OAIMsg{
							Role:    "user",
							Content: strings.Join(textParts, "\n"),
						})
						textParts = nil
					}
				}

				for _, bRaw := range blocks {
					var block map[string]interface{}
					if err := json.Unmarshal(bRaw, &block); err != nil {
						continue
					}
					typ, _ := block["type"].(string)
					switch typ {
					case "text":
						if txt, ok := block["text"].(string); ok {
							textParts = append(textParts, txt)
						}
					case "tool_result":
						flushText()

						toolUseID, _ := block["tool_use_id"].(string)
						contentRaw := block["content"]

						var resultStr string
						if s, ok := contentRaw.(string); ok {
							resultStr = s
						} else {
							bytes, _ := json.Marshal(contentRaw)
							resultStr = string(bytes)
						}

						omsgs = append(omsgs, OAIMsg{
							Role:       "tool",
							ToolCallID: toolUseID,
							Content:    resultStr,
						})

						if pending != nil {
							delete(pending.RemainingToolIDs, toolUseID)
							if len(pending.RemainingToolIDs) == 0 {
								flushPending()
							}
						}
					}
				}
				flushText()
				continue
			}

			if pending != nil {
				flushPending()
			}

			var s string
			if json.Unmarshal(am.Content, &s) == nil {
				omsgs = append(omsgs, OAIMsg{Role: role, Content: s})
			} else {
				omsgs = append(omsgs, OAIMsg{Role: role, Content: string(am.Content)})
			}
			continue
		}

		if pending != nil {
			flushPending()
		}
		var s string
		if json.Unmarshal(am.Content, &s) == nil {
			omsgs = append(omsgs, OAIMsg{Role: role, Content: s})
		} else {
			omsgs = append(omsgs, OAIMsg{Role: role, Content: string(am.Content)})
		}
	}

	flushPending()
	return omsgs
}

// ── Thinking Tag Parser ──────────────────────────────────────────────────────

type ContentType string

const (
	ContentTypeText     ContentType = "text"
	ContentTypeThinking ContentType = "thinking"
)

type ContentChunk struct {
	Type    ContentType
	Content string
}

type ThinkTagParser struct {
	buffer     string
	inThinkTag bool
	openTag    string
	closeTag   string
}

func NewThinkTagParser() *ThinkTagParser {
	return &ThinkTagParser{
		openTag:  "<think>",
		closeTag: "</think>",
	}
}

func (p *ThinkTagParser) InThinkMode() bool {
	return p.inThinkTag
}

func (p *ThinkTagParser) Feed(content string) []ContentChunk {
	p.buffer += content
	var chunks []ContentChunk

	for len(p.buffer) > 0 {
		prevLen := len(p.buffer)
		var chunk *ContentChunk
		if !p.inThinkTag {
			chunk = p.parseOutsideThink()
		} else {
			chunk = p.parseInsideThink()
		}

		if chunk != nil {
			chunks = append(chunks, *chunk)
		} else if len(p.buffer) == prevLen {
			break
		}
	}
	return chunks
}

func (p *ThinkTagParser) parseOutsideThink() *ContentChunk {
	thinkStart := strings.Index(p.buffer, p.openTag)
	orphanClose := strings.Index(p.buffer, p.closeTag)

	if orphanClose != -1 && (thinkStart == -1 || orphanClose < thinkStart) {
		preOrphan := p.buffer[:orphanClose]
		p.buffer = p.buffer[orphanClose+len(p.closeTag):]
		if preOrphan != "" {
			return &ContentChunk{Type: ContentTypeText, Content: preOrphan}
		}
		return nil
	}

	if thinkStart == -1 {
		lastBracket := strings.LastIndex(p.buffer, "<")
		if lastBracket != -1 {
			potentialTag := p.buffer[lastBracket:]
			tagLen := len(potentialTag)
			if (tagLen < len(p.openTag) && strings.HasPrefix(p.openTag, potentialTag)) ||
				(tagLen < len(p.closeTag) && strings.HasPrefix(p.closeTag, potentialTag)) {
				emit := p.buffer[:lastBracket]
				p.buffer = p.buffer[lastBracket:]
				if emit != "" {
					return &ContentChunk{Type: ContentTypeText, Content: emit}
				}
				return nil
			}
		}

		emit := p.buffer
		p.buffer = ""
		if emit != "" {
			return &ContentChunk{Type: ContentTypeText, Content: emit}
		}
		return nil
	}

	preThink := p.buffer[:thinkStart]
	p.buffer = p.buffer[thinkStart+len(p.openTag):]
	p.inThinkTag = true
	if preThink != "" {
		return &ContentChunk{Type: ContentTypeText, Content: preThink}
	}
	return nil
}

func (p *ThinkTagParser) parseInsideThink() *ContentChunk {
	thinkEnd := strings.Index(p.buffer, p.closeTag)

	if thinkEnd == -1 {
		lastBracket := strings.LastIndex(p.buffer, "<")
		if lastBracket != -1 && len(p.buffer)-lastBracket < len(p.closeTag) {
			potentialTag := p.buffer[lastBracket:]
			if strings.HasPrefix(p.closeTag, potentialTag) {
				emit := p.buffer[:lastBracket]
				p.buffer = p.buffer[lastBracket:]
				if emit != "" {
					return &ContentChunk{Type: ContentTypeThinking, Content: emit}
				}
				return nil
			}
		}

		emit := p.buffer
		p.buffer = ""
		if emit != "" {
			return &ContentChunk{Type: ContentTypeThinking, Content: emit}
		}
		return nil
	}

	thinkingContent := p.buffer[:thinkEnd]
	p.buffer = p.buffer[thinkEnd+len(p.closeTag):]
	p.inThinkTag = false
	if thinkingContent != "" {
		return &ContentChunk{Type: ContentTypeThinking, Content: thinkingContent}
	}
	return nil
}

func (p *ThinkTagParser) Flush() *ContentChunk {
	if p.buffer != "" {
		chunkType := ContentTypeText
		if p.inThinkTag {
			chunkType = ContentTypeThinking
		}
		content := p.buffer
		p.buffer = ""
		return &ContentChunk{Type: chunkType, Content: content}
	}
	return nil
}

// ── Heuristic Tool Parser ────────────────────────────────────────────────────

type HeuristicState int

const (
	HeuristicStateText HeuristicState = iota
	HeuristicStateMatchingFunction
	HeuristicStateParsingParameters
)

type HeuristicToolParser struct {
	state               HeuristicState
	buffer              string
	currentToolID       string
	currentFunctionName string
	currentParameters   map[string]string

	funcStartPattern  *regexp.Regexp
	paramPattern      *regexp.Regexp
	webToolJsonPattern *regexp.Regexp
}

func NewHeuristicToolParser() *HeuristicToolParser {
	return &HeuristicToolParser{
		state:              HeuristicStateText,
		currentParameters:  make(map[string]string),
		funcStartPattern:   regexp.MustCompile(`●\s*<function=([^>]+)>`),
		paramPattern:       regexp.MustCompile(`(?s)<parameter=([^>]+)>(.*?)(?:</parameter>|$)`),
		webToolJsonPattern: regexp.MustCompile(`(?is)\b(?:use\s+)?(WebFetch|WebSearch)\b.*?(\{.*?\})`),
	}
}

func newUUIDHex8() string {
	return fmt.Sprintf("%08x", time.Now().UnixNano()&0xFFFFFFFF)
}

var controlTokenRe = regexp.MustCompile(`<\|[^|>]{1,80}\|>`)

func stripControlTokens(text string) string {
	return controlTokenRe.ReplaceAllString(text, "")
}

func (p *HeuristicToolParser) extractWebToolJsonCalls() []map[string]interface{} {
	var detected []map[string]interface{}

	loc := p.webToolJsonPattern.FindStringSubmatchIndex(p.buffer)
	if loc != nil {
		toolName := p.buffer[loc[2]:loc[3]]
		jsonStr := p.buffer[loc[4]:loc[5]]

		var toolInput map[string]interface{}
		if err := json.Unmarshal([]byte(jsonStr), &toolInput); err == nil {
			valid := true
			if strings.EqualFold(toolName, "WebFetch") {
				if _, ok := toolInput["url"]; !ok {
					valid = false
				}
			} else if strings.EqualFold(toolName, "WebSearch") {
				if _, ok := toolInput["query"]; !ok {
					valid = false
				}
			} else {
				valid = false
			}

			if valid {
				matchedName := "WebSearch"
				if strings.EqualFold(toolName, "WebFetch") {
					matchedName = "WebFetch"
				}

				detected = append(detected, map[string]interface{}{
					"type":  "tool_use",
					"id":    fmt.Sprintf("toolu_heuristic_%s", newUUIDHex8()),
					"name":  matchedName,
					"input": toolInput,
				})
			}
		}
	}

	if len(detected) > 0 {
		p.buffer = ""
	}
	return detected
}

func (p *HeuristicToolParser) Feed(text string) (string, []map[string]interface{}) {
	p.buffer += text
	p.buffer = stripControlTokens(p.buffer)

	detectedTools := p.extractWebToolJsonCalls()
	if len(detectedTools) > 0 {
		return "", detectedTools
	}

	var filteredOutputParts []string

	for {
		if p.state == HeuristicStateText {
			if idx := strings.Index(p.buffer, "●"); idx != -1 {
				filteredOutputParts = append(filteredOutputParts, p.buffer[:idx])
				p.buffer = p.buffer[idx:]
				p.state = HeuristicStateMatchingFunction
			} else {
				start := strings.LastIndex(p.buffer, "<|")
				if start != -1 {
					end := strings.Index(p.buffer[start:], "|>")
					if end == -1 {
						prefix := p.buffer[:start]
						p.buffer = p.buffer[start:]
						if prefix != "" {
							filteredOutputParts = append(filteredOutputParts, prefix)
						}
						break
					}
				}

				filteredOutputParts = append(filteredOutputParts, p.buffer)
				p.buffer = ""
				break
			}
		}

		if p.state == HeuristicStateMatchingFunction {
			match := p.funcStartPattern.FindStringSubmatchIndex(p.buffer)
			if match != nil {
				p.currentFunctionName = strings.TrimSpace(p.buffer[match[2]:match[3]])
				p.currentToolID = fmt.Sprintf("toolu_heuristic_%s", newUUIDHex8())
				p.currentParameters = make(map[string]string)
				p.buffer = p.buffer[match[1]:]
				p.state = HeuristicStateParsingParameters
			} else {
				// Check if it can never match
				impossible := false
				if !strings.HasPrefix(p.buffer, "●") {
					impossible = true
				} else {
					trimmed := strings.TrimLeft(p.buffer[len("●"):], " \t\r\n")
					if len(trimmed) > 0 {
						if !strings.HasPrefix(trimmed, "<") {
							impossible = true
						} else if len(trimmed) >= len("<function=") && !strings.HasPrefix(trimmed, "<function=") {
							impossible = true
						}
					}
				}

				if impossible {
					r, size := utf8.DecodeRuneInString(p.buffer)
					filteredOutputParts = append(filteredOutputParts, string(r))
					p.buffer = p.buffer[size:]
					p.state = HeuristicStateText
				} else if len(p.buffer) > 100 {
					r, size := utf8.DecodeRuneInString(p.buffer)
					filteredOutputParts = append(filteredOutputParts, string(r))
					p.buffer = p.buffer[size:]
					p.state = HeuristicStateText
				} else {
					break
				}
			}
		}

		if p.state == HeuristicStateParsingParameters {
			finishedToolCall := false

			for {
				match := p.paramPattern.FindStringSubmatchIndex(p.buffer)
				if match != nil && strings.Contains(p.buffer[match[0]:match[1]], "</parameter>") {
					preMatchText := p.buffer[:match[0]]
					if preMatchText != "" {
						filteredOutputParts = append(filteredOutputParts, preMatchText)
					}

					key := strings.TrimSpace(p.buffer[match[2]:match[3]])
					val := strings.TrimSpace(p.buffer[match[4]:match[5]])
					p.currentParameters[key] = val
					p.buffer = p.buffer[match[1]:]
				} else {
					break
				}
			}

			if strings.Contains(p.buffer, "●") {
				idx := strings.Index(p.buffer, "●")
				if idx > 0 {
					filteredOutputParts = append(filteredOutputParts, p.buffer[:idx])
					p.buffer = p.buffer[idx:]
				}
				finishedToolCall = true
			} else if len(p.buffer) > 0 && !strings.HasPrefix(strings.TrimSpace(p.buffer), "<") {
				if !strings.Contains(p.buffer, "<parameter=") {
					filteredOutputParts = append(filteredOutputParts, p.buffer)
					p.buffer = ""
					finishedToolCall = true
				}
			}

			if finishedToolCall {
				inputMap := make(map[string]interface{})
				for k, v := range p.currentParameters {
					var valObj interface{}
					if err := json.Unmarshal([]byte(v), &valObj); err == nil {
						inputMap[k] = valObj
					} else {
						inputMap[k] = v
					}
				}

				detectedTools = append(detectedTools, map[string]interface{}{
					"type":  "tool_use",
					"id":    p.currentToolID,
					"name":  p.currentFunctionName,
					"input": inputMap,
				})

				p.state = HeuristicStateText
			} else {
				break
			}
		}
	}

	return strings.Join(filteredOutputParts, ""), detectedTools
}

func (p *HeuristicToolParser) Flush() []map[string]interface{} {
	p.buffer = stripControlTokens(p.buffer)
	var detectedTools []map[string]interface{}

	if p.state == HeuristicStateParsingParameters {
		partialPattern := regexp.MustCompile(`(?s)<parameter=([^>]+)>(.*)$`)
		match := partialPattern.FindStringSubmatchIndex(p.buffer)
		if match != nil {
			key := strings.TrimSpace(p.buffer[match[2]:match[3]])
			val := strings.TrimSpace(p.buffer[match[4]:match[5]])
			p.currentParameters[key] = val
		}

		inputMap := make(map[string]interface{})
		for k, v := range p.currentParameters {
			var valObj interface{}
			if err := json.Unmarshal([]byte(v), &valObj); err == nil {
				inputMap[k] = valObj
			} else {
				inputMap[k] = v
			}
		}

		detectedTools = append(detectedTools, map[string]interface{}{
			"type":  "tool_use",
			"id":    p.currentToolID,
			"name":  p.currentFunctionName,
			"input": inputMap,
		})

		p.state = HeuristicStateText
		p.buffer = ""
	}

	return detectedTools
}

// ── Stream Builder ───────────────────────────────────────────────────────────

type StreamBuilder struct {
	w              http.ResponseWriter
	msgID          string
	requestedModel string
	currentIndex   int
	activeType     string
}

func NewStreamBuilder(w http.ResponseWriter, msgID string, requestedModel string) *StreamBuilder {
	return &StreamBuilder{
		w:              w,
		msgID:          msgID,
		requestedModel: requestedModel,
		currentIndex:   -1,
	}
}

func (sb *StreamBuilder) StartMessage() {
	startData, _ := json.Marshal(map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":      sb.msgID,
			"type":    "message",
			"role":    "assistant",
			"model":   sb.requestedModel,
			"content": []interface{}{},
			"usage":   map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
	sse(sb.w, "message_start", string(startData))
}

func (sb *StreamBuilder) StopMessage(stopReason string) {
	sb.CloseActiveBlocks()

	msgDelta, _ := json.Marshal(map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason},
		"usage": map[string]interface{}{},
	})
	sse(sb.w, "message_delta", string(msgDelta))

	msgStop, _ := json.Marshal(map[string]string{"type": "message_stop"})
	sse(sb.w, "message_stop", string(msgStop))
}

func (sb *StreamBuilder) EnsureThinkingBlock() {
	if sb.activeType == "thinking" {
		return
	}
	sb.CloseActiveBlocks()

	sb.currentIndex++
	sb.activeType = "thinking"

	blockStart, _ := json.Marshal(map[string]interface{}{
		"type":          "content_block_start",
		"index":         sb.currentIndex,
		"content_block": map[string]string{"type": "thinking", "thinking": ""},
	})
	sse(sb.w, "content_block_start", string(blockStart))
}

func (sb *StreamBuilder) EnsureTextBlock() {
	if sb.activeType == "text" {
		return
	}
	sb.CloseActiveBlocks()

	sb.currentIndex++
	sb.activeType = "text"

	blockStart, _ := json.Marshal(map[string]interface{}{
		"type":          "content_block_start",
		"index":         sb.currentIndex,
		"content_block": map[string]string{"type": "text", "text": ""},
	})
	sse(sb.w, "content_block_start", string(blockStart))
}

func (sb *StreamBuilder) EmitThinkingDelta(thinking string) {
	sb.EnsureThinkingBlock()
	deltaData, _ := json.Marshal(map[string]interface{}{
		"type":  "content_block_delta",
		"index": sb.currentIndex,
		"delta": map[string]string{"type": "thinking_delta", "thinking": thinking},
	})
	sse(sb.w, "content_block_delta", string(deltaData))
}

func (sb *StreamBuilder) EmitTextDelta(text string) {
	sb.EnsureTextBlock()
	deltaData, _ := json.Marshal(map[string]interface{}{
		"type":  "content_block_delta",
		"index": sb.currentIndex,
		"delta": map[string]string{"type": "text_delta", "text": text},
	})
	sse(sb.w, "content_block_delta", string(deltaData))
}

func (sb *StreamBuilder) EmitToolUse(id string, name string, input map[string]interface{}) {
	sb.CloseActiveBlocks()

	sb.currentIndex++
	blockIdx := sb.currentIndex

	blockStart, _ := json.Marshal(map[string]interface{}{
		"type":          "content_block_start",
		"index":         blockIdx,
		"content_block": map[string]interface{}{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": map[string]interface{}{},
		},
	})
	sse(sb.w, "content_block_start", string(blockStart))

	argsBytes, _ := json.Marshal(input)
	deltaData, _ := json.Marshal(map[string]interface{}{
		"type":  "content_block_delta",
		"index": blockIdx,
		"delta": map[string]string{
			"type":         "input_json_delta",
			"partial_json": string(argsBytes),
		},
	})
	sse(sb.w, "content_block_delta", string(deltaData))

	blockStop, _ := json.Marshal(map[string]interface{}{
		"type":  "content_block_stop",
		"index": blockIdx,
	})
	sse(sb.w, "content_block_stop", string(blockStop))
}

func (sb *StreamBuilder) CloseActiveBlocks() {
	if sb.activeType != "" {
		blockStop, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_stop",
			"index": sb.currentIndex,
		})
		sse(sb.w, "content_block_stop", string(blockStop))
		sb.activeType = ""
	}
}


// toOpenAI translates an Anthropic request to an OpenAI request.
func toOpenAI(req AnthropicRequest, model string) OpenAIRequest {
	var msgs []OAIMsg

	// System prompt
	var systemContent string
	if len(req.System) > 0 && string(req.System) != "null" {
		systemContent = extractText(req.System)
	}
	systemContent += "\n\n[SYSTEM NOTE: You MUST call tools natively using the tool_calls parameter. Do NOT print raw JSON tool calls in your text.]"
	msgs = append(msgs, OAIMsg{Role: "system", Content: systemContent})

	// Translate messages
	msgs = append(msgs, translateMessages(req.Messages)...)

	// Translate tools
	var oaiTools []OAITool
	for _, t := range req.Tools {
		oaiTools = append(oaiTools, OAITool{
			Type: "function",
			Function: OAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	oaiReq := OpenAIRequest{
		Model:    model,
		Messages: msgs,
		Stream:   req.Stream,
		Temp:     req.Temp,
		TopP:     req.TopP,
		Stop:     req.StopSeqs,
		Tools:    oaiTools,
	}

	if len(req.Tools) > 0 {
		// If tools are present, force non-streaming upstream
		oaiReq.Stream = false
	}

	// Map max_tokens
	if req.MaxTokens != nil {
		oaiReq.MaxTokens = req.MaxTokens
		oaiReq.MaxCompletionTokens = req.MaxTokens
	}

	// Enable usage info in streaming
	if req.Stream && len(req.Tools) == 0 {
		oaiReq.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	return oaiReq
}

// ── Proxy handler ─────────────────────────────────────────────────────────────

// Proxy holds all dependencies for the proxy handler.
type Proxy struct {
	cfg       *ConfigStore
	stability *StabilityManager
	stats     *StatsManager
	pm        *ProviderManager
	http      *http.Client
}

// NewProxy creates a new proxy handler.
func NewProxy(cfg *ConfigStore, stability *StabilityManager) *Proxy {
	return &Proxy{
		cfg:       cfg,
		stability: stability,
		http: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// HandleMessages is the main proxy handler for POST /v1/messages.
func (px *Proxy) HandleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Decode Anthropic request
	var req AnthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("bad request: %v", err), http.StatusBadRequest)
		return
	}

	// Get active provider
	provider, ok := px.cfg.GetActiveProvider()
	if !ok {
		writeAnthropicError(w, "no providers configured", http.StatusServiceUnavailable)
		return
	}

	// Translate to OpenAI format
	oaiReq := toOpenAI(req, provider.Model)

	// Log incoming and translated requests
	reqBytes, _ := json.MarshalIndent(req, "", "  ")
	oaiReqBytes, _ := json.MarshalIndent(oaiReq, "", "  ")
	logTraffic("=== REQUEST START ===")
	logTraffic("Anthropic Request:\n%s", string(reqBytes))
	logTraffic("Translated OpenAI Request:\n%s", string(oaiReqBytes))

	// Execute with stability (retry + failover)
	resp, finalIdx, err := px.stability.RetryWithBackoff(func(idx int) (*http.Response, error) {
		p, ok := px.cfg.GetProviderByIndex(idx)
		if !ok {
			return nil, fmt.Errorf("provider %d not found", idx)
		}

		// Override model if provider has a specific model
		oaiReq.Model = p.Model

		body, err := json.Marshal(oaiReq)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}

		url := strings.TrimRight(p.URL, "/") + "/chat/completions"
		upReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("Authorization", "Bearer "+p.Key)

		return px.http.Do(upReq)
	})

	if err != nil {
		writeAnthropicError(w, fmt.Sprintf("upstream error: %v", err), http.StatusBadGateway)
		px.cfg.IncrementRequests(finalIdx, false)
		return
	}
	defer resp.Body.Close()

	px.cfg.IncrementRequests(finalIdx, resp.StatusCode < 400)

	// Set CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Check if upstream returned an error
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		writeAnthropicError(w, fmt.Sprintf("upstream error %d: %s", resp.StatusCode, string(body)), resp.StatusCode)
		return
	}

	// Forward response — use oaiReq.Model (actual provider model) so Claude Code
	// learns the real model being used and displays it correctly.
	actualModel := oaiReq.Model
	if req.Stream {
		if len(req.Tools) > 0 {
			px.handleStreamedTools(w, resp, actualModel)
		} else {
			px.handleStream(w, resp, actualModel)
		}
	} else {
		px.handleBlock(w, resp, actualModel)
	}
}

// handleBlock handles a non-streaming (blocking) response.
func (px *Proxy) handleBlock(w http.ResponseWriter, resp *http.Response, requestedModel string) {
	var oaiResp map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&oaiResp); err != nil {
		writeAnthropicError(w, fmt.Sprintf("decode upstream response: %v", err), http.StatusBadGateway)
		return
	}

	oaiRespBytes, _ := json.MarshalIndent(oaiResp, "", "  ")
	logTraffic("Upstream OpenAI Response (Blocking):\n%s", string(oaiRespBytes))

	var contentParts []map[string]interface{}

	if choices, ok := oaiResp["choices"].([]interface{}); ok && len(choices) > 0 {
		if first, ok := choices[0].(map[string]interface{}); ok {
			if msg, ok := first["message"].(map[string]interface{}); ok {
				// Handle reasoning_content (extended OpenAI format) if present
				if reasoning, ok := msg["reasoning_content"].(string); ok && reasoning != "" {
					contentParts = append(contentParts, map[string]interface{}{
						"type":     "thinking",
						"thinking": reasoning,
					})
				}

				// Text content
				if c, ok := msg["content"].(string); ok && c != "" {
					thinkParser := NewThinkTagParser()
					heuristicParser := NewHeuristicToolParser()

					var chunks []ContentChunk
					chunks = append(chunks, thinkParser.Feed(c)...)
					if remaining := thinkParser.Flush(); remaining != nil {
						chunks = append(chunks, *remaining)
					}

					for _, chunk := range chunks {
						if chunk.Type == ContentTypeThinking {
							contentParts = append(contentParts, map[string]interface{}{
								"type":     "thinking",
								"thinking": chunk.Content,
							})
						} else {
							filteredText, detectedTools := heuristicParser.Feed(chunk.Content)
							if filteredText != "" {
								contentParts = append(contentParts, map[string]interface{}{
									"type": "text",
									"text": filteredText,
								})
							}
							for _, toolUse := range detectedTools {
								contentParts = append(contentParts, toolUse)
							}
						}
					}

					for _, toolUse := range heuristicParser.Flush() {
						contentParts = append(contentParts, toolUse)
					}
				}
				// Tool calls
				if tcs, ok := msg["tool_calls"].([]interface{}); ok {
					for _, tcRaw := range tcs {
						if tc, ok := tcRaw.(map[string]interface{}); ok {
							id, _ := tc["id"].(string)
							if fn, ok := tc["function"].(map[string]interface{}); ok {
								name, _ := fn["name"].(string)
								argsStr, _ := fn["arguments"].(string)
								var args map[string]interface{}
								json.Unmarshal([]byte(argsStr), &args)

								contentParts = append(contentParts, map[string]interface{}{
									"type":  "tool_use",
									"id":    id,
									"name":  name,
									"input": args,
								})
							}
						}
					}
				}
			}
		}
	}

	if contentParts == nil {
		contentParts = []map[string]interface{}{}
	}

	// Extract usage
	inputTok := 0
	outputTok := 0
	if usage, ok := oaiResp["usage"].(map[string]interface{}); ok {
		if v, ok := usage["prompt_tokens"].(float64); ok {
			inputTok = int(v)
		}
		if v, ok := usage["completion_tokens"].(float64); ok {
			outputTok = int(v)
		}
	}

	// Determine stop reason
	stopReason := "end_turn"
	for _, part := range contentParts {
		if part["type"] == "tool_use" {
			stopReason = "tool_use"
			break
		}
	}

	if stopReason == "end_turn" {
		if choices, ok := oaiResp["choices"].([]interface{}); ok && len(choices) > 0 {
			if first, ok := choices[0].(map[string]interface{}); ok {
				if reason, ok := first["finish_reason"].(string); ok {
					switch reason {
					case "length":
						stopReason = "max_tokens"
					case "stop":
						stopReason = "end_turn"
					case "content_filter":
						stopReason = "stop_sequence"
					}
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":            newMsgID(),
		"type":          "message",
		"role":          "assistant",
		"content":       contentParts,
		"model":         requestedModel,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]int{
			"input_tokens":  inputTok,
			"output_tokens": outputTok,
		},
	})
}

// handleStreamedTools emulates streaming tool calls back to the client using SSE events based on a blocking upstream response.
func (px *Proxy) handleStreamedTools(w http.ResponseWriter, resp *http.Response, requestedModel string) {
	var oaiResp map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&oaiResp); err != nil {
		writeAnthropicError(w, fmt.Sprintf("decode upstream response: %v", err), http.StatusBadGateway)
		return
	}

	oaiRespBytes, _ := json.MarshalIndent(oaiResp, "", "  ")
	logTraffic("Upstream OpenAI Response (StreamedTools):\n%s", string(oaiRespBytes))

	var text string
	var reasoning string
	var toolCalls []map[string]interface{}

	if choices, ok := oaiResp["choices"].([]interface{}); ok && len(choices) > 0 {
		if first, ok := choices[0].(map[string]interface{}); ok {
			if msg, ok := first["message"].(map[string]interface{}); ok {
				if c, ok := msg["content"].(string); ok {
					text = c
				}
				if r, ok := msg["reasoning_content"].(string); ok {
					reasoning = r
				}
				if tcs, ok := msg["tool_calls"].([]interface{}); ok {
					for _, tcRaw := range tcs {
						if tc, ok := tcRaw.(map[string]interface{}); ok {
							id, _ := tc["id"].(string)
							if fn, ok := tc["function"].(map[string]interface{}); ok {
								name, _ := fn["name"].(string)
								argsStr, _ := fn["arguments"].(string)
								var args map[string]interface{}
								json.Unmarshal([]byte(argsStr), &args)

								toolCalls = append(toolCalls, map[string]interface{}{
									"id":    id,
									"name":  name,
									"input": args,
								})
							}
						}
					}
				}
			}
		}
	}

	var blocks []map[string]interface{}

	if reasoning != "" {
		blocks = append(blocks, map[string]interface{}{
			"type":     "thinking",
			"thinking": reasoning,
		})
	}

	if text != "" {
		thinkParser := NewThinkTagParser()
		heuristicParser := NewHeuristicToolParser()

		var chunks []ContentChunk
		chunks = append(chunks, thinkParser.Feed(text)...)
		if remaining := thinkParser.Flush(); remaining != nil {
			chunks = append(chunks, *remaining)
		}

		for _, chunk := range chunks {
			if chunk.Type == ContentTypeThinking {
				blocks = append(blocks, map[string]interface{}{
					"type":     "thinking",
					"thinking": chunk.Content,
				})
			} else {
				filteredText, detectedTools := heuristicParser.Feed(chunk.Content)
				if filteredText != "" {
					blocks = append(blocks, map[string]interface{}{
						"type": "text",
						"text": filteredText,
					})
				}
				for _, toolUse := range detectedTools {
					blocks = append(blocks, toolUse)
				}
			}
		}

		for _, toolUse := range heuristicParser.Flush() {
			blocks = append(blocks, toolUse)
		}
	}

	for _, tc := range toolCalls {
		blocks = append(blocks, map[string]interface{}{
			"type":  "tool_use",
			"id":    tc["id"],
			"name":  tc["name"],
			"input": tc["input"],
		})
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	msgID := newMsgID()

	// message_start
	startData, _ := json.Marshal(map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":      msgID,
			"type":    "message",
			"role":    "assistant",
			"model":   requestedModel,
			"content": []interface{}{},
			"usage":   map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
	sse(w, "message_start", string(startData))

	stopReason := "end_turn"
	for idx, b := range blocks {
		typ, _ := b["type"].(string)
		switch typ {
		case "thinking":
			blockStart, _ := json.Marshal(map[string]interface{}{
				"type":          "content_block_start",
				"index":         idx,
				"content_block": map[string]string{"type": "thinking", "thinking": ""},
			})
			sse(w, "content_block_start", string(blockStart))

			val, _ := b["thinking"].(string)
			deltaData, _ := json.Marshal(map[string]interface{}{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]string{"type": "thinking_delta", "thinking": val},
			})
			sse(w, "content_block_delta", string(deltaData))

			blockStop, _ := json.Marshal(map[string]interface{}{
				"type":  "content_block_stop",
				"index": idx,
			})
			sse(w, "content_block_stop", string(blockStop))

		case "text":
			blockStart, _ := json.Marshal(map[string]interface{}{
				"type":          "content_block_start",
				"index":         idx,
				"content_block": map[string]string{"type": "text", "text": ""},
			})
			sse(w, "content_block_start", string(blockStart))

			val, _ := b["text"].(string)
			deltaData, _ := json.Marshal(map[string]interface{}{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]string{"type": "text_delta", "text": val},
			})
			sse(w, "content_block_delta", string(deltaData))

			blockStop, _ := json.Marshal(map[string]interface{}{
				"type":  "content_block_stop",
				"index": idx,
			})
			sse(w, "content_block_stop", string(blockStop))

		case "tool_use":
			stopReason = "tool_use"
			blockStart, _ := json.Marshal(map[string]interface{}{
				"type":          "content_block_start",
				"index":         idx,
				"content_block": map[string]interface{}{
					"type":  "tool_use",
					"id":    b["id"],
					"name":  b["name"],
					"input": map[string]interface{}{},
				},
			})
			sse(w, "content_block_start", string(blockStart))

			argsBytes, _ := json.Marshal(b["input"])
			deltaData, _ := json.Marshal(map[string]interface{}{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]string{
					"type":         "input_json_delta",
					"partial_json": string(argsBytes),
				},
			})
			sse(w, "content_block_delta", string(deltaData))

			blockStop, _ := json.Marshal(map[string]interface{}{
				"type":  "content_block_stop",
				"index": idx,
			})
			sse(w, "content_block_stop", string(blockStop))
		}
	}

	// message_delta
	msgDelta, _ := json.Marshal(map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason},
		"usage": map[string]interface{}{},
	})
	sse(w, "message_delta", string(msgDelta))

	// message_stop
	msgStop, _ := json.Marshal(map[string]string{"type": "message_stop"})
	sse(w, "message_stop", string(msgStop))
}

// handleStream handles a streaming response.
func (px *Proxy) handleStream(w http.ResponseWriter, resp *http.Response, requestedModel string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	msgID := newMsgID()

	thinkParser := NewThinkTagParser()
	heuristicParser := NewHeuristicToolParser()
	sb := NewStreamBuilder(w, msgID, requestedModel)
	sb.StartMessage()

	// Stream chunks from upstream
	scanner := bufio.NewScanner(resp.Body)
	// Increase buffer size for large chunks
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var stopReason = "end_turn"

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		// Check for usage info in streaming (some providers send it)
		if usage, ok := chunk["usage"].(map[string]interface{}); ok {
			if pt, ok := usage["prompt_tokens"].(float64); ok || pt != 0 {
				// Send a message_delta with usage info
				usageData, _ := json.Marshal(map[string]interface{}{
					"type": "message_delta",
					"delta": map[string]interface{}{},
					"usage": map[string]interface{}{
						"input_tokens":  usage["prompt_tokens"],
						"output_tokens": usage["completion_tokens"],
					},
				})
				sse(w, "message_delta", string(usageData))
			}
			continue
		}

		choices, _ := chunk["choices"].([]interface{})
		if len(choices) == 0 {
			continue
		}

		first, ok := choices[0].(map[string]interface{})
		if !ok {
			continue
		}

		// Check finish_reason
		if finishReason, ok := first["finish_reason"].(string); ok && finishReason != "" {
			if finishReason == "length" {
				stopReason = "max_tokens"
			}
			continue
		}

		delta, _ := first["delta"].(map[string]interface{})

		// Handle reasoning_content if present (e.g. deepseek reasoning stream)
		if reasoning, ok := delta["reasoning_content"].(string); ok && reasoning != "" {
			sb.EmitThinkingDelta(reasoning)
		}

		text, _ := delta["content"].(string)
		if text == "" {
			continue
		}

		chunks := thinkParser.Feed(text)
		for _, chunk := range chunks {
			if chunk.Type == ContentTypeThinking {
				sb.EmitThinkingDelta(chunk.Content)
			} else {
				filteredText, detectedTools := heuristicParser.Feed(chunk.Content)
				if filteredText != "" {
					sb.EmitTextDelta(filteredText)
				}
				for _, toolUse := range detectedTools {
					stopReason = "tool_use"
					id, _ := toolUse["id"].(string)
					name, _ := toolUse["name"].(string)
					input, _ := toolUse["input"].(map[string]interface{})
					sb.EmitToolUse(id, name, input)
				}
			}
		}
	}

	// Flush think parser
	if remaining := thinkParser.Flush(); remaining != nil {
		if remaining.Type == ContentTypeThinking {
			sb.EmitThinkingDelta(remaining.Content)
		} else {
			filteredText, detectedTools := heuristicParser.Feed(remaining.Content)
			if filteredText != "" {
				sb.EmitTextDelta(filteredText)
			}
			for _, toolUse := range detectedTools {
				stopReason = "tool_use"
				id, _ := toolUse["id"].(string)
				name, _ := toolUse["name"].(string)
				input, _ := toolUse["input"].(map[string]interface{})
				sb.EmitToolUse(id, name, input)
			}
		}
	}

	// Flush heuristic parser
	for _, toolUse := range heuristicParser.Flush() {
		stopReason = "tool_use"
		id, _ := toolUse["id"].(string)
		name, _ := toolUse["name"].(string)
		input, _ := toolUse["input"].(map[string]interface{})
		sb.EmitToolUse(id, name, input)
	}

	sb.StopMessage(stopReason)
}

// sse writes a Server-Sent Event to the response.
func sse(w http.ResponseWriter, event, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// newMsgID generates a pseudo-unique message ID.
func newMsgID() string {
	return fmt.Sprintf("msg_%d_%d", time.Now().UnixNano(), randInt63())
}

// writeAnthropicError writes an error in Anthropic format.
func writeAnthropicError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "api_error",
			"message": msg,
		},
	})
}

// randInt63 returns a random int63 for message IDs.
func randInt63() int64 {
	// Simple pseudo-random without importing math/rand
	return int64(time.Now().UnixNano() & 0x7FFFFFFFFFFFFFFF)
}

// ── API Handlers (port from dashboard.go) ─────────────────────────────────────

// HandleAPIProviders returns providers as JSON (GET) and manages CRUD (POST/PUT/DELETE).
func (px *Proxy) HandleAPIProviders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		data, err := px.cfg.ProviderJSON()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)

	case http.MethodPost:
		var p Provider
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		p.Status = "unknown"
		p.LastCheck = ""
		if err := px.cfg.AddProvider(p); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(p)

	case http.MethodPut:
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 3 {
			http.Error(w, "missing index", 400)
			return
		}
		var idx int
		fmt.Sscanf(parts[2], "%d", &idx)
		var p Provider
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := px.cfg.UpdateProvider(idx, p); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(p)

	case http.MethodDelete:
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 3 {
			http.Error(w, "missing index", 400)
			return
		}
		var idx int
		fmt.Sscanf(parts[2], "%d", &idx)
		if err := px.cfg.RemoveProvider(idx); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	}
}

// HandleAPITest tests a provider connection.
func (px *Proxy) HandleAPITest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 {
		http.Error(w, "missing index", 400)
		return
	}
	var idx int
	fmt.Sscanf(parts[3], "%d", &idx)

	status, latency, modelCount, errMsg := px.pm.TestConnection(idx)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      status,
		"latency_ms":  latency,
		"model_count": modelCount,
		"error":       errMsg,
	})
}

// HandleAPISwitch switches the active provider.
func (px *Proxy) HandleAPISwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		ProviderIdx int    `json:"provider_idx"`
		Model       string `json:"model,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	if err := px.cfg.SetActiveProvider(req.ProviderIdx); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	if req.Model != "" {
		p, _ := px.cfg.GetProviderByIndex(req.ProviderIdx)
		p.Model = req.Model
		px.cfg.UpdateProvider(req.ProviderIdx, p)
	}

	// Dynamically update Claude Code's settings.json so the UI shows the real model
	if p, ok := px.cfg.GetProviderByIndex(req.ProviderIdx); ok && p.Model != "" {
		updateClaudeSettings(p.Model)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"active_idx": req.ProviderIdx,
		"status":     "ok",
	})
}

// updateClaudeSettings rewrites ~/.claude/settings.json to inject customModels mapping
// the Anthropic defaults to the proxy's active model, so Claude Code's UI shows the true active model.
func updateClaudeSettings(modelName string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return
	}

	customModels, ok := settings["customModels"].(map[string]interface{})
	if !ok {
		customModels = make(map[string]interface{})
	}

	// Map the standard Claude aliases to the current active model
	customModels["claude-3-7-sonnet-20250219"] = modelName
	customModels["claude-3-5-sonnet-20241022"] = modelName
	customModels["claude-3-5-haiku-20241022"] = modelName
	customModels["claude-3-opus-20240229"] = modelName

	settings["customModels"] = customModels

	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return
	}

	os.WriteFile(settingsPath, newData, 0644)

	// Ensure the terminal integration is set so the user truly doesn't have to type anything manually.
	EnsureTerminalIntegration()
}

// EnsureTerminalIntegration automatically sets ANTHROPIC_BASE_URL in the system.
func EnsureTerminalIntegration() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	exportStr := "export ANTHROPIC_BASE_URL=http://localhost:8082/v1"

	if runtime.GOOS == "windows" {
		// Set environment variable persistently in Windows
		exec.Command("setx", "ANTHROPIC_BASE_URL", "http://localhost:8082/v1").Run()
	} else {
		// macOS / Linux: Cleanly inject without duplicating or leaving old ports
		files := []string{".zshrc", ".bashrc", ".bash_profile"}
		for _, f := range files {
			path := filepath.Join(home, f)
			if _, err := os.Stat(path); err == nil {
				data, err := os.ReadFile(path)
				if err != nil {
					continue
				}

				lines := strings.Split(string(data), "\n")
				var newLines []string

				for _, line := range lines {
					if strings.Contains(line, "ANTHROPIC_BASE_URL") || strings.Contains(line, "Claude Proxy Pro Auto-Injection") {
						continue // Filter out old or competitor exports
					}
					newLines = append(newLines, line)
				}

				// Trim trailing empty lines
				for len(newLines) > 0 && strings.TrimSpace(newLines[len(newLines)-1]) == "" {
					newLines = newLines[:len(newLines)-1]
				}

				// Append our clean integration
				newLines = append(newLines, "")
				newLines = append(newLines, "# Claude Proxy Pro Auto-Injection")
				newLines = append(newLines, exportStr)

				os.WriteFile(path, []byte(strings.Join(newLines, "\n")+"\n"), 0644)
			}
		}
	}
}

// clearClaudeSettings removes our injected customModels from settings.json
func clearClaudeSettings() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return
	}

	// Remove customModels entirely to restore native Claude Code behavior
	delete(settings, "customModels")

	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return
	}

	os.WriteFile(settingsPath, newData, 0644)
}

// RemoveTerminalIntegration cleans up the injected variables from the terminal profile or registry
func RemoveTerminalIntegration() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	if runtime.GOOS == "windows" {
		// Remove environment variable persistently in Windows
		exec.Command("REG", "DELETE", "HKCU\\Environment", "/v", "ANTHROPIC_BASE_URL", "/f").Run()
	} else {
		// macOS / Linux: Cleanly remove from .zshrc and .bashrc
		files := []string{".zshrc", ".bashrc", ".bash_profile"}
		for _, f := range files {
			path := filepath.Join(home, f)
			if _, err := os.Stat(path); err == nil {
				data, err := os.ReadFile(path)
				if err != nil {
					continue
				}

				lines := strings.Split(string(data), "\n")
				var newLines []string

				for _, line := range lines {
					if strings.Contains(line, "ANTHROPIC_BASE_URL") || strings.Contains(line, "Claude Proxy Pro Auto-Injection") {
						continue // Filter out our exports
					}
					newLines = append(newLines, line)
				}

				// Trim trailing empty lines
				for len(newLines) > 0 && strings.TrimSpace(newLines[len(newLines)-1]) == "" {
					newLines = newLines[:len(newLines)-1]
				}

				os.WriteFile(path, []byte(strings.Join(newLines, "\n")+"\n"), 0644)
			}
		}
	}
}

// HandleAPIStats returns statistics.
func (px *Proxy) HandleAPIStats(w http.ResponseWriter, r *http.Request) {
	data, err := px.stats.StatsJSON()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// HandleAPILogs returns request logs.
func (px *Proxy) HandleAPILogs(w http.ResponseWriter, r *http.Request) {
	data, err := px.stats.LogsJSON()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// HandleAPIHealth returns health/stability status.
func (px *Proxy) HandleAPIHealth(w http.ResponseWriter, r *http.Request) {
	stability := px.stability.GetStatus()
	appCfg := px.cfg.Get()
	result := map[string]interface{}{
		"is_healthy":      stability.IsHealthy,
		"uptime":          stability.Uptime,
		"start_time":      stability.StartTime.Format(time.RFC3339),
		"total_requests":  stability.TotalRequests,
		"total_errors":    stability.TotalErrors,
		"total_retries":   stability.TotalRetries,
		"total_failovers": stability.TotalFailovers,
		"active_idx":      appCfg.ActiveIdx,
		"auto_retry":      appCfg.AutoRetry,
		"failover":        appCfg.Failover,
		"retry_max":       appCfg.RetryMax,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// HandleAPIModels returns discovered models.
func (px *Proxy) HandleAPIModels(w http.ResponseWriter, r *http.Request) {
	data, err := px.cfg.ModelsJSON()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// HandleAPIDiscover triggers model discovery.
func (px *Proxy) HandleAPIDiscover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	go px.pm.DiscoverModels()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "discovery started"})
}

// HandleAPIConfig returns/updates the full config.
func (px *Proxy) HandleAPIConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		data, err := px.cfg.ConfigJSON()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	case http.MethodPut:
		var cfg AppConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		current := px.cfg.Get()
		current.AutoRetry = cfg.AutoRetry
		current.Failover = cfg.Failover
		current.RetryMax = cfg.RetryMax
		current.CheckInterval = cfg.CheckInterval
		px.cfg.mu.Lock()
		px.cfg.cfg = current
		px.cfg.mu.Unlock()
		if err := px.cfg.save(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(current)
	}
}

func logTraffic(format string, args ...interface{}) {
	f, err := os.OpenFile("/Users/fakhreddinefarhat/claude-proxy-pro/traffic.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] ", time.Now().Format(time.RFC3339))
	fmt.Fprintf(f, format, args...)
	fmt.Fprintln(f)
}
