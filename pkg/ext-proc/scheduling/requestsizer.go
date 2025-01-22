package scheduling

import "unicode"

// RequestSizer defines an interface for estimating the size of a request.
type RequestSizer interface {
		Size(text string) uint64
}

// WordCountSizer estimates request size based on the number of words.
type WordCountSizer struct{}

// Size implements the RequestSizer interface for WordCountSizer.
func (s WordCountSizer) Size(text string) uint64 {
		count := uint64(1) // Start with 1 to account for empty strings
		inWord := false
		for _, r := range text {
				if unicode.IsSpace(r) {
						inWord = false
				} else if !inWord {
						count++
						inWord = true
				}
		}
		return count
}
