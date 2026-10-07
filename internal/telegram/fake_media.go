package telegram

import (
	"context"
	"errors"
	"sync"
)

// The file parts of Fake live apart from the rest, with their own lock, since they are only used by tests of the file
// transfer and the buttons.
type fakeMedia struct {
	mu        sync.Mutex
	uploaded  []OutFile
	fetchable map[string][]byte
	fetchErr  error
	fileErr   error
	cleared   []int64
	clearErr  error
}

func (f *Fake) media() *fakeMedia {
	f.files.mu.Lock()
	if f.files.fetchable == nil {
		f.files.fetchable = map[string][]byte{}
	}
	f.files.mu.Unlock()
	return &f.files
}

// SendFile records an upload and returns the id the fake gave the message.
func (f *Fake) SendFile(_ context.Context, chatID int64, o OutFile) ([]int64, error) {
	m := f.media()
	m.mu.Lock()
	if m.fileErr != nil {
		err := m.fileErr
		m.mu.Unlock()
		return nil, err
	}
	m.uploaded = append(m.uploaded, OutFile{Name: o.Name, Data: append([]byte(nil), o.Data...), Caption: o.Caption, Photo: o.Photo, Buttons: o.Buttons})
	m.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextMsg++
	return []int64{1000 + f.nextMsg}, nil
}

// Uploads returns the files sent to the chat so far.
func (f *Fake) Uploads() []OutFile {
	m := f.media()
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]OutFile(nil), m.uploaded...)
}

// SetFileErr makes SendFile fail with err until it is called again with nil.
func (f *Fake) SetFileErr(err error) {
	m := f.media()
	m.mu.Lock()
	m.fileErr = err
	m.mu.Unlock()
}

// Serve makes a file that the user "sent" available to Fetch under its id.
func (f *Fake) Serve(id string, data []byte) {
	m := f.media()
	m.mu.Lock()
	m.fetchable[id] = append([]byte(nil), data...)
	m.mu.Unlock()
}

// SetFetchErr makes Fetch fail with err until it is called again with nil.
func (f *Fake) SetFetchErr(err error) {
	m := f.media()
	m.mu.Lock()
	m.fetchErr = err
	m.mu.Unlock()
}

// Fetch returns a served file, refusing more than maxBytes like the real client does.
func (f *Fake) Fetch(_ context.Context, ref FileRef, maxBytes int64) ([]byte, error) {
	m := f.media()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fetchErr != nil {
		return nil, m.fetchErr
	}
	data, ok := m.fetchable[ref.ID]
	if !ok {
		return nil, errors.New("fake telegram: no such file")
	}
	if ref.Size > maxBytes || int64(len(data)) > maxBytes {
		return nil, errors.New("fake telegram: file too large")
	}
	return append([]byte(nil), data...), nil
}

// ClearButtons records that the buttons were taken off a message.
func (f *Fake) ClearButtons(_ context.Context, chatID, messageID int64) error {
	m := f.media()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.clearErr != nil {
		return m.clearErr
	}
	m.cleared = append(m.cleared, messageID)
	return nil
}

// Cleared returns the ids of the messages whose buttons were taken off.
func (f *Fake) Cleared() []int64 {
	m := f.media()
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int64(nil), m.cleared...)
}

var (
	_ FileSender   = (*Fake)(nil)
	_ FileFetcher  = (*Fake)(nil)
	_ MarkupEditor = (*Fake)(nil)
)
