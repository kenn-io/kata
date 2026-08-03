package markdownrender

// CodeBlockBackground returns the shared ANSI-256 code-block background for
// the terminal theme.
func CodeBlockBackground(isDark bool) string {
	if isDark {
		return "236"
	}
	return "252"
}
