package internal

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

var nonAlphanumeric = regexp.MustCompile(`[^a-z0-9]+`)

func Slugify(name string) string {
	s := strings.ToLower(name)
	s = nonAlphanumeric.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}


func GenerateID() (string, error) {
    b := make([]byte, 4)
    if _, err := rand.Read(b); err != nil {
        return "", fmt.Errorf("cannot generate id: %w", err)
    }
    return hex.EncodeToString(b), nil
}


func SanitizeAxisValue(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), "_")
	s = reStripInvalid.ReplaceAllString(s, "")
	s = reCollapseUnder.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	return s
}
