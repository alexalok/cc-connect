package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestResponseLinks(t *testing.T) {
	tests := []struct {
		name, text string
		want       []responseAttachment
	}{
		{"normal", "Here is [report](/tmp/report.pdf).", []responseAttachment{{"/tmp/report.pdf", false}}},
		{"image", "![chart](/tmp/chart.png)", []responseAttachment{{"/tmp/chart.png", true}}},
		{"spaces", "[report](</tmp/my report (v2).pdf>)", []responseAttachment{{"/tmp/my report (v2).pdf", false}}},
		{"encoded", "[report](/tmp/my%20report.pdf)", []responseAttachment{{"/tmp/my report.pdf", false}}},
		{"parens", "[report](/tmp/report(v2).pdf)", []responseAttachment{{"/tmp/report(v2).pdf", false}}},
		{"escape", `[report](/tmp/report\(v2\).pdf)`, []responseAttachment{{"/tmp/report(v2).pdf", false}}},
		{"relative", `[report](output/report.pdf "Download")`, []responseAttachment{{"/workspace/output/report.pdf", false}}},
		{"file_url", "[report](file:///tmp/report.pdf)", []responseAttachment{{"/tmp/report.pdf", false}}},
		{"deduplicate", "[a](/tmp/report.pdf) [b](/tmp/./report.pdf)", []responseAttachment{{"/tmp/report.pdf", false}}},
		{"examples", "```md\n[x](/tmp/secret)\n```\n~~~\n[x](/tmp/secret)\n~~~\n`[x](/tmp/secret)`\n    [x](/tmp/secret)\n> [x](/tmp/secret)", nil},
		{"open_fence", "```\n[x](/tmp/secret)", nil},
		{"source_citations", "[a](/tmp/a.go:12) [b](/tmp/b.go#L2) [c](/tmp/c.go):3 [d](/tmp/d.go:2:3)", nil},
		{"urls", "[a](https://example.com/a.pdf) [b](file://host/tmp/a.pdf) [c](//host/tmp/a.pdf) [d](sandbox:/mnt/data/a.pdf)", nil},
		{"bare_paths", "Created /tmp/a.pdf and `output/b.pdf`", nil},
		{"unclosed", "[x](/tmp/a.pdf", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := responseLinks(tt.text, "/workspace"); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v want %#v", got, tt.want)
			}
		})
	}
	if got := responseLinks("[a](relative.pdf)", ""); len(got) != 0 {
		t.Fatal("relative link guessed a working directory")
	}
}

type responseFileDelivery struct {
	reply any
	file  FileAttachment
	image bool
}
type responseFilePlatform struct {
	stubPlatformEngine
	attachmentMu sync.Mutex
	deliveries   []responseFileDelivery
	attempts     int
	failAt       int
}

func (p *responseFilePlatform) SendFile(_ context.Context, reply any, file FileAttachment) error {
	p.attachmentMu.Lock()
	defer p.attachmentMu.Unlock()
	p.attempts++
	if p.attempts == p.failAt {
		return errors.New("temporary upload failure")
	}
	p.deliveries = append(p.deliveries, responseFileDelivery{reply: reply, file: file})
	return nil
}
func (p *responseFilePlatform) SendImage(_ context.Context, reply any, img ImageAttachment) error {
	p.attachmentMu.Lock()
	defer p.attachmentMu.Unlock()
	p.deliveries = append(p.deliveries, responseFileDelivery{reply: reply, file: FileAttachment{Data: img.Data, FileName: img.FileName}, image: true})
	return nil
}
func (p *responseFilePlatform) attachments() []responseFileDelivery {
	p.attachmentMu.Lock()
	defer p.attachmentMu.Unlock()
	return append([]responseFileDelivery(nil), p.deliveries...)
}

func TestResponseAttachments_BoundsRoutingAndOptIn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(path, []byte("report"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &responseFilePlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("project", &stubAgent{}, []Platform{p}, "", LangEnglish)
	defer e.Stop()
	text := fmt.Sprintf("[report](%s)", path)
	e.deliverResponseAttachments(context.Background(), p, "topic-a", text, dir)
	if len(p.attachments()) != 0 {
		t.Fatal("uploaded without opt-in")
	}
	e.SetResponseAttachments(true, 10)
	e.deliverResponseAttachments(context.Background(), p, "topic-a", text+" "+text, dir)
	if got := p.attachments(); len(got) != 1 || got[0].reply != "topic-a" || string(got[0].file.Data) != "report" {
		t.Fatalf("wrong delivery: %#v", got)
	}
	e.SetResponseAttachments(true, 3)
	e.deliverResponseAttachments(context.Background(), p, "topic-b", text, dir)
	e.deliverResponseAttachments(context.Background(), p, "topic-b", "[dir]("+dir+")", dir)
	e.deliverResponseAttachments(context.Background(), p, "topic-b", "[missing]("+dir+"/missing)", dir)
	if len(p.attachments()) != 1 {
		t.Fatal("invalid file uploaded")
	}
	if len(p.getSent()) != 3 {
		t.Fatalf("expected visible failures: %v", p.getSent())
	}
	e.SetAttachmentSendEnabled(false)
	e.SetResponseAttachments(true, 10)
	e.deliverResponseAttachments(context.Background(), p, "topic-a", text, dir)
	if len(p.attachments()) != 1 {
		t.Fatal("attachment_send=off ignored")
	}
}

func TestResponseAttachments_InlineImageAndDocument(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plot.png")
	if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &responseFilePlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("project", &stubAgent{}, []Platform{p}, "", LangEnglish)
	defer e.Stop()
	e.SetResponseAttachments(true, 1024)
	e.deliverResponseAttachments(context.Background(), p, "topic", "![plot]("+path+")", dir)
	e.deliverResponseAttachments(context.Background(), p, "topic", "[download]("+path+")", dir)
	got := p.attachments()
	if len(got) != 2 || !got[0].image || got[1].image {
		t.Fatalf("wrong media routing: %#v", got)
	}
}

type attachmentTopicPlatform struct {
	topicMessagePlatform
	files responseFilePlatform
}

func (p *attachmentTopicPlatform) SendFile(ctx context.Context, reply any, file FileAttachment) error {
	return p.files.SendFile(ctx, reply, file)
}
func TestResponseAttachments_MirrorRetriesWithoutRepeatingTextOrFiles(t *testing.T) {
	dir := t.TempDir()
	var links []string
	for _, name := range []string{"first.txt", "second.txt"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		links = append(links, fmt.Sprintf("[%s](%s)", name, path))
	}
	a := &topicMessageAgent{topicDiscoveryAgent: topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "native"}}}, messages: []SessionMessage{{ID: "answer", Role: "assistant", Content: strings.Join(links, " ")}}}
	p := &attachmentTopicPlatform{topicMessagePlatform: topicMessagePlatform{topicSyncPlatform: topicSyncPlatform{target: "owner"}}}
	p.files.failAt = 2
	store := filepath.Join(dir, "sessions.json")
	e, w := newTopicSyncTest(t, a, p, store)
	e.SetResponseAttachments(true, 1024)
	if err := w.poll(context.Background()); err == nil {
		t.Fatal("expected upload failure")
	}
	if len(p.files.attachments()) != 1 {
		t.Fatal("first file not uploaded")
	}
	textCount := len(p.getSent())
	e2, w2 := newTopicSyncTest(t, a, p, store)
	e2.SetResponseAttachments(true, 1024)
	if err := w2.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w2.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.files.attachments(); len(got) != 2 || got[0].reply != p.keys[0] || got[1].reply != p.keys[0] {
		t.Fatalf("wrong/duplicate uploads: %#v", got)
	}
	if len(p.getSent()) != textCount {
		t.Fatal("replayed response text")
	}
}

func TestResponseAttachments_MirrorDoesNotUploadOldOrUserLinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.txt")
	_ = os.WriteFile(path, []byte("report"), 0600)
	a := &topicMessageAgent{topicDiscoveryAgent: topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "native"}}}, messages: []SessionMessage{{ID: "old", Role: "assistant", Content: "[old](" + path + ")"}}}
	p := &attachmentTopicPlatform{topicMessagePlatform: topicMessagePlatform{topicSyncPlatform: topicSyncPlatform{target: "owner"}}}
	e, w := newTopicSyncTest(t, a, p, filepath.Join(dir, "sessions.json"))
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.SetResponseAttachments(true, 1024)
	a.messages = append(a.messages, SessionMessage{ID: "user", Role: "user", Content: "[user](" + path + ")"})
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.files.attachments()) != 0 {
		t.Fatal("old or user message uploaded a file")
	}
	a.messages = append(a.messages, SessionMessage{ID: "new", Role: "assistant", Content: "[new](" + path + ")"})
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.files.attachments()) != 1 {
		t.Fatal("new response did not upload")
	}
}

type attachmentWorkDirAgent struct {
	topicMessageAgent
	dir      string
	provider string
}

func (a *attachmentWorkDirAgent) GetSessionWorkDir(_ context.Context, _ string, provider string) (string, error) {
	a.provider = provider
	return a.dir, nil
}
func TestResponseAttachments_MirrorUsesNativeWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte("native report"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &attachmentWorkDirAgent{topicMessageAgent: topicMessageAgent{topicDiscoveryAgent: topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "native"}}}}, dir: dir}
	p := &attachmentTopicPlatform{topicMessagePlatform: topicMessagePlatform{topicSyncPlatform: topicSyncPlatform{target: "owner"}}}
	e, w := newTopicSyncTest(t, a, p, filepath.Join(t.TempDir(), "sessions.json"))
	e.SetResponseAttachments(true, 1024)
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.sessions.GetOrCreateActive(p.keys[0]).SetActiveProvider("saved-provider")
	a.messages = []SessionMessage{{ID: "answer", Role: "assistant", Content: "[report](report.txt)"}}
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := p.files.attachments()
	if len(got) != 1 || string(got[0].file.Data) != "native report" || a.provider != "saved-provider" {
		t.Fatalf("wrong native context: %#v, provider %s", got, a.provider)
	}
}

func TestResponseAttachments_MissingMirrorFileDoesNotBlockLaterMessages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(path, []byte("report"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &topicMessageAgent{topicDiscoveryAgent: topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "native"}}}, messages: []SessionMessage{
		{ID: "missing", Role: "assistant", Content: "[missing](" + dir + "/missing.txt)"},
		{ID: "next", Role: "assistant", Content: "[report](" + path + ")"},
	}}
	p := &attachmentTopicPlatform{topicMessagePlatform: topicMessagePlatform{topicSyncPlatform: topicSyncPlatform{target: "owner"}}}
	store := filepath.Join(dir, "sessions.json")
	e, w := newTopicSyncTest(t, a, p, store)
	e.SetResponseAttachments(true, 1024)
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	textCount := len(p.getSent())
	e2, w2 := newTopicSyncTest(t, a, p, store)
	e2.SetResponseAttachments(true, 1024)
	if err := w2.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.files.attachments()) != 1 || len(p.getSent()) != textCount {
		t.Fatal("retry duplicated warning or blocked valid attachment")
	}
}

func TestResponseAttachments_ConcurrentConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(path, []byte("report"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &responseFilePlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("project", &stubAgent{}, []Platform{p}, "", LangEnglish)
	defer e.Stop()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			e.SetResponseAttachments(i%2 == 0, 1024)
			e.SetAttachmentSendEnabled(i%2 == 0)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			e.deliverResponseAttachments(context.Background(), p, "topic", "[report]("+path+")", dir)
		}
	}()
	wg.Wait()
}

func TestResponseAttachments_RenderPreservesExamplesAndNativeText(t *testing.T) {
	raw := "Download [report](</tmp/My Report.pdf>). See [site](https://example.com).\n`[report](</tmp/My Report.pdf>)`\n```md\n[report](</tmp/My Report.pdf>)\n```"
	rendered := renderResponseAttachmentLinks(raw)
	if !strings.HasPrefix(rendered, "Download report (` /tmp/My Report.pdf `). See [site](https://example.com).") {
		t.Fatalf("bad rendered response: %s", rendered)
	}
	if !strings.Contains(rendered, "`[report](</tmp/My Report.pdf>)`") || !strings.HasSuffix(rendered, "```md\n[report](</tmp/My Report.pdf>)\n```") {
		t.Fatal("rewrote an example")
	}
	if got := responseLinks(raw, ""); len(got) != 1 || got[0].path != "/tmp/My Report.pdf" {
		t.Fatal("native link changed")
	}
}

func TestResponseAttachments_EnablingDoesNotRechunkOldMessages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(path, []byte("report"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &topicMessageAgent{topicDiscoveryAgent: topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "native"}}}}
	p := &attachmentTopicPlatform{topicMessagePlatform: topicMessagePlatform{topicSyncPlatform: topicSyncPlatform{target: "owner"}}}
	e, w := newTopicSyncTest(t, a, p, filepath.Join(dir, "sessions.json"))
	// Enough links near a chunk boundary that changing their Markdown would
	// change how many chunks the historical message needs.
	a.messages = []SessionMessage{{ID: "old", Role: "assistant", Content: strings.Repeat("[file]("+path+") ", 100)}}
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	textCount := len(p.getSent())
	e.SetResponseAttachments(true, 1024)
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.getSent()) != textCount || len(p.files.attachments()) != 0 {
		t.Fatal("enabling attachments replayed historical output")
	}
}

func TestResponseAttachments_MirrorKeepsRenderingWhenUploadsDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.txt")
	body := strings.Repeat("[report]("+path+" \"A long descriptive title for the generated report\") ", 120)
	a := &topicMessageAgent{topicDiscoveryAgent: topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "native"}}}, messages: []SessionMessage{{ID: "answer", Role: "assistant", Content: body}}}
	p := &attachmentTopicPlatform{topicMessagePlatform: topicMessagePlatform{topicSyncPlatform: topicSyncPlatform{target: "owner"}, failAt: 2}}
	store := filepath.Join(dir, "sessions.json")
	e, w := newTopicSyncTest(t, a, p, store)
	e.SetResponseAttachments(true, 1024)
	e.SetAttachmentSendEnabled(false)
	if err := w.poll(context.Background()); err == nil {
		t.Fatal("expected partial text failure")
	}
	e2, w2 := newTopicSyncTest(t, a, p, store)
	e2.SetResponseAttachments(true, 1024)
	e2.SetAttachmentSendEnabled(false)
	if err := w2.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w2.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	var sent []string
	for _, text := range p.getSent() {
		if !strings.HasPrefix(text, "topic:") {
			sent = append(sent, text)
		}
	}
	want := w2.formatMirroredMessage(SessionMessage{Role: "assistant", Content: renderResponseAttachmentLinks(body)})
	if strings.Join(sent, "") != strings.Join(SplitMessageCodeFenceAware(want, maxPlatformMessageLen), "") {
		t.Fatal("retry changed rendered text boundaries")
	}
	if len(p.files.attachments()) != 0 {
		t.Fatal("uploaded with attachment_send=off")
	}
}

func TestResponseLinks_WindowsDriveTargets(t *testing.T) {
	for _, target := range []string{"C:/Users/alice/report.pdf", `C:\Users\alice\report.pdf`} {
		got := responseLinks("[report]("+target+")", "")
		if len(got) != 1 || got[0].path != filepath.Clean(target) {
			t.Fatalf("ignored native drive path %q: %#v", target, got)
		}
	}
}

func TestResponseLinks_NestedListsAndTheirCodeBlocks(t *testing.T) {
	text := "1. Artifacts:\n    - [report](report.pdf)\n      ![plot](plot.png)\n\n          [example](example.pdf)\n\n2. Another:\n   - ```md\n     [code](code.pdf)\n     ```\n   - [second](second.pdf)\n"
	got := responseLinks(text, "/workspace")
	want := []responseAttachment{{"/workspace/report.pdf", false}, {"/workspace/plot.png", true}, {"/workspace/second.pdf", false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nested list links: got %#v want %#v", got, want)
	}
}
