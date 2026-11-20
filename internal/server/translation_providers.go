package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const microsoftTranslatorURL = "https://api.cognitive.microsofttranslator.com/translate"
const googleTranslationURL = "https://translation.googleapis.com/language/translate/v2"
const deepLFreeURL = "https://api-free.deepl.com/v2/translate"

type emailTranslator func(context.Context, string, []string) ([]string, int)

func loadEmailTranslation() (*emailTranslation, error) {
	providers := make(map[string]emailTranslator)
	client := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	azure, err := fileSetting("HARBOR_TRANSLATOR_KEY")
	if err != nil {
		return nil, err
	}
	region := strings.TrimSpace(os.Getenv("HARBOR_TRANSLATOR_REGION"))
	if azure = strings.TrimSpace(azure); azure != "" {
		if strings.ContainsAny(azure+region, "\r\n") {
			return nil, errors.New("invalid Microsoft Translator credentials")
		}
		providers["microsoft"] = microsoftEmailTranslator(client, azure, region)
	}
	deepL, err := fileSetting("HARBOR_DEEPL_FREE_KEY")
	if err != nil {
		return nil, err
	}
	if deepL = strings.TrimSpace(deepL); deepL != "" {
		if !strings.HasSuffix(deepL, ":fx") || strings.ContainsAny(deepL, "\r\n") {
			return nil, errors.New("HARBOR_DEEPL_FREE_KEY requires a DeepL API Free key")
		}
		providers["deepl"] = deepLEmailTranslator(client, deepL)
	}
	google, err := fileSetting("HARBOR_GOOGLE_TRANSLATE_KEY")
	if err != nil {
		return nil, err
	}
	if google = strings.TrimSpace(google); google != "" {
		if strings.ContainsAny(google, "\r\n") {
			return nil, errors.New("invalid Google Cloud Translation credentials")
		}
		providers["google"] = googleEmailTranslator(client, google)
	}
	if setting := strings.TrimSpace(os.Getenv("HARBOR_LIBRETRANSLATE_URL")); setting != "" {
		endpoint, err := url.Parse(setting)
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
			endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" {
			return nil, errors.New("HARBOR_LIBRETRANSLATE_URL must be an HTTP(S) base URL without credentials or query parameters")
		}
		key, err := fileSetting("HARBOR_LIBRETRANSLATE_KEY")
		if err != nil {
			return nil, err
		}
		providers["libretranslate"] = libreEmailTranslator(client, endpoint.JoinPath("translate").String(), strings.TrimSpace(key))
	}
	if len(providers) == 0 {
		return nil, nil
	}
	return &emailTranslation{providers: providers, slots: make(chan struct{}, 4)}, nil
}

func requestEmailTranslation(ctx context.Context, client *http.Client, endpoint string, headers http.Header, input, output any) int {
	payload, err := json.Marshal(input)
	if err != nil {
		return http.StatusBadGateway
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return http.StatusBadGateway
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return http.StatusBadGateway
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusTooManyRequests, 456: // DeepL uses 456 for an exhausted quota.
		return http.StatusTooManyRequests
	case http.StatusOK:
		const limit = 512 << 10
		result, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err == nil && len(result) <= limit && json.Unmarshal(result, output) == nil {
			return http.StatusOK
		}
	}
	return http.StatusBadGateway
}

func microsoftEmailTranslator(client *http.Client, key, region string) emailTranslator {
	headers := make(http.Header)
	headers.Set("Ocp-Apim-Subscription-Key", key)
	if region != "" {
		headers.Set("Ocp-Apim-Subscription-Region", region)
	}
	return func(ctx context.Context, target string, texts []string) ([]string, int) {
		input := make([]struct{ Text string }, len(texts))
		for i, text := range texts {
			input[i].Text = text
		}
		query := url.Values{"api-version": {"3.0"}, "to": {target}, "textType": {"plain"}}
		var output []struct {
			Translations []struct{ Text string }
		}
		status := requestEmailTranslation(ctx, client, microsoftTranslatorURL+"?"+query.Encode(), headers, input, &output)
		if status != http.StatusOK {
			return nil, status
		}
		result := make([]string, len(output))
		for i, item := range output {
			if len(item.Translations) != 1 {
				return nil, http.StatusBadGateway
			}
			result[i] = item.Translations[0].Text
		}
		return result, status
	}
}

func deepLEmailTranslator(client *http.Client, key string) emailTranslator {
	headers := http.Header{"Authorization": {"DeepL-Auth-Key " + key}}
	return func(ctx context.Context, target string, texts []string) ([]string, int) {
		lang := strings.ToUpper(target)
		if lang == "EN" {
			lang = "EN-US"
		}
		input := struct {
			Text     []string `json:"text"`
			Language string   `json:"target_lang"`
		}{texts, lang}
		var output struct{ Translations []struct{ Text string } }
		status := requestEmailTranslation(ctx, client, deepLFreeURL, headers, input, &output)
		result := make([]string, len(output.Translations))
		for i, item := range output.Translations {
			result[i] = item.Text
		}
		return result, status
	}
}

func libreEmailTranslator(client *http.Client, endpoint, key string) emailTranslator {
	return func(ctx context.Context, target string, texts []string) ([]string, int) {
		switch target {
		case "zh-Hans":
			target = "zh"
		case "zh-Hant":
			target = "zt"
		}
		input := struct {
			Text   []string `json:"q"`
			Source string   `json:"source"`
			Target string   `json:"target"`
			Key    string   `json:"api_key,omitempty"`
		}{texts, "auto", target, key}
		var output struct {
			Texts []string `json:"translatedText"`
		}
		status := requestEmailTranslation(ctx, client, endpoint, nil, input, &output)
		return output.Texts, status
	}
}

func googleEmailTranslator(client *http.Client, key string) emailTranslator {
	headers := make(http.Header)
	headers.Set("X-Goog-Api-Key", key)
	return func(ctx context.Context, target string, texts []string) ([]string, int) {
		switch target {
		case "zh-Hans":
			target = "zh-CN"
		case "zh-Hant":
			target = "zh-TW"
		}
		input := struct {
			Text   []string `json:"q"`
			Target string   `json:"target"`
			Format string   `json:"format"`
		}{texts, target, "text"}
		var output struct {
			Data struct {
				Translations []struct {
					Text string `json:"translatedText"`
				}
			}
		}
		status := requestEmailTranslation(ctx, client, googleTranslationURL, headers, input, &output)
		result := make([]string, len(output.Data.Translations))
		for i, item := range output.Data.Translations {
			result[i] = item.Text
		}
		return result, status
	}
}
