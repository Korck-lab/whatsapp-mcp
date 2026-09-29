package main

import (
	"os"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

const directorGroup = "120363430884533529@g.us"

func writeRobotList(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "robot-prefix-chats.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRobotPrefix_ListedChatTextGetsPrefix(t *testing.T) {
	list := writeRobotList(t, directorGroup+"\n")
	got := withRobotPrefix(list, "Teste do prefixo", false, directorGroup)
	if got != "🤖: Teste do prefixo" {
		t.Fatalf("got %q", got)
	}
}

func TestRobotPrefix_UnlistedChatIsUnchanged(t *testing.T) {
	list := writeRobotList(t, directorGroup+"\n")
	got := withRobotPrefix(list, "Olá", false, "5541999999999@s.whatsapp.net")
	if got != "Olá" {
		t.Fatalf("got %q", got)
	}
}

func TestRobotPrefix_MissingListFileIsUnchanged(t *testing.T) {
	got := withRobotPrefix(filepath.Join(t.TempDir(), "absent.txt"), "Olá", false, directorGroup)
	if got != "Olá" {
		t.Fatalf("got %q", got)
	}
}

func TestRobotPrefix_AlreadyPrefixedIsNotPrefixedAgain(t *testing.T) {
	list := writeRobotList(t, directorGroup+"\n")
	for _, in := range []string{"🤖: pronto", "🤖 pronto", "🤖"} {
		if got := withRobotPrefix(list, in, false, directorGroup); got != in {
			t.Errorf("withRobotPrefix(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestRobotPrefix_MediaWithoutCaptionGetsRobotAlone(t *testing.T) {
	list := writeRobotList(t, directorGroup+"\n")
	if got := withRobotPrefix(list, "", true, directorGroup); got != "🤖" {
		t.Fatalf("got %q, want the robot alone", got)
	}
	if got := withRobotPrefix(list, "legenda", true, directorGroup); got != "🤖: legenda" {
		t.Fatalf("got %q", got)
	}
}

func TestRobotPrefix_ListIsReadAtSendTime(t *testing.T) {
	list := writeRobotList(t, "")
	if got := withRobotPrefix(list, "a", false, directorGroup); got != "a" {
		t.Fatalf("before the edit: got %q", got)
	}
	if err := os.WriteFile(list, []byte("# robot prefix chats\n\n  "+directorGroup+"  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := withRobotPrefix(list, "a", false, directorGroup); got != "🤖: a" {
		t.Fatalf("after the edit: got %q", got)
	}
}

func TestRobotPrefix_AnyOfTheChatFormsMatches(t *testing.T) {
	list := writeRobotList(t, "5541999999999@s.whatsapp.net\n")
	got := withRobotPrefix(list, "oi", false, "5541999999999@lid", "5541999999999@s.whatsapp.net")
	if got != "🤖: oi" {
		t.Fatalf("got %q", got)
	}
}

func TestOutboundText_VoiceMessageIsNotPrefixed(t *testing.T) {
	list := writeRobotList(t, directorGroup+"\n")
	jid, _ := parseRecipientForTest(t, directorGroup)
	client := newTestClient(&mockLIDStore{})
	if got := outboundText(client, list, jid, "", "/tmp/voice.ogg"); got != "" {
		t.Fatalf("voice: got %q, want unchanged", got)
	}
	if got := outboundText(client, list, jid, "", "/tmp/photo.jpg"); got != "🤖" {
		t.Fatalf("photo: got %q", got)
	}
	if got := outboundText(client, list, jid, "pronto", ""); got != "🤖: pronto" {
		t.Fatalf("text: got %q", got)
	}
}

func TestOutboundText_LIDRecipientMatchesItsPhoneEntry(t *testing.T) {
	phone := types.JID{User: "5541999999999", Server: types.DefaultUserServer}
	lid := types.JID{User: "99887766", Server: types.HiddenUserServer}
	client := newTestClient(&mockLIDStore{
		lidByPN: map[types.JID]types.JID{phone: lid},
		pnByLID: map[types.JID]types.JID{lid: phone},
	})
	list := writeRobotList(t, phone.String()+"\n")
	if got := outboundText(client, list, lid, "oi", ""); got != "🤖: oi" {
		t.Fatalf("got %q", got)
	}
}

func parseRecipientForTest(t *testing.T, s string) (types.JID, error) {
	t.Helper()
	j, err := types.ParseJID(s)
	if err != nil {
		t.Fatal(err)
	}
	return j, nil
}
