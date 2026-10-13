package chat

type ChatResult struct {
	ConversationID string            `json:"conversation_id,omitempty"`
	Answer         string            `json:"answer"`
	MatchedRowID   *int              `json:"matched_row_id,omitempty"`
	Debug          *ChatDebugMetrics `json:"debug,omitempty"`
}

type ChatDebugMetrics struct {
	Strategy              string `json:"strategy"`
	ExecutionMode         string `json:"execution_mode,omitempty"`
	QueryType             string `json:"query_type,omitempty"`
	RetrievalMode         string `json:"retrieval_mode,omitempty"`
	PromptProjectionMode  string `json:"prompt_projection_mode,omitempty"`
	Version               int    `json:"version"`
	CommunityFilterCount  int    `json:"community_filter_count"`
	TotalRowsLoaded       int    `json:"total_rows_loaded"`
	RowsSelected          int    `json:"rows_selected"`
	NarrativeRowsIncluded int    `json:"narrative_rows_included,omitempty"`
	PromptChars           int    `json:"prompt_chars"`
	PromptBytes           int    `json:"prompt_bytes"`
	AudioIncluded         bool   `json:"audio_included"`
	PrimaryModel          string `json:"primary_model,omitempty"`
	UsedModel             string `json:"used_model,omitempty"`
	PreparationMillis     int64  `json:"preparation_ms"`
	GenerationMillis      int64  `json:"generation_ms"`
	TotalMillis           int64  `json:"total_ms"`
}

type ttsJSON struct {
	Text   string `json:"text"`
	Answer string `json:"answer"`
}

type TTSAudio struct {
	MimeType string
	Data     []byte
}

const (
	ttsModel      = "gemini-2.5-flash-tts"
	ttsVoiceName  = "Algenib"
	ttsStyleInstr = "empathetic and more human like"
	ttsLanguage   = "English (United States)"
)

const (
	ttsSampleRate    = 24000
	ttsChannels      = 1
	ttsBitsPerSample = 16
)
