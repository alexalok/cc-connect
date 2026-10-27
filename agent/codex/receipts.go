package codex

import "github.com/chenhg5/cc-connect/core"

var _ core.ReceiptAwareSession = (*appServerSession)(nil)

// Send waits for a successful turn/start response containing a turn ID.
// Only that acknowledgment warrants marking the original input as received.
func (s *appServerSession) SendWithReceipt(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment, onDelivered func()) error {
	if err := s.Send(prompt, messageID, images, files); err != nil {
		return err
	}
	if onDelivered != nil {
		onDelivered()
	}
	return nil
}
