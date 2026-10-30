package wa

import (
	"testing"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/rodrigovmoreira/Squamata-whatsapp/internal/sessions"
)

func TestExtractContent(t *testing.T) {
	cases := map[string]struct {
		msg      *waE2E.Message
		wantKind kind
		wantText string
		wantMime string
		wantName string
	}{
		"texto simples": {
			msg:      &waE2E.Message{Conversation: proto.String("bom dia")},
			wantKind: kindText, wantText: "bom dia",
		},
		"texto estendido": {
			msg: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String("com link https://x"),
			}},
			wantKind: kindText, wantText: "com link https://x",
		},
		"imagem com legenda": {
			msg: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
				Caption:    proto.String("segue a foto"),
				Mimetype:   proto.String("image/jpeg"),
				FileLength: proto.Uint64(48213),
			}},
			wantKind: kindImage, wantText: "segue a foto", wantMime: "image/jpeg", wantName: "imagem.jpg",
		},
		"imagem sem legenda": {
			msg: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
				Mimetype: proto.String("image/png"),
			}},
			wantKind: kindImage, wantMime: "image/png", wantName: "imagem.png",
		},
		"audio ptt": {
			msg: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
				Mimetype: proto.String("audio/ogg; codecs=opus"),
			}},
			wantKind: kindAudio, wantMime: "audio/ogg; codecs=opus", wantName: "audio.ogg",
		},
		"video": {
			msg: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
				Caption:  proto.String("olha isso"),
				Mimetype: proto.String("video/mp4"),
			}},
			wantKind: kindVideo, wantText: "olha isso", wantMime: "video/mp4", wantName: "video.mp4",
		},
		"documento": {
			msg: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
				FileName: proto.String("orcamento.pdf"),
				Mimetype: proto.String("application/pdf"),
			}},
			wantKind: kindDocument, wantMime: "application/pdf", wantName: "orcamento.pdf",
		},
		"sticker e outros": {
			msg:      &waE2E.Message{StickerMessage: &waE2E.StickerMessage{}},
			wantKind: kindOther,
		},
		"mensagem nula": {
			msg:      nil,
			wantKind: kindOther,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := extractContent(tc.msg)
			if got.Kind != tc.wantKind {
				t.Errorf("kind = %q, esperado %q", got.Kind, tc.wantKind)
			}
			if got.Text != tc.wantText {
				t.Errorf("text = %q, esperado %q", got.Text, tc.wantText)
			}
			if got.Mime != tc.wantMime {
				t.Errorf("mime = %q, esperado %q", got.Mime, tc.wantMime)
			}
			if got.Filename != tc.wantName {
				t.Errorf("filename = %q, esperado %q", got.Filename, tc.wantName)
			}
		})
	}
}

func TestExtractContentKeepsAudioSeconds(t *testing.T) {
	got := extractContent(&waE2E.Message{AudioMessage: &waE2E.AudioMessage{
		Mimetype: proto.String("audio/ogg"),
		Seconds:  proto.Uint32(17),
	}})
	if got.Seconds == nil || *got.Seconds != 17 {
		t.Fatalf("seconds = %v, esperado 17", got.Seconds)
	}
}

func TestShouldIgnore(t *testing.T) {
	dm := types.MessageInfo{MessageSource: types.MessageSource{
		Chat:   types.NewJID("5511987654321", types.DefaultUserServer),
		Sender: types.NewJID("5511987654321", types.DefaultUserServer),
	}}

	group := dm
	group.IsGroup = true
	group.Chat = types.NewJID("120363335026718801", types.GroupServer)

	broadcast := dm
	broadcast.Chat = types.NewJID("status", types.BroadcastServer)

	newsletter := dm
	newsletter.Chat = types.NewJID("120363335026718801", types.NewsletterServer)

	longID := dm
	longID.Sender = types.NewJID("120363335026718801", types.DefaultUserServer)

	cases := map[string]struct {
		info types.MessageInfo
		want bool
	}{
		"conversa privada":       {dm, false},
		"grupo":                  {group, true},
		"status broadcast":       {broadcast, true},
		"canal/newsletter":       {newsletter, true},
		"id técnico muito longo": {longID, true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := shouldIgnore(tc.info, sessions.DefaultPolicy()); got != tc.want {
				t.Errorf("shouldIgnore = %v, esperado %v", got, tc.want)
			}
		})
	}
}

// A política é por sessão: quem quiser grupos/canais recebe.
func TestShouldIgnoreHonorsPolicy(t *testing.T) {
	group := types.MessageInfo{MessageSource: types.MessageSource{
		Chat:    types.NewJID("120363335026718801", types.GroupServer),
		Sender:  types.NewJID("5511987654321", types.DefaultUserServer),
		IsGroup: true,
	}}
	newsletter := types.MessageInfo{MessageSource: types.MessageSource{
		Chat: types.NewJID("120363335026718801", types.NewsletterServer),
	}}

	open := sessions.Policy{ReceiveGroups: true, ReceiveNewsletters: true, MediaProfile: string(sessions.MediaProfileChat)}
	if shouldIgnore(group, open) {
		t.Error("grupo deveria passar com ReceiveGroups=true")
	}
	if shouldIgnore(newsletter, open) {
		t.Error("canal deveria passar com ReceiveNewsletters=true")
	}
	if !shouldIgnore(group, sessions.DefaultPolicy()) {
		t.Error("grupo deveria ser ignorado com a política padrão")
	}
}

func TestExtensionFor(t *testing.T) {
	cases := map[string]string{
		"image/jpeg":             ".jpg",
		"image/png":              ".png",
		"image/webp":             ".webp",
		"audio/ogg; codecs=opus": ".ogg",
		"video/mp4":              ".mp4",
		"application/pdf":        "",
	}
	for mime, want := range cases {
		if got := extensionFor(mime); got != want {
			t.Errorf("extensionFor(%q) = %q, esperado %q", mime, got, want)
		}
	}
}
