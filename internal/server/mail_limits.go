package server

import (
	"fmt"
	"os"
	"strconv"
)

const maxSupportedMessage = 40 << 20

type mailSizeLimits struct {
	message, attachments int
}

func loadMailSizeLimits() (mailSizeLimits, error) {
	var sizes mailSizeLimits
	for _, setting := range []struct {
		name          string
		limit         *int
		fallback, max int
	}{
		{"HARBOR_MAX_MESSAGE_MIB", &sizes.message, mailLimit >> 20, maxSupportedMessage >> 20},
		{"HARBOR_MAX_ATTACHMENTS_MIB", &sizes.attachments, attachmentLimit >> 20, 28},
	} {
		value := setting.fallback
		if input := os.Getenv(setting.name); input != "" {
			parsed, err := strconv.Atoi(input)
			if err != nil || parsed < 1 || parsed > setting.max {
				return sizes, fmt.Errorf("%s must be a whole number from 1 to %d", setting.name, setting.max)
			}
			value = parsed
		}
		*setting.limit = value << 20
	}
	// Base64 and MIME line folding consume approximately 37% extra space.
	if sizes.attachments*138/100+(512<<10) > sizes.message {
		return sizes, fmt.Errorf("HARBOR_MAX_MESSAGE_MIB must allow for HARBOR_MAX_ATTACHMENTS_MIB plus MIME overhead")
	}
	return sizes, nil
}

func (a *app) messageLimit() int {
	if a.sizes.message != 0 {
		return a.sizes.message
	}
	return mailLimit
}

func (a *app) attachmentLimit() int {
	if a.sizes.attachments != 0 {
		return a.sizes.attachments
	}
	return attachmentLimit
}

func (a *app) oversizedMessage() string {
	return fmt.Sprintf("编码后的邮件超过 %d MiB，请减少附件或正文", a.messageLimit()>>20)
}
