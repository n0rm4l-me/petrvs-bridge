package telegram

import "context"

// ClearButtons takes the buttons off a message the bot sent, so that a prompt cannot be answered twice from the same
// message. It does not wait out a rate limit: the hub ignores the result, and the agent refuses a second answer anyway.
func (c *HTTP) ClearButtons(ctx context.Context, chatID, messageID int64) error {
	_, err := c.call(ctx, "editMessageReplyMarkup", map[string]any{
		"chat_id": chatID, "message_id": messageID,
		"reply_markup": map[string]any{"inline_keyboard": [][]map[string]string{}},
	}, sendTimeout)
	return err
}

var _ MarkupEditor = (*HTTP)(nil)
