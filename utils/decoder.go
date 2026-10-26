package utils

import (
	"encoding/base64"
	"fmt"
	"html"
	"strings"
)

func DecodeAdLink(encodedStr string) (string, error) {
	if encodedStr == "" {
		return "", fmt.Errorf("empty string")
	}
	reversed := reverseString(encodedStr)

	decodedBytes, err := base64.StdEncoding.DecodeString(reversed)
	if err != nil {
		return "", err
	}

	htmlUnescaped := html.UnescapeString(string(decodedBytes))

	return htmlUnescaped, nil
}

func Base64Decode(input string) (string, error) {
	if input == "" {
		return "", fmt.Errorf("empty string")
	}
	decodedBytes, err := base64.StdEncoding.DecodeString(input)
	if err != nil {
		return "", err
	}
	return string(decodedBytes), nil
}

// Helper function to reverse a string
func reverseString(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

// unshuffleStringByStep reconstructs an original string from a shuffled version.
// The input was created by picking characters using a stepping cursor (advancing
// by step positions through the original, skipping already-used positions).
// This function inverts the process: it walks the same cursor sequence and places
// each shuffled[i] back into original[index].
func unshuffleStringByStep(shuffled string, step int) (string, error) {
	runes := []rune(shuffled)
	length := len(runes)
	if length == 0 {
		return "", fmt.Errorf("empty string")
	}
	if step <= 0 {
		return "", fmt.Errorf("step must be greater than 0")
	}

	original := make([]rune, length)
	used := make([]bool, length)
	index := 0

	for i := 0; i < length; i++ {
		for used[index] {
			index = (index + 1) % length
		}
		used[index] = true
		original[i] = runes[index]
		index = (index + step) % length
	}

	return string(original), nil
}

// DecodeStarckDataU decodes the obfuscated magnet link from the data-u attribute. This is indexer-specific.
func DecodeStarckDataU(dataU string) (string, error) {
	if dataU == "" {
		return "", fmt.Errorf("empty data-u value")
	}

	unshuffled, err := unshuffleStringByStep(dataU, 3)
	if err != nil {
		return "", fmt.Errorf("unshuffle failed: %w", err)
	}

	if !IsMagnetLink(unshuffled) {
		return "", fmt.Errorf("decoded string is not a valid magnet link: %s", unshuffled)
	}

	return unshuffled, nil
}

// DecodeStarckXID decodes the xid parameter of the starckfilmes download
// buttons (https://filmedl.com/?xid=...): the same step-3 shuffle as data-u,
// over the base64 of the magnet link. This is indexer-specific.
func DecodeStarckXID(xid string) (string, error) {
	if xid == "" {
		return "", fmt.Errorf("empty xid value")
	}

	unshuffled, err := unshuffleStringByStep(xid, 3)
	if err != nil {
		return "", fmt.Errorf("unshuffle failed: %w", err)
	}

	// the site drops the base64 padding
	unshuffled = strings.TrimRight(unshuffled, "=")
	decoded, err := base64.RawStdEncoding.DecodeString(unshuffled)
	if err != nil {
		decoded, err = base64.RawURLEncoding.DecodeString(unshuffled)
		if err != nil {
			return "", fmt.Errorf("base64 decode failed: %w", err)
		}
	}

	magnetLink := html.UnescapeString(strings.TrimSpace(string(decoded)))
	if !IsMagnetLink(magnetLink) {
		return "", fmt.Errorf("decoded string is not a valid magnet link: %.40q", magnetLink)
	}

	return magnetLink, nil
}

func IsMagnetLink(link string) bool {
	return len(link) > 8 && link[:8] == "magnet:?"
}
