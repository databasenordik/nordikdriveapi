package chat

import (
	"context"
	"fmt"

	f "nordik-drive-api/internal/file"
	"nordik-drive-api/internal/requestscope"
)

type fastTextChatOutcome struct {
	Result *ChatResult
	Routed bool
}

type fullTextGenerationOutcome struct {
	Answer    string
	UsedModel string
}

// lookupChatFile coalesces concurrent metadata lookups without retaining a TTL
// cache. This removes burst duplication while guaranteeing that a completed
// lookup never hides a newly uploaded file version.
func (cs *ChatService) lookupChatFile(filename string) (f.File, error) {
	value, err := coalesced(requestscope.Context(cs.DB), &cs.sharedState().fileLookupGroup, filename, func() (any, error) {
		var file f.File
		fileQuery := cs.DB.Select("id, version, description").Where("filename = ?", filename).Order("version DESC")
		if queryErr := fileQuery.First(&file).Error; queryErr != nil {
			if !isMissingDescriptionColumnError(queryErr) {
				return nil, fmt.Errorf("file not found")
			}
			if fallbackErr := cs.DB.Select("id, version").Where("filename = ?", filename).Order("version DESC").First(&file).Error; fallbackErr != nil {
				return nil, fmt.Errorf("file not found")
			}
		}
		return file, nil
	})
	if err != nil {
		return f.File{}, err
	}
	file, ok := value.(f.File)
	if !ok {
		return f.File{}, fmt.Errorf("file not found")
	}
	return file, nil
}

// tryCoalescedFastTextChat ensures identical concurrent deterministic requests
// share one database query and one model-rendering call. The answer cache is
// checked again inside the singleflight callback so a request arriving while
// another is finishing immediately reuses the completed answer.
func (cs *ChatService) tryCoalescedFastTextChat(input ChatQueryInput) (*ChatResult, bool, error) {
	if cached, ok := cs.getFastChatAnswer(input); ok {
		return cached, true, nil
	}

	value, err := coalesced(requestscope.Context(cs.DB), &cs.sharedState().fastRouteGroup, fastChatAnswerCacheKey(input), func() (any, error) {
		if cached, ok := cs.getFastChatAnswer(input); ok {
			return fastTextChatOutcome{Result: cached, Routed: true}, nil
		}
		if routed, ok := cs.tryDatabaseChat(input); ok {
			routed = cs.finalizeRoutedChatAnswer(requestscope.Context(cs.DB), input, routed)
			cs.storeFastChatAnswer(input, routed)
			return fastTextChatOutcome{Result: routed, Routed: true}, nil
		}

		routed, ok, routeErr := cs.tryDeterministicChat(input)
		if routeErr != nil {
			return nil, routeErr
		}
		if !ok {
			return fastTextChatOutcome{}, nil
		}
		routed = cs.finalizeRoutedChatAnswer(requestscope.Context(cs.DB), input, routed)
		cs.storeFastChatAnswer(input, routed)
		return fastTextChatOutcome{Result: routed, Routed: true}, nil
	})
	if err != nil {
		return nil, false, err
	}
	outcome, ok := value.(fastTextChatOutcome)
	if !ok || !outcome.Routed || outcome.Result == nil {
		return nil, false, nil
	}
	return cloneChatResult(outcome.Result), true, nil
}

// generateCoalescedFullTextAnswer prevents identical concurrent complex
// questions from consuming multiple model requests. Structured preparation can
// still happen concurrently, but the expensive remote generation is shared.
func (cs *ChatService) generateCoalescedFullTextAnswer(
	ctx context.Context,
	input ChatQueryInput,
	prompt string,
	primaryModel string,
	fallbackModel string,
) (string, string, error) {
	value, err := coalesced(ctx, &cs.sharedState().fullGenerationGroup, "full:"+fastChatAnswerCacheKey(input), func() (any, error) {
		answer, usedModel, generationErr := cs.generateFromPromptWithModels(ctx, prompt, nil, "", primaryModel, fallbackModel)
		if generationErr != nil {
			return nil, generationErr
		}
		return fullTextGenerationOutcome{Answer: answer, UsedModel: usedModel}, nil
	})
	if err != nil {
		return "", primaryModel, err
	}
	outcome, ok := value.(fullTextGenerationOutcome)
	if !ok {
		return "", primaryModel, fmt.Errorf("invalid generated chat response")
	}
	return outcome.Answer, outcome.UsedModel, nil
}
