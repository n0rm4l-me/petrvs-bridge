package slack

import "github.com/n0rm4l-me/petrvs-bridge/internal/telegram"

func telegramMessage(text string, markdown bool) telegram.Message {
	return telegram.Message{Text: text, Markdown: markdown}
}
