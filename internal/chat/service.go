package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sync/singleflight"
	"google.golang.org/api/googleapi"
	"google.golang.org/genai"
	"gorm.io/gorm"

	f "nordik-drive-api/internal/file"
	"nordik-drive-api/internal/requestscope"
)

type ChatService struct {
	DB     *gorm.DB
	APIKey string
	Client *genai.Client

	ProjectID string
	Location  string

	stateMu       sync.Mutex
	state         *chatState
	QueryStrategy ChatQueryStrategy
}

type chatState struct {
	datasetCache           chatBoundedCache
	structuredDatasetCache chatBoundedCache
	databaseQueryCache     chatBoundedCache
	fastAnswerCacheMu      sync.RWMutex
	fastAnswerCache        map[string]*ChatResult
	fastAnswerCacheOrder   []string
	fileLookupGroup        singleflight.Group
	fastRouteGroup         singleflight.Group
	fullGenerationGroup    singleflight.Group
}

func (cs *ChatService) sharedState() *chatState {
	cs.stateMu.Lock()
	defer cs.stateMu.Unlock()
	if cs.state == nil {
		cs.state = &chatState{}
	}
	return cs.state
}

var (
	genaiGenerateContentHook = func(client *genai.Client, ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return client.Models.GenerateContent(ctx, model, contents, config)
	}

	openMultipartFileHook = func(fh *multipart.FileHeader) (multipart.File, error) {
		return fh.Open()
	}
	readAllHook = func(r io.Reader) ([]byte, error) {
		return io.ReadAll(r)
	}
)

func firstTextFromResp(genResp *genai.GenerateContentResponse) string {
	if genResp == nil {
		return ""
	}
	for _, candidate := range genResp.Candidates {
		if candidate.Content == nil {
			continue
		}
		for _, part := range candidate.Content.Parts {
			if strings.TrimSpace(part.Text) != "" {
				return part.Text
			}
		}
	}
	return ""
}

func (cs *ChatService) generateFromPrompt(
	ctx context.Context,
	prompt string,
	audioBytes []byte,
	audioMime string,
) (answer string, usedModel string, err error) {
	return cs.generateFromPromptWithModels(ctx, prompt, audioBytes, audioMime, chatFastModel, chatQualityModel)
}

func (cs *ChatService) generateFromPromptWithModels(
	ctx context.Context,
	prompt string,
	audioBytes []byte,
	audioMime string,
	primaryModel string,
	fallbackModel string,
) (answer string, usedModel string, err error) {
	return cs.generateFromPromptWithModelsConfig(ctx, prompt, audioBytes, audioMime, primaryModel, fallbackModel, nil)
}

func (cs *ChatService) generateFromPromptWithModelsConfig(
	ctx context.Context,
	prompt string,
	audioBytes []byte,
	audioMime string,
	primaryModel string,
	fallbackModel string,
	config *genai.GenerateContentConfig,
) (answer string, usedModel string, err error) {
	if cs.Client == nil {
		return "", "", fmt.Errorf("genai client not initialized")
	}

	parts := []*genai.Part{{Text: prompt}}
	if len(audioBytes) > 0 {
		audioMime = strings.TrimSpace(audioMime)
		if audioMime == "" || audioMime == "application/octet-stream" {
			audioMime = "audio/webm"
		}
		parts = append(parts, &genai.Part{
			InlineData: &genai.Blob{Data: audioBytes, MIMEType: audioMime},
		})
	}

	contents := []*genai.Content{
		{Role: "user", Parts: parts},
	}

	genResp, usedModel, err := cs.generateWithModelFallback(ctx, primaryModel, fallbackModel, contents, config)
	if err != nil {
		return "", usedModel, err
	}

	out := strings.TrimSpace(firstTextFromResp(genResp))
	if out == "" {
		return "", usedModel, fmt.Errorf("no response from Gemini")
	}

	return out, usedModel, nil
}

func (cs *ChatService) DescribeRow(rowID int) (string, error) {
	describeInstruction := `
You are writing a respectful, empathetic narration about ONE individual using ONLY the provided record.

Style requirements:
- Warm, human, and gentle tone.
- 1-2 short paragraphs (no bullet points).
- Do NOT mention JSON, database, tables, ids, or technical details.

Accuracy requirements:
- Use ONLY what appears in the record.
- Do NOT guess or add details (age, family, story, emotions, location history) if not present.
- If a detail is missing (date/cause/community/school), say it is not listed.

Safety:
- Avoid graphic details. If cause/factor is present, describe it briefly and sensitively.

Output:
- Start by stating the person's name (if available).
- Summarize key fields that exist (community/school/date/cause/notes), and clearly say when something isn't listed.
`

	return cs.generateRowNarrative(rowID, describeInstruction)
}

func (cs *ChatService) GenerateHonourText(rowID int) (string, error) {
	honourInstruction := `
You are writing a respectful daily honour for ONE survivor using ONLY the provided record.

Style requirements:
- Warm, gentle, and commemorative tone.
- Write one short paragraph.
- Keep it concise and readable for a daily highlight.
- Do NOT mention JSON, database, tables, ids, or technical details.

Accuracy requirements:
- Use ONLY what appears in the record.
- Do NOT guess or invent biography, family history, age, or emotions.
- If details are missing, simply focus on what is listed instead of filling gaps.

Safety:
- Avoid graphic detail.
- If death or harm-related information appears, mention it briefly and sensitively.

Output:
- Begin with the person's name if it is listed.
- Honour the person with the known details from the record in a respectful way.
`

	return cs.generateRowNarrative(rowID, honourInstruction)
}

func (cs *ChatService) generateRowNarrative(rowID int, instruction string) (string, error) {
	if cs.DB == nil {
		return "", fmt.Errorf("db not initialized")
	}
	if cs.Client == nil {
		return "", fmt.Errorf("genai client not initialized")
	}

	var row f.FileData
	if err := cs.DB.First(&row, rowID).Error; err != nil {
		return "", fmt.Errorf("row not found")
	}

	rowJSON := strings.TrimSpace(string(row.RowData))
	prompt := fmt.Sprintf(
		"%s\n\nRECORD (only source of truth):\n%s",
		strings.TrimSpace(instruction),
		rowJSON,
	)

	ctx := requestscope.Context(cs.DB)
	out, usedModel, err := cs.generateFromPrompt(ctx, prompt, nil, "")
	if err != nil {
		return "", fmt.Errorf("generation error (%s): %w", usedModel, err)
	}
	return out, nil
}

func normalizeCommunities(communities []string) []string {
	filtered := make([]string, 0, len(communities))
	seen := make(map[string]struct{}, len(communities))
	for _, community := range communities {
		community = strings.TrimSpace(community)
		if community == "" {
			continue
		}
		if _, exists := seen[community]; exists {
			continue
		}
		seen[community] = struct{}{}
		filtered = append(filtered, community)
	}
	sort.Strings(filtered)
	return filtered
}

func (cs *ChatService) getQueryStrategy() ChatQueryStrategy {
	if cs != nil && cs.QueryStrategy != nil {
		return cs.QueryStrategy
	}
	return structuredRetrievalChatStrategy{}
}

func isRateLimit429(err error) bool {
	if err == nil {
		return false
	}

	var gerr *googleapi.Error
	if errors.As(err, &gerr) && gerr.Code == http.StatusTooManyRequests {
		return true
	}

	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "429") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "resource_exhausted") ||
		strings.Contains(msg, "rate limit") {
		return true
	}

	return false
}

func (cs *ChatService) generateWith429Fallback(
	ctx context.Context,
	contents []*genai.Content,
) (*genai.GenerateContentResponse, string, error) {
	return cs.generateWithModelFallback(ctx, "gemini-2.5-flash", "gemini-2.5-pro", contents, nil)
}

func (cs *ChatService) generateWithModelFallback(
	ctx context.Context,
	primary string,
	fallback string,
	contents []*genai.Content,
	config *genai.GenerateContentConfig,
) (*genai.GenerateContentResponse, string, error) {
	resp, err := cs.measuredGenerate(ctx, primary, contents, config)
	if err == nil {
		return resp, primary, nil
	}
	if !isRateLimit429(err) {
		return nil, primary, err
	}

	resp2, err2 := cs.measuredGenerate(ctx, fallback, contents, config)
	if err2 == nil {
		return resp2, fallback, nil
	}

	return nil, fallback, err2
}
