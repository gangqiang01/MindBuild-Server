package redmine

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// renderIssue turns a Redmine issue into a self-contained Markdown document.
// The structured metadata is collected up front so retrieval can filter by
// project/tracker/status and so the LLM can answer "show me resolved auth
// bugs" without re-reading every body. Structural labels are English on
// purpose: the rendered content is stored once per data source and read by
// users across locales, while the values (tracker, status, names) arrive from
// Redmine in whatever language that instance uses.
func renderIssue(iss *issue, baseURL, projectID string, includeJournals bool, images map[int]string) (title, body string) {
	title = fmt.Sprintf("[#%d] %s", iss.ID, iss.Subject)
	var b strings.Builder
	fmt.Fprintf(&b, "# [%s #%d] %s\n\n", iss.Tracker.Name, iss.ID, iss.Subject)

	var meta []string
	if iss.Project.Name != "" {
		meta = append(meta, "Project: "+iss.Project.Name)
	}
	if iss.Tracker.Name != "" {
		meta = append(meta, "Tracker: "+iss.Tracker.Name)
	}
	if iss.Status.Name != "" {
		meta = append(meta, "Status: "+iss.Status.Name)
	}
	if iss.Priority.Name != "" {
		meta = append(meta, "Priority: "+iss.Priority.Name)
	}
	if iss.FixedVersion != nil && iss.FixedVersion.Name != "" {
		meta = append(meta, "Version: "+iss.FixedVersion.Name)
	}
	if iss.Category != nil && iss.Category.Name != "" {
		meta = append(meta, "Category: "+iss.Category.Name)
	}
	if len(meta) > 0 {
		b.WriteString(strings.Join(meta, " | ") + "\n")
	}

	var people []string
	if iss.Author.Name != "" {
		people = append(people, "Author: "+iss.Author.Name)
	}
	if iss.AssignedTo != nil && iss.AssignedTo.Name != "" {
		people = append(people, "Assigned to: "+iss.AssignedTo.Name)
	}
	people = append(people, "Created: "+isoTime(iss.CreatedOn))
	if u := isoTime(iss.UpdatedOn); u != "" {
		people = append(people, "Updated: "+u)
	}
	b.WriteString(strings.Join(people, " | ") + "\n")

	if len(iss.CustomFields) > 0 {
		var cf []string
		for _, f := range iss.CustomFields {
			if strings.TrimSpace(f.Value) != "" {
				cf = append(cf, f.Name+"="+f.Value)
			}
		}
		if len(cf) > 0 {
			b.WriteString("Custom fields: " + strings.Join(cf, ", ") + "\n")
		}
	}
	b.WriteString("Link: " + issueURL(baseURL, iss.ID) + "\n\n")

	b.WriteString("## Description\n\n")
	b.WriteString(normalizeBody(iss.Description, "textile"))
	b.WriteString("\n\n")

	if includeJournals && len(iss.Journals) > 0 {
		b.WriteString("## Discussion\n\n")
		for _, j := range iss.Journals {
			ts := isoTime(j.CreatedOn)
			wrote := false
			if strings.TrimSpace(j.Notes) != "" {
				fmt.Fprintf(&b, "- %s %s: %s\n", ts, j.User.Name, singleLine(j.Notes))
				wrote = true
			}
			if changes := summarizeDetails(j.Details); changes != "" {
				if wrote {
					fmt.Fprintf(&b, "  - %s %s: %s\n", ts, j.User.Name, changes)
				} else {
					fmt.Fprintf(&b, "- %s %s: %s\n", ts, j.User.Name, changes)
				}
			}
		}
		b.WriteString("\n")
	}

	if len(iss.Relations) > 0 {
		b.WriteString("## Relations\n\n")
		for _, r := range iss.Relations {
			fmt.Fprintf(&b, "- %s #%d -> #%d\n", relationLabel(r.RelationType), r.IssueID, r.IssueToID)
		}
		b.WriteString("\n")
	}

	if len(iss.Attachments) > 0 {
		// Split image attachments (inlined as base64 data URIs so screenshots
		// render when the bug is viewed) from other files (linked). Redmine
		// attachment URLs need the API key, which the KB viewer does not have,
		// so remote image URLs would not load — inlining the bytes avoids that.
		var imgs, files []attachment
		for _, a := range iss.Attachments {
			if strings.HasPrefix(a.ContentType, "image/") {
				imgs = append(imgs, a)
			} else {
				files = append(files, a)
			}
		}
		if len(imgs) > 0 {
			b.WriteString("## Images\n\n")
			for _, a := range imgs {
				src, ok := images[a.ID]
				if !ok || src == "" {
					// Too large or download failed: fall back to a link. Use the
					// configured base_url (rewritten), not Redmine's internal host,
					// so the link is reachable from the frontend.
					src = rewriteAttachmentURL(a.ContentURL, baseURL)
				}
				fmt.Fprintf(&b, "![%s](%s)\n\n", a.Filename, src)
			}
		}
		if len(files) > 0 {
			b.WriteString("## Attachments\n\n")
			for _, a := range files {
				desc := strings.TrimSpace(a.Description)
				if desc != "" {
					desc = " — " + desc
				}
				fmt.Fprintf(&b, "- [%s](%s) (%s, %s)%s\n", a.Filename, rewriteAttachmentURL(a.ContentURL, baseURL), humanSize(a.Size), a.ContentType, desc)
			}
			b.WriteString("\n")
		}
	}

	return title, b.String()
}

// renderWikiPage turns a Redmine wiki page into Markdown. The body is
// normalized from its stored format (textile/markdown/html) so the KB indexes
// one consistent format.
func renderWikiPage(p *wikiPageDetail, baseURL, projectID string) (title, body string) {
	title = p.Title
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", p.Title)
	var meta []string
	meta = append(meta, "Project: "+projectID)
	meta = append(meta, "Page: "+p.Title)
	meta = append(meta, "Version: "+strconv.Itoa(p.Version))
	if u := isoTime(p.UpdatedOn); u != "" {
		meta = append(meta, "Updated: "+u)
	}
	b.WriteString(strings.Join(meta, " | ") + "\n")
	b.WriteString("Link: " + wikiURL(baseURL, projectID, p.Title) + "\n\n")
	b.WriteString(normalizeBody(p.Text, p.Format))
	b.WriteString("\n")

	if len(p.Attachments) > 0 {
		b.WriteString("\n## Attachments\n\n")
		for _, a := range p.Attachments {
			fmt.Fprintf(&b, "- [%s](%s) (%s, %s)\n", a.Filename, rewriteAttachmentURL(a.ContentURL, baseURL), humanSize(a.Size), a.ContentType)
		}
	}
	return title, b.String()
}

// normalizeBody converts a Redmine text body to Markdown according to its
// format. Markdown/common_mark passes through; textile gets a best-effort
// conversion of the constructs an LLM benefits from (headings, code blocks,
// inline emphasis, links); html passes through since Markdown embeds raw HTML.
// Anything unconverted is left as-is: the model can still read raw Textile.
func normalizeBody(text, format string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "_(no description)_"
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "markdown", "common_mark", "md":
		return text
	case "textile":
		return textileToMarkdown(text)
	case "html", "":
		return text
	default:
		return text
	}
}

// summarizeDetails renders a journal's field-change details as one line, e.g.
// "status New -> In Progress, done_ratio 0 -> 100". Empty when there are no
// details (a pure comment journal).
func summarizeDetails(details []journalDetail) string {
	var parts []string
	for _, d := range details {
		name := detailLabel(d)
		if d.OldValue == "" && d.NewValue == "" {
			continue
		}
		if d.OldValue == "" {
			parts = append(parts, fmt.Sprintf("%s set to %s", name, d.NewValue))
		} else {
			parts = append(parts, fmt.Sprintf("%s %s -> %s", name, d.OldValue, d.NewValue))
		}
	}
	return strings.Join(parts, ", ")
}

func detailLabel(d journalDetail) string {
	switch d.Property {
	case "attr":
		return d.Name
	case "relation":
		return "relation"
	case "attachment":
		return "attachment"
	default:
		return d.Name
	}
}

func relationLabel(t string) string {
	switch strings.ToLower(t) {
	case "relates":
		return "relates to"
	case "duplicates":
		return "duplicates"
	case "duplicated":
		return "duplicated by"
	case "blocks":
		return "blocks"
	case "blocked":
		return "blocked by"
	case "precedes":
		return "precedes"
	case "follows":
		return "follows"
	case "copied_to":
		return "copied to"
	case "copied_from":
		return "copied from"
	default:
		return t
	}
}

func humanSize(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := unit, 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func singleLine(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	// Collapse runs of spaces so a multi-paragraph note stays one bullet.
	re := regexp.MustCompile(`[ \t]+`)
	return re.ReplaceAllString(s, " ")
}

// issueURL / wikiURL build human-facing links back to the Redmine instance.
func issueURL(base string, id int) string {
	return strings.TrimRight(base, "/") + "/issues/" + strconv.Itoa(id)
}

func wikiURL(base, projectID, title string) string {
	return strings.TrimRight(base, "/") + "/projects/" + projectID + "/wiki/" + urlPathEscape(title)
}

// urlPathEscape percent-encodes a wiki title for a URL path segment. Spaces
// become %20 (Redmine accepts both %20 and +; %20 is unambiguous).
func urlPathEscape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s) * 3)
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// ---- Textile -> Markdown (best effort) ----
// Redmine stores wiki/issue bodies as Textile unless the instance is configured
// for Markdown. A full Textile parser is out of scope; this converts the
// constructs that matter for retrieval (headings, code blocks, inline emphasis,
// links, blockquotes) and leaves the rest readable as raw Textile.

var (
	textileHeadingRE = regexp.MustCompile(`(?m)^h([1-6])\.\s+(.*)$`)
	textileQuoteRE   = regexp.MustCompile(`(?m)^bq\.\s+(.*)$`)
	textileCodeRE    = regexp.MustCompile(`(?ms)^bc\.\s+(.*)$`)
	textilePreRE     = regexp.MustCompile(`(?ms)^pre\.\s+(.*)$`)
	textileLinkRE    = regexp.MustCompile(`"([^"]+)":((?:https?|ftp)://[^\s]+)`)
	textileBoldRE    = regexp.MustCompile(`\*([^\s*][^*]*?)\*`)
	textileItalicRE  = regexp.MustCompile(`_([^\s_][^_]*?)_`)
	textileImageRE   = regexp.MustCompile(`!((?:https?://|/)[^\s!]+)!`)
)

func textileToMarkdown(text string) string {
	// Strip notextile wrappers but keep their content.
	text = strings.ReplaceAll(text, "<notextile>", "")
	text = strings.ReplaceAll(text, "</notextile>", "")

	// Block-level: convert one fenced code block / pre / blockquote at a time.
	// bc. and pre. run to the next blank line in Textile; the regexes above are
	// single-line by default but (?s) lets . match newlines for the body.
	text = textileCodeRE.ReplaceAllStringFunc(text, func(m string) string {
		sub := textileCodeRE.FindStringSubmatch(m)
		return "```\n" + strings.TrimSpace(sub[1]) + "\n```"
	})
	text = textilePreRE.ReplaceAllStringFunc(text, func(m string) string {
		sub := textilePreRE.FindStringSubmatch(m)
		return "```\n" + strings.TrimSpace(sub[1]) + "\n```"
	})
	text = textileQuoteRE.ReplaceAllStringFunc(text, func(m string) string {
		sub := textileQuoteRE.FindStringSubmatch(m)
		return "> " + sub[1]
	})
	text = textileHeadingRE.ReplaceAllStringFunc(text, func(m string) string {
		sub := textileHeadingRE.FindStringSubmatch(m)
		n := sub[1]
		return strings.Repeat("#", parseLen(n)) + " " + sub[2]
	})

	// Inline.
	text = textileLinkRE.ReplaceAllString(text, "[$1]($2)")
	text = textileImageRE.ReplaceAllString(text, "![]($1)")
	text = textileBoldRE.ReplaceAllString(text, "**$1**")
	text = textileItalicRE.ReplaceAllString(text, "*$1*")

	return text
}

func parseLen(s string) int {
	n, _ := strconv.Atoi(s)
	if n < 1 {
		n = 1
	}
	return n
}
