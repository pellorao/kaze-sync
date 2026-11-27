package main

import (
	"fmt"
	"html"
	"io"
	"regexp"
	"strings"
)

// notice is one line of the bell (the notification popover of every page); a folded line (many alike) has the
// notices it folds in Items.
type notice struct {
	Level    string   `json:"level"` // error or warning
	Group    string   `json:"group,omitempty"`
	FolderID string   `json:"folder_id,omitempty"`
	Text     string   `json:"text"`
	Meta     string   `json:"meta,omitempty"`
	Link     string   `json:"link,omitempty"`
	LinkText string   `json:"link_text,omitempty"`
	Advice   string   `json:"advice,omitempty"`
	Count    int      `json:"count"` // how many notices the line stands for (the bell counts each)
	Items    []notice `json:"items,omitempty"`
}

var (
	noticeTokRe = regexp.MustCompile(`(?s)<div class="grp-h"[^>]*>(.*?)</div>|<details class="nitem fold">(.*?)</details>|<div class="nitem">(.*?)</div>`)
	groupLinkRe = regexp.MustCompile(`<a href="/folders/(\d+)">`)
	cntRe       = regexp.MustCompile(`(?s)<span class="cnt.*?</span>`)
	levelRe     = regexp.MustCompile(`class="ic (err|warn)"`)
	msgRe       = regexp.MustCompile(`(?s)<span class="msg">(.*?)</span>`)
	metaRe      = regexp.MustCompile(`(?s)<span class="meta">(.*?)</span>`)
	advRe       = regexp.MustCompile(`(?s)<span class="adv">(.*?)</span>`)
	linkRe      = regexp.MustCompile(`(?s)<a class="path" href="([^"]*)">(.*?)</a>`)
	subItemRe   = regexp.MustCompile(`(?s)<div class="nitem">(.*?)</div>`)
	tagRe       = regexp.MustCompile(`(?s)<[^>]*>`)
)

// plain is the text of a fragment of the page.
func plain(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(tagRe.ReplaceAllString(s, ""))), " ")
}

func firstGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

func parseNoticeItem(s string) notice {
	n := notice{Level: "warning", Count: 1, Text: plain(firstGroup(msgRe, s)), Meta: plain(firstGroup(metaRe, s)), Advice: plain(firstGroup(advRe, s))}
	if firstGroup(levelRe, s) == "err" {
		n.Level = "error"
	}
	if m := linkRe.FindStringSubmatch(s); m != nil {
		n.Link, n.LinkText = html.UnescapeString(m[1]), plain(m[2])
	}
	return n
}

// parseNotices reads the bell out of a page: the popover that every page of a session carries.
func parseNotices(page string) []notice {
	i := strings.Index(page, `id="notif"`)
	if i < 0 {
		return nil
	}
	sec := page[i:]
	if j := strings.Index(sec, `class="notif-f"`); j >= 0 {
		sec = sec[:j]
	}
	var out []notice
	group, folder := "", ""
	for _, m := range noticeTokRe.FindAllStringSubmatch(sec, -1) {
		switch {
		case strings.HasPrefix(m[0], `<div class="grp-h"`):
			group, folder = plain(cntRe.ReplaceAllString(m[1], "")), firstGroup(groupLinkRe, m[1])
		case strings.HasPrefix(m[0], "<details"):
			summary, body, _ := strings.Cut(m[2], "</summary>")
			n := parseNoticeItem(summary)
			for _, sm := range subItemRe.FindAllStringSubmatch(body, -1) {
				n.Items = append(n.Items, parseNoticeItem(sm[1]))
			}
			n.Count = len(n.Items)
			n.Group, n.FolderID = group, folder
			out = append(out, n)
		default:
			n := parseNoticeItem(m[3])
			n.Group, n.FolderID = group, folder
			out = append(out, n)
		}
	}
	return out
}

func printNotices(w io.Writer, ns []notice) {
	var e, wn int
	for _, n := range ns {
		if n.Level == "error" {
			e += n.Count
		} else {
			wn += n.Count
		}
	}
	fmt.Fprintf(w, "bell: %d error(s), %d warning(s)\n", e, wn)
	for _, n := range ns {
		line := fmt.Sprintf("  %-7s ", n.Level)
		if n.Group != "" {
			line += "[" + n.Group + "] "
		}
		line += n.Text
		if n.Count > 1 {
			line += fmt.Sprintf(" (%d)", n.Count)
		}
		if n.Link != "" {
			line += " -> " + n.Link
		}
		fmt.Fprintln(w, line)
	}
}
