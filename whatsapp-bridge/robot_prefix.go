package main

import (
	"bufio"
	"os"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// Robot prefix: the chats listed in robotPrefixChatsPath see every message
// this bridge sends through /api/send start with "🤖: ", so a reader can tell
// an automated message from a human one, also in the phone's notification
// summary. The list is read on every send, so an edit needs no restart.
//
// One JID per line, in the form the bridge stores the chat under
// (group@g.us, or phone@s.whatsapp.net). Blank lines and lines that start
// with "#" are ignored. A missing file means no chat is prefixed.
//
// Scope: text messages and the captions of image, video and document
// messages. A voice message has no caption and is sent as is. Reactions,
// read receipts and group-admin calls do not pass through here.
const robotPrefixChatsPath = "store/robot-prefix-chats.txt"

const (
	robotMark   = "🤖"
	robotPrefix = robotMark + ": "
)

// robotPrefixChats reads the list file. A read error yields an empty set:
// a send never fails because of this list.
func robotPrefixChats(path string) map[string]bool {
	chats := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return chats
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		chats[line] = true
	}
	return chats
}

// withRobotPrefix returns the text the chat will see. It adds the prefix
// when any of chatJIDs is listed in listPath and the text does not already
// start with the robot mark. A media message with no caption gets the mark
// alone as its caption.
func withRobotPrefix(listPath string, message string, isMediaCaption bool, chatJIDs ...string) string {
	if strings.HasPrefix(message, robotMark) {
		return message
	}
	if message == "" && !isMediaCaption {
		return message
	}
	chats := robotPrefixChats(listPath)
	listed := false
	for _, jid := range chatJIDs {
		if chats[jid] {
			listed = true
			break
		}
	}
	if !listed {
		return message
	}
	if message == "" {
		return robotMark
	}
	return robotPrefix + message
}

// outboundText applies the robot prefix to one /api/send call. lookupJID is
// the recipient as the caller gave it; an @lid recipient is also matched by
// its phone JID, the form the chat is stored under. A voice message has no
// caption, so its text is returned unchanged.
func outboundText(client *whatsmeow.Client, listPath string, lookupJID types.JID, message, mediaPath string) string {
	isMedia := mediaPath != ""
	if isMedia {
		if mediaType, _, _ := classifyMediaPath(mediaPath); mediaType == whatsmeow.MediaAudio {
			return message
		}
	}
	return withRobotPrefix(listPath, message, isMedia,
		lookupJID.ToNonAD().String(),
		resolveUserJID(client, lookupJID, types.EmptyJID).String())
}
