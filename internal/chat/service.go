package chat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/oauth2/google"
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

	defaultHTTPClientHook = func(ctx context.Context) (*http.Client, error) {
		return google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
	}
	httpDoHook = func(c *http.Client, req *http.Request) (*http.Response, error) {
		return c.Do(req)
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

func (cs *ChatService) TTS(text string) (*TTSAudio, error) {
	ctx := requestscope.Context(cs.DB)

	project := strings.TrimSpace(cs.ProjectID)
	loc := strings.TrimSpace(cs.Location)

	if cs.Client != nil {
		cc := cs.Client.ClientConfig()
		if project == "" {
			project = strings.TrimSpace(cc.Project)
		}
		if loc == "" {
			loc = strings.TrimSpace(cc.Location)
		}
	}
	if project == "" {
		project = strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT"))
	}

	if project == "" {
		return nil, fmt.Errorf("missing project id (set ChatService.ProjectID or GOOGLE_CLOUD_PROJECT)")
	}
	if loc == "" {
		loc = "global"
	}

	host := "https://aiplatform.googleapis.com"
	if strings.ToLower(loc) != "global" {
		host = fmt.Sprintf("https://%s-aiplatform.googleapis.com", loc)
	}

	url := fmt.Sprintf(
		"%s/v1beta1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
		host, project, loc, ttsModel,
	)

	prompt := fmt.Sprintf(
		"Style: %s.\nRead the following TEXT exactly as written. Do not add or remove words.\n\nTEXT:\n%s",
		ttsStyleInstr, text,
	)

	reqBody := map[string]any{
		"contents": []any{
			map[string]any{
				"role": "user",
				"parts": []any{
					map[string]any{"text": prompt},
				},
			},
		},
		"generationConfig": map[string]any{
			"responseModalities": []string{"AUDIO"},
			"speechConfig": map[string]any{
				"voiceConfig": map[string]any{
					"prebuiltVoiceConfig": map[string]any{
						"voiceName": ttsVoiceName,
					},
				},
			},
		},
	}

	payload, _ := json.Marshal(reqBody)

	httpClient, err := defaultHTTPClientHook(ctx)
	if err != nil {
		return nil, fmt.Errorf("adc auth error: %w", err)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpDoHook(httpClient, req)
	if err != nil {
		return nil, fmt.Errorf("vertex tts request error: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vertex tts failed (%d): %s", resp.StatusCode, string(raw))
	}

	type inlineData struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	}
	type part struct {
		InlineData *inlineData `json:"inlineData,omitempty"`
		Text       string      `json:"text,omitempty"`
	}
	type content struct {
		Parts []part `json:"parts"`
	}
	type candidate struct {
		Content content `json:"content"`
	}
	type genResp struct {
		Candidates []candidate `json:"candidates"`
	}

	var gr genResp
	if err := json.Unmarshal(raw, &gr); err != nil {
		return nil, fmt.Errorf("failed to parse vertex response: %w", err)
	}
	if len(gr.Candidates) == 0 || len(gr.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("no audio returned from vertex")
	}

	for _, p := range gr.Candidates[0].Content.Parts {
		if p.InlineData == nil || p.InlineData.Data == "" {
			continue
		}

		pcm, err := decodeBase64Loose(p.InlineData.Data)
		if err != nil {
			return nil, fmt.Errorf("failed to decode audio data: %w", err)
		}

		rate := parseRateFromMime(p.InlineData.MimeType)
		if rate == 0 {
			rate = ttsSampleRate
		}

		wavBytes := pcmToWav(pcm, rate, ttsChannels, ttsBitsPerSample)
		return &TTSAudio{MimeType: "audio/wav", Data: wavBytes}, nil
	}

	return nil, fmt.Errorf("no inline audio returned from vertex")
}

var rateRe = regexp.MustCompile(`rate\s*=\s*(\d+)`)

func parseRateFromMime(m string) int {
	m = strings.ToLower(m)
	if m == "" {
		return 0
	}
	mm := rateRe.FindStringSubmatch(m)
	if len(mm) == 2 {
		if v, err := strconv.Atoi(mm[1]); err == nil && v > 0 {
			return v
		}
	}
	return 0
}

func decodeBase64Loose(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

func pcmToWav(pcm []byte, sampleRate, channels, bitsPerSample int) []byte {
	if sampleRate <= 0 {
		sampleRate = ttsSampleRate
	}
	if channels <= 0 {
		channels = ttsChannels
	}
	if bitsPerSample <= 0 {
		bitsPerSample = ttsBitsPerSample
	}

	byteRate := sampleRate * channels * (bitsPerSample / 8)
	blockAlign := channels * (bitsPerSample / 8)
	dataSize := len(pcm)
	riffSize := 36 + dataSize

	buf := new(bytes.Buffer)
	buf.WriteString("RIFF")
	_ = binary.Write(buf, binary.LittleEndian, uint32(riffSize))
	buf.WriteString("WAVE")

	buf.WriteString("fmt ")
	_ = binary.Write(buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(buf, binary.LittleEndian, uint16(channels))
	_ = binary.Write(buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(buf, binary.LittleEndian, uint32(byteRate))
	_ = binary.Write(buf, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(buf, binary.LittleEndian, uint16(bitsPerSample))

	buf.WriteString("data")
	_ = binary.Write(buf, binary.LittleEndian, uint32(dataSize))
	buf.Write(pcm)

	return buf.Bytes()
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
	resp, err := genaiGenerateContentHook(cs.Client, ctx, primary, contents, config)
	if err == nil {
		return resp, primary, nil
	}
	if !isRateLimit429(err) {
		return nil, primary, err
	}

	resp2, err2 := genaiGenerateContentHook(cs.Client, ctx, fallback, contents, config)
	if err2 == nil {
		return resp2, fallback, nil
	}

	return nil, fallback, err2
}
