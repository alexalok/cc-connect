package core

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type responseAttachment struct {
	path  string
	image bool
}

// Together with the per-file size limit, this bounds successful uploads to
// ten times that limit per response, as well as bounding failed-path notices.
const maxResponseAttachments = 10

func boundedResponseLinks(text, workDir string) ([]responseAttachment, bool) {
	links := responseLinks(text, workDir)
	if len(links) > maxResponseAttachments {
		return links[:maxResponseAttachments], true
	}
	return links, false
}

// SetResponseAttachments enables delivery at the bridge boundary. It does not
// add instructions, tools, or routing information to the agent's prompt.
func (e *Engine) SetResponseAttachments(enabled bool, maxBytes int64, artifactRoots ...string) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxAttachmentSize
	}
	e.responseAttachmentLimit.Store(maxBytes)
	roots := append([]string(nil), artifactRoots...)
	e.responseAttachmentRoots.Store(&roots)
	e.responseAttachments.Store(enabled)
}

func responseWorkDir(session AgentSession, agent Agent, fallback string) string {
	dir := ""
	if wd, ok := session.(interface{ GetWorkDir() string }); ok {
		dir = wd.GetWorkDir()
	}
	if dir == "" {
		dir = fallback
	}
	if dir == "" {
		if wd, ok := agent.(interface{ GetWorkDir() string }); ok {
			dir = wd.GetWorkDir()
		}
	}
	if dir == "" {
		return ""
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	return absolute
}

// responseLinks only treats Markdown links as attachment intent. In particular,
// tool output, bare paths, inline code, fenced code, URLs, and source-line
// citations must never cause uploads. Selection is independent of file existence
// so mirror delivery offsets remain stable across retries.
func responseLinks(text, workDir string) []responseAttachment {
	return scanResponseLinks(text, workDir, nil)
}

// Syntax-only scans never access this path. Windows requires a drive-qualified
// root before relative targets can be recognized; the actual cwd is used later.
func responseLinkSyntaxRoot() string {
	if filepath.Separator == '\\' {
		return `C:\`
	}
	return "/"
}

func scanResponseLinks(text, workDir string, visit func(start, end int, label, target string)) []responseAttachment {
	var out []responseAttachment
	seen := map[string]bool{}
	fence := byte(0)
	fenceLen := 0
	fenceIndent := 0
	var listIndents []int
	inlineTicks := 0
	offset := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		lineStart := offset
		offset += len(line)
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if fence != 0 {
			n := 0
			for n < len(trimmed) && trimmed[n] == fence {
				n++
			}
			if indent >= fenceIndent && indent-fenceIndent <= 3 && n >= fenceLen && strings.TrimSpace(trimmed[n:]) == "" {
				fence = 0
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(trimmed, ">") || strings.HasPrefix(line, "\t") {
			continue
		}
		for len(listIndents) > 0 && indent < listIndents[len(listIndents)-1] {
			listIndents = listIndents[:len(listIndents)-1]
		}
		containerIndent := 0
		if len(listIndents) > 0 {
			containerIndent = listIndents[len(listIndents)-1]
		}
		if indent-containerIndent >= 4 {
			continue
		}
		if width := responseListMarkerWidth(trimmed); width > 0 {
			containerIndent = indent + width
			listIndents = append(listIndents, containerIndent)
			trimmed = trimmed[width:]
		}
		if len(trimmed) > 0 && (trimmed[0] == '`' || trimmed[0] == '~') && inlineTicks == 0 {
			n := 0
			for n < len(trimmed) && trimmed[n] == trimmed[0] {
				n++
			}
			if n >= 3 {
				fence = trimmed[0]
				fenceLen = n
				fenceIndent = containerIndent
				continue
			}
		}
		for i := 0; i < len(line); {
			if line[i] == '\\' && inlineTicks == 0 && i+1 < len(line) {
				i += 2
				continue
			}
			if line[i] == '`' {
				n := 1
				for i+n < len(line) && line[i+n] == '`' {
					n++
				}
				if inlineTicks == 0 {
					inlineTicks = n
				} else if inlineTicks == n {
					inlineTicks = 0
				}
				i += n
				continue
			}
			if inlineTicks != 0 || line[i] != '[' {
				i++
				continue
			}
			start := i
			// Balance nested brackets in labels, including escaped punctuation.
			depth := 1
			i++
			for i < len(line) && depth > 0 {
				if line[i] == '\\' && i+1 < len(line) {
					i += 2
					continue
				}
				if line[i] == '[' {
					depth++
				}
				if line[i] == ']' {
					depth--
				}
				i++
			}
			if depth != 0 || i >= len(line) || line[i] != '(' {
				continue
			}
			label := line[start+1 : i-1]
			target, end, ok := responseLinkTarget(line, i+1)
			if !ok {
				i++
				continue
			}
			i = end
			// Older Codex responses put :line just outside the Markdown link.
			if i+1 < len(line) && line[i] == ':' && line[i+1] >= '0' && line[i+1] <= '9' {
				continue
			}
			path, ok := responseLinkPath(target, workDir)
			if !ok {
				continue
			}
			image := start > 0 && line[start-1] == '!'
			if visit != nil {
				rawStart := start
				if image {
					rawStart--
				}
				visit(lineStart+rawStart, lineStart+end, label, target)
			}
			if seen[path] {
				continue
			}
			seen[path] = true
			out = append(out, responseAttachment{path: path, image: start > 0 && line[start-1] == '!'})
		}
	}
	return out
}

// Return the content offset of a CommonMark bullet/ordered list marker.
// Indentation beyond a list's content column is still checked for code blocks.
func responseListMarkerWidth(line string) int {
	if len(line) < 2 {
		return 0
	}
	i := 0
	if line[0] == '-' || line[0] == '+' || line[0] == '*' {
		i = 1
	} else {
		for i < len(line) && i < 9 && line[i] >= '0' && line[i] <= '9' {
			i++
		}
		if i == 0 || i >= len(line) || (line[i] != '.' && line[i] != ')') {
			return 0
		}
		i++
	}
	if i >= len(line) || line[i] != ' ' {
		return 0
	}
	end := i
	for end < len(line) && line[end] == ' ' {
		end++
	}
	if end-i > 4 {
		return i + 1
	}
	return end
}

// Render local artifact links as readable labels/paths instead of URLs that
// messaging platforms cannot open. This only changes the delivered view.
func renderResponseAttachmentLinks(text string) string {
	var out strings.Builder
	end := 0
	scanResponseLinks(text, responseLinkSyntaxRoot(), func(start, next int, label, target string) {
		out.WriteString(text[end:start])
		out.WriteString(label)
		out.WriteString(" (")
		ticks := "`"
		for strings.Contains(target, ticks) {
			ticks += "`"
		}
		out.WriteString(ticks + " " + target + " " + ticks)
		out.WriteString(")")
		end = next
	})
	if end == 0 {
		return text
	}
	out.WriteString(text[end:])
	return out.String()
}

func responseLinkTarget(line string, i int) (string, int, bool) {
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	start := i
	if i >= len(line) {
		return "", i, false
	}
	if line[i] == '<' {
		i++
		start = i
		for i < len(line) && line[i] != '>' {
			i++
		}
		if i == len(line) {
			return "", i, false
		}
		target := line[start:i]
		i++
		end, ok := responseLinkEnd(line, i)
		return target, end, ok
	}
	depth := 0
	for i < len(line) {
		c := line[i]
		if c == '\\' && i+1 < len(line) {
			i += 2
			continue
		}
		if c == '(' {
			depth++
		}
		if c == ')' {
			if depth == 0 {
				return line[start:i], i + 1, i > start
			}
			depth--
		}
		if (c == ' ' || c == '\t') && depth == 0 {
			end, ok := responseLinkEnd(line, i)
			return line[start:i], end, ok && i > start
		}
		if c == '\n' {
			break
		}
		i++
	}
	return "", i, false
}

func responseLinkEnd(line string, i int) (int, bool) {
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i < len(line) && (line[i] == '"' || line[i] == '\'') {
		quote := line[i]
		i++
		for i < len(line) && line[i] != quote {
			if line[i] == '\\' && i+1 < len(line) {
				i++
			}
			i++
		}
		if i == len(line) {
			return i, false
		}
		i++
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
	}
	return i + 1, i < len(line) && line[i] == ')'
}

func responseLinkPath(target, workDir string) (string, bool) {
	// Markdown escapes only punctuation, not arbitrary characters in filenames.
	var b strings.Builder
	for i := 0; i < len(target); i++ {
		if target[i] == '\\' && i+1 < len(target) && strings.ContainsRune("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", rune(target[i+1])) {
			i++
		}
		b.WriteByte(target[i])
	}
	target = b.String()
	drive := ""
	if len(target) >= 3 && ((target[0] >= 'A' && target[0] <= 'Z') || (target[0] >= 'a' && target[0] <= 'z')) && target[1] == ':' && (target[2] == '/' || target[2] == '\\') {
		drive, target = target[:2], target[2:]
	}
	u, err := url.Parse(target)
	if err != nil || u.Host != "" || u.RawQuery != "" || u.Fragment != "" || strings.HasPrefix(target, "//") {
		return "", false
	}
	if u.Scheme != "" && u.Scheme != "file" {
		return "", false
	}
	path := drive + u.Path
	if path == "" || strings.ContainsAny(path, "\x00\r\n") {
		return "", false
	}
	if reHashLocation.MatchString(path) || reColonLineOnly.MatchString(path) || reColonLineCol.MatchString(path) || reColonLineRange.MatchString(path) {
		return "", false
	}
	if drive == "" && !filepath.IsAbs(path) {
		if workDir == "" || !filepath.IsAbs(workDir) {
			return "", false
		}
		path = filepath.Join(workDir, path)
	}
	return filepath.Clean(path), true
}

func (e *Engine) deliverResponseAttachments(ctx context.Context, p Platform, reply any, text, workDir string) {
	if !e.responseAttachments.Load() || !e.attachmentSendEnabled.Load() {
		return
	}
	attachments, truncated := boundedResponseLinks(text, workDir)
	var failed []string
	for _, attachment := range attachments {
		if ctx.Err() != nil {
			return
		}
		if _, err := e.sendResponseAttachment(ctx, p, reply, attachment, workDir); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("response attachment failed", "path", attachment.path, "error", err)
			failed = append(failed, filepath.Base(attachment.path))
		}
	}
	if len(failed) > 0 {
		e.send(p, reply, e.i18n.Tf(MsgResponseAttachmentFailed, strings.Join(failed, ", ")))
	}
	if truncated {
		e.send(p, reply, e.i18n.Tf(MsgResponseAttachmentLimit, maxResponseAttachments))
	}
}

// The boolean distinguishes local failures (missing/oversized/unsupported files)
// from a delivery failure that the mirror should retry without replaying text.
func (e *Engine) sendResponseAttachment(ctx context.Context, p Platform, reply any, attachment responseAttachment, workDir string) (bool, error) {
	if !filepath.IsAbs(attachment.path) {
		return false, fmt.Errorf("attachment path is not absolute on this host")
	}
	limit := e.responseAttachmentLimit.Load()
	if limit <= 0 {
		limit = DefaultMaxAttachmentSize
	}
	f, err := e.openResponseAttachment(attachment.path, workDir, limit)
	if err != nil {
		return false, err
	}
	defer f.Close()
	// Check the opened file too; a replacement during open must not bypass the
	// size limit. LimitReader also bounds growth while reading.
	opened, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !opened.Mode().IsRegular() || opened.Size() > limit {
		return false, fmt.Errorf("invalid attachment file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return false, err
	}
	if int64(len(data)) > limit {
		return false, fmt.Errorf("file exceeds attachment size limit")
	}
	mime := http.DetectContentType(data)
	name := filepath.Base(attachment.path)
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if err := e.waitOutgoing(p); err != nil {
		return true, err
	}
	if attachment.image && strings.HasPrefix(mime, "image/") {
		if sender, ok := p.(ImageSender); ok {
			return true, sender.SendImage(ctx, reply, ImageAttachment{Data: data, FileName: name, MimeType: mime})
		}
	}
	if sender, ok := p.(FileSender); ok {
		return true, sender.SendFile(ctx, reply, FileAttachment{Data: data, FileName: name, MimeType: mime})
	}
	return false, ErrNotSupported
}

// Root operations enforce containment while resolving paths, including symlink
// replacements between Stat and Open. The workspace and operator-configured
// artifact directories are the only allowed roots; never guess the bridge cwd.
func (e *Engine) openResponseAttachment(path, workDir string, limit int64) (*os.File, error) {
	roots := []string{workDir}
	if configured := e.responseAttachmentRoots.Load(); configured != nil {
		roots = append(roots, (*configured)...)
	}
	var lastErr error
	for _, dir := range roots {
		if !filepath.IsAbs(dir) {
			continue
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || !filepath.IsLocal(rel) {
			continue
		}
		root, err := openResponseRoot(dir)
		if err != nil {
			lastErr = err
			continue
		}
		info, err := root.Stat(rel)
		if err == nil && (!info.Mode().IsRegular() || info.Size() > limit) {
			err = fmt.Errorf("invalid attachment file")
		}
		var f *os.File
		if err == nil {
			f, err = root.Open(rel)
		}
		root.Close()
		if err == nil {
			return f, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("attachment is outside the workspace and configured artifact directories")
}

// Walk from the volume root through pinned directory handles. Lstat plus an
// identity check on each opened directory rejects symlink components and swaps
// between inspection and open. The caller uses the final handle for file
// inspection and opening; the opened file then pins subsequent reads.
func openResponseRoot(dir string) (*os.Root, error) {
	dir = filepath.Clean(dir)
	anchor := filepath.VolumeName(dir) + string(filepath.Separator)
	current, err := os.OpenRoot(anchor)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(anchor, dir)
	if err != nil {
		current.Close()
		return nil, err
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		info, err := current.Lstat(part)
		if err != nil {
			current.Close()
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			current.Close()
			return nil, fmt.Errorf("attachment root contains a non-directory or symlink")
		}
		next, err := current.OpenRoot(part)
		current.Close()
		if err != nil {
			return nil, err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			next.Close()
			return nil, fmt.Errorf("attachment root changed during access")
		}
		current = next
	}
	return current, nil
}
