package notify

// Send dispatches a desktop notification with the given title and message.
// The implementation is platform-specific. On unsupported platforms this is a no-op.
func Send(title, message string) error {
	return send(title, message)
}
