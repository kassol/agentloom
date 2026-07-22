package feishugw

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	channelnormalize "github.com/larksuite/oapi-sdk-go/v3/channel/normalize"
	channeltypes "github.com/larksuite/oapi-sdk-go/v3/channel/types"

	"github.com/yan5xu/codex-loom/internal/hub"
)

const (
	feishuRichTextPlaceholder = "[rich text message]"
	feishuUnsupportedMessage  = "[unsupported message]"
)

func applyFeishuPostContent(params *hub.IngressParams, raw, source string) {
	if params == nil {
		return
	}
	text, attachments, format, err := normalizeFeishuPostContent(raw)
	if err != nil {
		replaceFallback := !usableFeishuContent(params.Content.Text)
		if !replaceFallback && source == "message-details" && strings.TrimSpace(raw) != "" {
			replaceFallback = feishuNormalizationIsFallback(params)
		}
		if replaceFallback {
			params.Content.Text = feishuContentFallback("post", raw, err)
			setFeishuContentNormalization(params, map[string]any{
				"status": "fallback", "format": "post", "source": source, "error": err.Error(),
			})
		}
		return
	}
	params.Content.Text = text
	params.Content.Attachments = mergeFeishuAttachments(params.Content.Attachments, attachments)
	setFeishuContentNormalization(params, map[string]any{
		"status": "normalized", "format": format, "source": source,
	})
}

func normalizeFeishuPostContent(raw string) (string, []hub.AttachmentRef, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil, "", fmt.Errorf("native content is empty")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return "", nil, "", fmt.Errorf("native content is invalid JSON: %w", err)
	}
	body, format, ok := selectFeishuPostBody(payload)
	if !ok {
		return "", nil, "", fmt.Errorf("native content has no post content or content_v2 paragraphs")
	}

	// The SDK channel normalizer expects a locale wrapper, while messages.get
	// and current receive events can return the post body directly.
	wrapped, err := json.Marshal(map[string]any{"zh_cn": body})
	if err != nil {
		return "", nil, "", fmt.Errorf("wrap native post content: %w", err)
	}
	text, resources := channelnormalize.ParseContent("post", string(wrapped))
	text = strings.TrimSpace(text)
	if !usableFeishuContent(text) {
		return "", nil, "", fmt.Errorf("post paragraphs contain no readable content")
	}
	return text, feishuResourceAttachments(resources), format, nil
}

func selectFeishuPostBody(payload map[string]any) (map[string]any, string, bool) {
	if hasFeishuPostParagraphs(payload) {
		return payload, "direct-post", true
	}
	for _, locale := range []string{"zh_cn", "en_us", "ja_jp"} {
		if body, ok := payload[locale].(map[string]any); ok && hasFeishuPostParagraphs(body) {
			return body, "localized-post:" + locale, true
		}
	}
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if body, ok := payload[key].(map[string]any); ok && hasFeishuPostParagraphs(body) {
			return body, "localized-post:" + key, true
		}
	}
	return nil, "", false
}

func hasFeishuPostParagraphs(body map[string]any) bool {
	if body == nil {
		return false
	}
	if _, ok := body["content_v2"].([]any); ok {
		return true
	}
	_, ok := body["content"].([]any)
	return ok
}

func usableFeishuContent(text string) bool {
	switch strings.TrimSpace(text) {
	case "", feishuRichTextPlaceholder, feishuUnsupportedMessage:
		return false
	default:
		return true
	}
}

func feishuResourceAttachments(resources []channeltypes.Resource) []hub.AttachmentRef {
	attachments := make([]hub.AttachmentRef, 0, len(resources))
	for _, resource := range resources {
		if strings.TrimSpace(resource.FileKey) == "" {
			continue
		}
		attachments = append(attachments, hub.AttachmentRef{
			ID: resource.FileKey, Name: resource.FileName, MimeType: resourceMimeType(resource.Type),
		})
	}
	return attachments
}

func mergeFeishuAttachments(existing, incoming []hub.AttachmentRef) []hub.AttachmentRef {
	result := append([]hub.AttachmentRef(nil), existing...)
	seen := make(map[string]int, len(result))
	for index, attachment := range result {
		seen[feishuAttachmentKey(attachment)] = index
	}
	for _, attachment := range incoming {
		key := feishuAttachmentKey(attachment)
		if index, ok := seen[key]; ok {
			if result[index].Name == "" && attachment.Name != "" {
				result[index].Name = attachment.Name
			}
			if (result[index].MimeType == "" || result[index].MimeType == "application/octet-stream") && attachment.MimeType != "" {
				result[index].MimeType = attachment.MimeType
			}
			continue
		}
		seen[key] = len(result)
		result = append(result, attachment)
	}
	return result
}

func feishuAttachmentKey(attachment hub.AttachmentRef) string {
	if attachment.ID != "" {
		return "id:" + attachment.ID
	}
	return "name:" + attachment.Name + "\x00" + attachment.MimeType
}

func feishuContentFallback(msgType, raw string, err error) string {
	reason := "unknown normalization error"
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		reason = strings.TrimSpace(err.Error())
	}
	return fmt.Sprintf("[Feishu %s normalization failed: %s]\n\n<feishu_native_content format=\"json\">\n%s\n</feishu_native_content>", msgType, reason, strings.TrimSpace(raw))
}

func setFeishuContentNormalization(params *hub.IngressParams, value map[string]any) {
	if params.ProviderMetadata == nil {
		params.ProviderMetadata = map[string]any{}
	}
	params.ProviderMetadata["contentNormalization"] = value
}

func feishuNormalizationIsFallback(params *hub.IngressParams) bool {
	if params == nil || params.ProviderMetadata == nil {
		return false
	}
	value, _ := params.ProviderMetadata["contentNormalization"].(map[string]any)
	status, _ := value["status"].(string)
	return status == "fallback"
}
