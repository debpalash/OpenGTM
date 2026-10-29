package outreachsend

// SendErrorText is the operator-facing text of a failed send, classified exactly as the send handler stores it:
// a server reply verbatim ("550 5.1.1 ..."), a transport failure as a fixed phrase that carries no address,
// banner or credential.
func SendErrorText(err error) string {
	text, _ := classifySendError(err)
	return text
}
