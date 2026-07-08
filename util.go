package main

import "io"

// readSnippet reads up to 2 KiB from r for inclusion in error messages.
func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 2048))
	return string(b)
}
