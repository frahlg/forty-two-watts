package notifications

import (
	"fmt"
	"regexp"
	"strings"
)

// RenderPush fills a catalogue sentence's {arg} placeholders from the event's
// own facts. It refuses to render a sentence it cannot finish: a lock screen
// showing a literal "{kwh}" is a promise the catalogue made that the event
// broke, and silence is better than gibberish.
func RenderPush(kind string, args map[string]string) (title, body string, err error) {
	sentence, ok := PushSentences[kind]
	if !ok {
		return "", "", fmt.Errorf("notifications: %q is not in the push catalogue", kind)
	}
	title = fillPlaceholders(sentence.Title, args)
	body = fillPlaceholders(sentence.Body, args)
	if missing := placeholderPattern.FindString(title + " " + body); missing != "" {
		return "", "", fmt.Errorf("notifications: %s left %s unfilled", kind, missing)
	}
	return title, body, nil
}

var placeholderPattern = regexp.MustCompile(`\{[a-z_]+\}`)

func fillPlaceholders(s string, args map[string]string) string {
	for k, v := range args {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}
