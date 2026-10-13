package triage

import (
	"context"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/orionsbelt-dev/chuck/internal/config"
)

// HaikuModel is the triage model.
const HaikuModel = "claude-haiku-4-5"

// Classifier turns an event into raw model output. Validation happens in
// ParseResult, so invalid output is still stored with its cost.
type Classifier interface {
	Classify(ctx context.Context, in Input) (raw string, u Usage, err error)
}

// Haiku classifies with Claude Haiku using structured outputs, so the
// response is schema-valid JSON.
type Haiku struct {
	client anthropic.Client
	system string
	schema map[string]any
}

// NewHaiku builds a classifier for p's actions. Pass option.WithAPIKey.
func NewHaiku(p config.Policy, opts ...option.RequestOption) *Haiku {
	actions := ProposableActions(p)
	return &Haiku{
		client: anthropic.NewClient(opts...),
		system: systemPrompt(p, actions),
		schema: resultSchema(actions),
	}
}

func (h *Haiku) Classify(ctx context.Context, in Input) (string, Usage, error) {
	msg, err := h.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     HaikuModel,
		MaxTokens: 1024,
		System:    []anthropic.TextBlockParam{{Text: h.system}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userPrompt(in))),
		},
		OutputConfig: anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{Schema: h.schema},
		},
	})
	if err != nil {
		return "", Usage{}, err
	}
	var text strings.Builder
	for _, block := range msg.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(tb.Text)
		}
	}
	// A refusal or max_tokens stop leaves text that fails ParseResult and is
	// recorded as triage_failed with its cost.
	return text.String(), Usage{Model: HaikuModel, InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens}, nil
}
