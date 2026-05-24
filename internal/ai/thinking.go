package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

// ThinkingCard is the minimal card payload the AI sees. The
// "Enriched" version below carries prior AI output too, so the model
// has graph context instead of just raw text.
type ThinkingCard struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// ThinkingCardWithEnrichment lets us pass siblings' prior enrichments
// (summary + connections) into a new card's enrichment call so the model
// builds on the existing graph instead of starting blank each time.
type ThinkingCardWithEnrichment struct {
	ID          int64               `json:"id"`
	Kind        string              `json:"kind"`
	Content     string              `json:"content"`
	Summary     string              `json:"summary,omitempty"`
	Connections []ThinkingConnection `json:"connections,omitempty"`
}

// ThinkingEnrichment is what the AI returns per card. Stored verbatim
// as the card's ai_enrichment JSONB blob.
type ThinkingEnrichment struct {
	Summary         string               `json:"summary"`
	Implications    []string             `json:"implications"`
	QuestionsRaised []string             `json:"questions_raised"`
	Connections     []ThinkingConnection `json:"connections"`
	Confidence      float64              `json:"confidence"`
}

type ThinkingConnection struct {
	CardID   int64  `json:"card_id"`
	Relation string `json:"relation"`
}

// Twelve-relation vocabulary. Lets the model express richer ties than
// "supports / related". Frontend renders these as short uppercase
// labels next to the connection target.
const ThinkingRelationsHelp = `Relations vocabulary (pick the most specific that fits):
- supports     : this card backs up / strengthens the other
- contradicts  : this card opposes the other
- extends      : this card builds on / adds to the other
- depends_on   : this card only makes sense if the other holds
- refines      : this card sharpens or qualifies the other
- fixes        : this card resolves / answers / closes the other
- refs         : this card mentions / cites the other
- points       : this card points at the other as the next step
- questions    : this card raises a question about the other
- exemplifies  : this card is a concrete example of the other
- generalizes  : this card abstracts the other into a wider claim
- related      : fallback for a thematic but weak tie`

// EnrichThinkingCard asks Gemini to produce an enrichment payload for one
// card, given its sibling cards (with their existing enrichments) as
// context. The model is told to act as a research partner curating a
// structure, not a chat respondent answering a prompt.
func (s *Service) EnrichThinkingCard(ctx context.Context, projectTitle, projectDesc string, target ThinkingCard, siblings []ThinkingCardWithEnrichment) (*ThinkingEnrichment, error) {
	sys := `You are a research partner sitting next to the user. The user is "thinking" inside a project — capturing typed cards (entity, question, idea, reflection, claim, fact, hypothesis, evidence, contradiction, decision, todo, or untyped note). You do NOT chat. You quietly enrich the user's structure of thought, card by card.

For the TARGET card below, given the rest of the project (each sibling already carries its own summary + outbound connections from prior enrichments), return a JSON enrichment that:

- summary: ONE sentence (≤24 words) distilling what this card means inside the project's current shape. Don't restate; interpret.
- implications: 1-3 short bullets. What follows if this card holds?
- questions_raised: 1-3 short open questions this card surfaces that the project hasn't answered yet.
- connections: 0-6 entries pointing to sibling cards by id with a relation. ` + ThinkingRelationsHelp + `
  Prefer fewer high-confidence connections over many weak ones. Use existing sibling connections as evidence of what the user has been linking.
- confidence: 0.0-1.0, your confidence given the context you had.

Reply with ONLY a single JSON object. No prose. No markdown fences.`

	siblingJSON, _ := json.Marshal(siblings)
	targetJSON, _ := json.Marshal(target)

	prompt := fmt.Sprintf("Project: %s\nDescription: %s\n\nTarget card:\n%s\n\nSibling cards (%d, with prior enrichments):\n%s",
		projectTitle, projectDesc, string(targetJSON), len(siblings), string(siblingJSON))

	temp := float32(0.4)
	maxOut := int32(900)
	thinkBudget := int32(0)
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: sys}}},
		Temperature:       &temp,
		MaxOutputTokens:   maxOut,
		ThinkingConfig:    &genai.ThinkingConfig{ThinkingBudget: &thinkBudget},
		ResponseMIMEType:  "application/json",
	}
	resp, err := s.client.Models.GenerateContent(ctx, s.model, []*genai.Content{
		{Role: "user", Parts: []*genai.Part{{Text: prompt}}},
	}, cfg)
	if err != nil {
		return nil, fmt.Errorf("enrich: %w", err)
	}
	raw := strings.TrimSpace(stripJSONFence(collectText(resp)))
	if raw == "" {
		return &ThinkingEnrichment{Confidence: 0}, nil
	}
	var out ThinkingEnrichment
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return &ThinkingEnrichment{Summary: raw, Confidence: 0.2}, nil
	}
	// Sanity: drop connections that don't refer to any real sibling.
	if len(out.Connections) > 0 {
		valid := make(map[int64]bool, len(siblings))
		for _, sib := range siblings {
			valid[sib.ID] = true
		}
		filtered := make([]ThinkingConnection, 0, len(out.Connections))
		for _, c := range out.Connections {
			if valid[c.CardID] && c.CardID != target.ID {
				filtered = append(filtered, c)
			}
		}
		out.Connections = filtered
	}
	return &out, nil
}

// SynthesizeThinking produces a long-form markdown brief for the whole
// project. The user opts into this — see frontend "Synthesize" button.
//
// Returns (markdownDoc, gapQuestions, error). Markdown is rendered
// verbatim in the project header; gapQuestions are surfaced as
// dismissible chips so the user can address them one at a time.
func (s *Service) SynthesizeThinking(ctx context.Context, projectTitle, projectDesc string, cards []ThinkingCardWithEnrichment) (string, []string, error) {
	sys := `You are the user's research partner. You have watched them think across the cards below — each typed, each previously enriched with a summary + connections. The user is NOT asking you a question. They want a written brief that captures where the project currently stands.

Produce a JSON object:

- thesis: a markdown document with these sections (omit a section only if there is genuinely nothing to write under it):

  # Overview
  2-3 sentences naming the territory the project is exploring.

  # Current thesis
  1-2 paragraphs of what the project is converging on. Be specific. Reference cards' content implicitly.

  # Evidence map
  Short bulleted clusters: group cards (by id and quoted phrase) that support each line of the thesis.

  # Tensions
  Bulleted list of contradictions / unresolved tensions between cards. Cite the conflicting card ids.

  # Suggested next thoughts
  3-5 prompts the user could capture next to deepen the project. These are seeds for cards, not chat replies.

- gap_questions: 3-6 short open questions, highest-leverage first. These mirror what's in "Suggested next thoughts" but as one-liners suitable for a chip UI.

Reply with ONLY a single JSON object. The thesis VALUE must be a single markdown string (not nested JSON). No outer markdown fences.`

	body, _ := json.Marshal(cards)
	prompt := fmt.Sprintf("Project: %s\nDescription: %s\n\nCards (%d, with prior enrichments):\n%s",
		projectTitle, projectDesc, len(cards), string(body))

	temp := float32(0.5)
	maxOut := int32(2200)
	thinkBudget := int32(0)
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: sys}}},
		Temperature:       &temp,
		MaxOutputTokens:   maxOut,
		ThinkingConfig:    &genai.ThinkingConfig{ThinkingBudget: &thinkBudget},
		ResponseMIMEType:  "application/json",
	}
	resp, err := s.client.Models.GenerateContent(ctx, s.model, []*genai.Content{
		{Role: "user", Parts: []*genai.Part{{Text: prompt}}},
	}, cfg)
	if err != nil {
		return "", nil, fmt.Errorf("synthesize: %w", err)
	}
	raw := strings.TrimSpace(stripJSONFence(collectText(resp)))
	var out struct {
		Thesis       string   `json:"thesis"`
		GapQuestions []string `json:"gap_questions"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return raw, nil, nil
	}
	return out.Thesis, out.GapQuestions, nil
}

func collectText(resp *genai.GenerateContentResponse) string {
	if resp == nil || len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range resp.Candidates[0].Content.Parts {
		if p.Text != "" && !p.Thought {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func stripJSONFence(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
