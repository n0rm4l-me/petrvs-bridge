package telegram

import "context"

// FileRef names a file that Telegram holds: a photo or a document that the user sent to the bot.
type FileRef struct {
	ID    string // the file_id to fetch it with
	Name  string // the file name as sent (documents); empty for photos
	Mime  string
	Size  int64 // 0 when Telegram did not say
	Photo bool  // sent as a photo (which Telegram recompresses) rather than as a document
}

// OutFile is a file to upload to the chat.
type OutFile struct {
	Name    string
	Data    []byte
	Caption string // the client cuts it to what Telegram allows (1024 characters)
	Photo   bool   // show it as a photo if Telegram can; otherwise it goes as a document
	Buttons [][]Button
}

// FileSender is implemented by clients that can upload a file to the chat; the hub uses it when it is there.
type FileSender interface {
	// SendFile returns the id Telegram gave to the message.
	SendFile(ctx context.Context, chatID int64, f OutFile) ([]int64, error)
}

// FileFetcher is implemented by clients that can download a file the user sent; the hub uses it when it is there.
type FileFetcher interface {
	// Fetch downloads the file and refuses more than maxBytes (also when Telegram did not say how big it is).
	Fetch(ctx context.Context, f FileRef, maxBytes int64) ([]byte, error)
}

// MarkupEditor is implemented by clients that can take the buttons off a message the bot sent.
type MarkupEditor interface {
	ClearButtons(ctx context.Context, chatID, messageID int64) error
}
