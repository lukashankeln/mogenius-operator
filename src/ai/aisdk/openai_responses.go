package aisdk

import (
	"context"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// The Responses API (/v1/responses) is used for official OpenAI endpoints
// instead of Chat Completions. Reason: GPT-5-family reasoning models reject
// function tools combined with an active reasoning_effort on
// /v1/chat/completions (400 Bad Request). The Responses API supports tools and
// reasoning together, so agents and interactive chat can use tools on those
// models. Chat Completions stays the path for custom-baseURL / OpenAI-compatible
// endpoints (Azure, LiteLLM, vLLM, proxies) that may not implement /v1/responses
// — see OpenAIProvider.useResponsesAPI.

// toResponsesInput maps provider-neutral messages into Responses API input
// items. System messages are collected into the returned instructions string
// (the Responses API's dedicated system/developer channel); everything else
// becomes an input item, preserving the full multi-turn tool-call history.
func toResponsesInput(messages []Message) (string, responses.ResponseInputParam) {
	var instructions strings.Builder
	items := make(responses.ResponseInputParam, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case RoleSystem:
			if instructions.Len() > 0 {
				instructions.WriteString("\n\n")
			}
			instructions.WriteString(m.Content)

		case RoleUser:
			items = append(items, responses.ResponseInputItemParamOfMessage(m.Content, responses.EasyInputMessageRoleUser))

		case RoleAssistant:
			if m.Content != "" {
				items = append(items, responses.ResponseInputItemParamOfMessage(m.Content, responses.EasyInputMessageRoleAssistant))
			}
			for _, tc := range m.ToolCalls {
				items = append(items, responses.ResponseInputItemParamOfFunctionCall(tc.Arguments, tc.ID, tc.Name))
			}

		case RoleTool:
			item := responses.ResponseInputItemParamOfFunctionCallOutput(m.Content)
			if item.OfFunctionCallOutput != nil {
				item.OfFunctionCallOutput.CallID = openai.String(m.ToolCallID)
			}
			items = append(items, item)
		}
	}
	return instructions.String(), items
}

// toResponsesTools maps provider-neutral tools into Responses API function
// tools. strict is false to keep parity with the Chat Completions path, which
// passes MCP tool schemas through unmodified.
func toResponsesTools(tools []Tool) []responses.ToolUnionParam {
	params := make([]responses.ToolUnionParam, len(tools))
	for i, t := range tools {
		props := t.InputSchema
		if props == nil {
			props = map[string]any{}
		}
		schema := map[string]any{
			"type":       "object",
			"properties": props,
		}
		if len(t.Required) > 0 {
			schema["required"] = t.Required
		}
		params[i] = responses.ToolParamOfFunction(t.Name, schema, false)
	}
	return params
}

// responsesParams assembles the shared ResponseNewParams for both the streaming
// and non-streaming paths. Store is false so the API stays stateless (no
// previous_response_id); Reasoning is left unset so the model applies its own
// default — the whole point of using this API for GPT-5 reasoning models.
func responsesParams(model string, messages []Message, tools []Tool) responses.ResponseNewParams {
	instructions, input := toResponsesInput(messages)
	params := responses.ResponseNewParams{
		Model: model,
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: input},
		Store: openai.Bool(false),
	}
	if instructions != "" {
		params.Instructions = openai.String(instructions)
	}
	if len(tools) > 0 {
		params.Tools = toResponsesTools(tools)
	}
	return params
}

// responsesFinishReason maps a Responses API terminal status onto the
// Chat-Completions-style finish reason the agent loop understands. An
// incomplete response caused by the output-token ceiling maps to "length",
// which the loop treats as a context/budget stop.
func responsesFinishReason(status responses.ResponseStatus, incompleteReason string) string {
	if status == responses.ResponseStatusIncomplete {
		if incompleteReason == "max_output_tokens" {
			return "length"
		}
		return string(status)
	}
	return ""
}

// chatResponses is the non-streaming Chat implementation over /v1/responses.
func (p *OpenAIProvider) chatResponses(ctx context.Context, model string, messages []Message, tools []Tool) (Response, error) {
	resp, err := p.client.Responses.New(ctx, responsesParams(model, messages, tools))
	if err != nil {
		return Response{}, err
	}

	var toolCalls []ToolCall
	for _, item := range resp.Output {
		if item.Type == "function_call" {
			fc := item.AsFunctionCall()
			toolCalls = append(toolCalls, ToolCall{
				ID:        fc.CallID,
				Name:      fc.Name,
				Arguments: fc.Arguments,
			})
		}
	}

	return Response{
		Content:      resp.OutputText(),
		ToolCalls:    toolCalls,
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		FinishReason: responsesFinishReason(resp.Status, resp.IncompleteDetails.Reason),
	}, nil
}

// chatStreamResponses is the streaming ChatStream implementation over
// /v1/responses. Text is streamed live via output_text.delta events; tool calls
// and usage are read from the terminal response on the completed/incomplete
// event, mirroring the Chat Completions streaming contract.
func (p *OpenAIProvider) chatStreamResponses(ctx context.Context, model string, messages []Message, tools []Tool) (<-chan StreamChunk, error) {
	stream := p.client.Responses.NewStreaming(ctx, responsesParams(model, messages, tools))

	ch := make(chan StreamChunk, 16)
	go func() {
		defer close(ch)

		var inputTokens, outputTokens int64
		var finishReason string
		var toolCalls []ToolCall

		for stream.Next() {
			event := stream.Current()
			switch event.Type {
			case "response.output_text.delta":
				if event.Delta != "" {
					ch <- StreamChunk{Content: event.Delta}
				}

			case "response.output_item.added":
				if event.Item.Type == "function_call" {
					if name := event.Item.AsFunctionCall().Name; name != "" {
						ch <- StreamChunk{ToolCallName: name}
					}
				}

			case "response.completed", "response.incomplete":
				r := event.Response
				inputTokens = r.Usage.InputTokens
				outputTokens = r.Usage.OutputTokens
				for _, item := range r.Output {
					if item.Type == "function_call" {
						fc := item.AsFunctionCall()
						toolCalls = append(toolCalls, ToolCall{ID: fc.CallID, Name: fc.Name, Arguments: fc.Arguments})
					}
				}
				finishReason = responsesFinishReason(r.Status, r.IncompleteDetails.Reason)
			}
		}

		if err := stream.Err(); err != nil {
			ch <- StreamChunk{Err: fmt.Errorf("streaming error: %w", err), Done: true}
			return
		}

		ch <- StreamChunk{
			Done:         true,
			ToolCalls:    toolCalls,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			FinishReason: finishReason,
		}
	}()

	return ch, nil
}
