package filetx

import "testing"

func TestIsVoiceOnlyForTheFormatsTelegramPlaysAsAVoiceMessage(t *testing.T) {
	for name, want := range map[string]bool{
		"hello.ogg": true, "hello.OGG": true, "hello.oga": true, "hello.opus": true, "hello.mp3": true, "hello.m4a": true, "a b.M4A": true,
		"song.flac": false, "clip.wav": false, "clip.aac": false, "video.mp4": false, "shot.png": false, "notes.txt": false,
		"ogg": false, "noext": false, ".ogg": true, "": false,
	} {
		if got := IsVoice(name); got != want {
			t.Errorf("IsVoice(%q) = %v, want %v", name, got, want)
		}
	}
}
