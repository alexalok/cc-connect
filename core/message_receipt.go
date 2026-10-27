package core

import "sync"

// sendAgentMessage keeps delivery acknowledgments separate from bridge acceptance.
// Send returning nil alone does not prove the agent received the input.
func sendAgentMessage(session AgentSession, prompt, messageID string, images []ImageAttachment, files []FileAttachment, onDelivered func()) error {
	if sender, ok := session.(ReceiptAwareSession); ok && onDelivered != nil {
		return sender.SendWithReceipt(prompt, messageID, images, files, sync.OnceFunc(onDelivered))
	}
	return session.Send(prompt, messageID, images, files)
}
