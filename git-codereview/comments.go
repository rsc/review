// Copyright 2015 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Reading and replying to Gerrit comments.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type ChangeComments struct {
	ID   string
	Revs []*RevComments
}

type RevComments struct {
	ID       int
	Ref      string
	Hash     string
	Comments []*GerritCommentInfo
	Files    []*FileComments
}

type FileComments struct {
	Name     string
	Comments []*GerritCommentInfo
}

type GerritCommentInfo struct {
	ID        string              `json:"id,omitempty"`
	Path      string              `json:"path,omitempty"`
	Side      string              `json:"side,omitempty"`
	Line      int                 `json:"line,omitempty"`
	Range     *GerritCommentRange `json:"range"`
	InReplyTo string              `json:"in_reply_to,omitempty"`
	Message   string              `json:"message,omitempty"`
	Updated   string              `json:"updated,omitempty"`
	Author    *GerritAccount      `json:"author"`

	revID string // associated revision ID (internal use)
}

type GerritCommentRange struct {
	StartLine int `json:"start_line"`
	StartChar int `json:"start_char"`
	EndLine   int `json:"end_line"`
	EndChar   int `json:"end_char"`
}

func (g *GerritCommentInfo) IsDraft() bool {
	return g.Author == nil
}

func (g *GerritCommentInfo) AuthorName() string {
	if g.Author == nil {
		return ""
	}
	return g.Author.Name
}

func (g *GerritCommentInfo) AuthorEmail() string {
	if g.Author == nil {
		return ""
	}
	return g.Author.Email
}

func cmdComments(args []string) {
	var commentsEdit bool

	flags.BoolVar(&commentsEdit, "e", false, "prepare replies in editor")
	flags.Usage = func() {
		fmt.Fprintf(stderr(), "Usage: %s comments %s [-e] [commit-hash]\n", os.Args[0], globalFlags)
	}
	flags.Parse(args)
	if len(flags.Args()) > 1 {
		flags.Usage()
		os.Exit(2)
	}

	b := CurrentBranch()
	var c *Commit
	if len(flags.Args()) == 1 {
		c = b.CommitByHash("comments", flags.Arg(0))
	} else {
		c = b.DefaultCommit("comments")
	}

	// for side effect of dying with a good message if origin is GitHub
	loadGerritOrigin()

	g, err := b.GerritChange(c, "ALL_REVISIONS", "MESSAGES")
	if err != nil {
		dief("%v", err)
	}

	var selfEmail string
	if g.Owner != nil {
		selfEmail = g.Owner.Email
	}

	var change ChangeComments
	change.ID = g.ID
	for hash, info := range g.Revisions {
		var rev RevComments
		rev.ID = info.Number
		rev.Ref = info.Ref
		rev.Hash = hash
		for _, m := range g.Messages {
			if m.RevisionNumber == rev.ID {
				rev.Comments = append(rev.Comments, &GerritCommentInfo{
					Message: m.Message,
					Author:  m.Author,
					Updated: m.Updated,
				})
			}
		}

		var m map[string][]*GerritCommentInfo
		err := gerritAPI("GET", "/a/changes/"+g.ID+"/revisions/"+hash+"/comments/", nil, &m)
		if err != nil {
			dief("reading comments: %v", err)
		}
		var mdraft map[string][]*GerritCommentInfo
		err = gerritAPI("GET", "/a/changes/"+g.ID+"/revisions/"+hash+"/drafts/", nil, &mdraft)
		if err != nil {
			dief("reading comments: %v", err)
		}
		for name, list := range mdraft {
			for _, c := range list {
				c.Author = nil // should already be nil but make sure
			}
			m[name] = append(m[name], list...)
		}
		for name, list := range m {
			var file FileComments
			file.Name = name
			for _, c := range list {
				c.revID = fmt.Sprint(rev.ID)
				c.Path = name // implicit now but needed for writing back
				file.Comments = append(file.Comments, c)
			}
			sort.Stable(commentsByLine(file.Comments))
			rev.Files = append(rev.Files, &file)
		}
		sort.Sort(filesByName(rev.Files))
		change.Revs = append(change.Revs, &rev)
	}
	sort.Sort(revsByID(change.Revs))

	if len(change.Revs) > 0 {
		rev := change.Revs[0]
		ref := rev.Ref
		i := strings.LastIndex(ref, "/")
		if i >= 0 {
			ref = ref[:i] + "/*"
		}
		run("git", "fetch", "-q", "origin", ref+":"+ref)
	}

	var buf bytes.Buffer
	printChangeComments(&buf, &change, selfEmail, commentsEdit)

	if !commentsEdit {
		os.Stdout.Write(buf.Bytes())
		return
	}

	f, err := ioutil.TempFile(repoRoot(), "comments."+c.ShortHash+".")
	if err != nil {
		dief("%s", err)
	}
	name := f.Name()
	if _, err := f.Write(buf.Bytes()); err != nil {
		dief("writing editor template: %v", err)
	}
	if err := f.Close(); err != nil {
		dief("writing editor template: %v", err)
	}

	editor := trim(cmdOutput("git", "var", "GIT_EDITOR"))
	if editor == "" {
		editor = "ed" // the standard editor
	}

	run(editor, name)
	if *noRun {
		return
	}

	data, err := ioutil.ReadFile(name)
	if err != nil {
		dief("reading editor template: %v", err)
	}

	if !updateChangeComments(&change, string(data)) {
		dief("unable to write all changes to Gerrit\neditor file saved in %s", name)
	}

	os.Remove(name)
}

func printChangeComments(w io.Writer, change *ChangeComments, selfEmail string, template bool) {
	prefix := ""
	if template {
		prefix = "\t"
	}
	var maxDraft string
	for _, rev := range change.Revs {
		fmt.Fprintf(w, "%sPatch Set #%d %s\n", prefix, rev.ID, rev.Ref)
		// TODO: Show scores here.
		for _, c := range rev.Comments {
			fmt.Fprintf(w, "\n")
			printComment(w, c, prefix, "")
		}

		addr := ""
		haveDraft := false
		haveReply := false
		flush := func() {
			if addr != "" && !haveDraft && template {
				if haveReply {
					fmt.Fprintf(w, "\n<replied>\n")
				} else {
					fmt.Fprintf(w, "\n<no reply>\n")
				}
			}
			haveDraft = false
			haveReply = false
		}

		for _, file := range rev.Files {
			for i, c := range file.Comments {
				newAddr, text := commentLine(rev, file.Name, c)
				if newAddr != addr {
					flush()
					addr = newAddr
					fmt.Fprintf(w, "\n%s¶ %s\n", prefix, addr)
					if file.Name != "/COMMIT_MSG" {
						fmt.Fprintf(w, "\n%s\t%s\n", prefix, strings.Replace(strings.TrimSuffix(text, "\n"), "\n", "\n"+prefix+"\t", -1))
					}
				}
				fmt.Fprintf(w, "\n")
				if c.IsDraft() {
					if i == 0 || !template {
						fmt.Fprintf(w, "%s« %s (%s %s) »\n", prefix, "Draft", shortTime(c.Updated), c.ID)
					}
					if maxDraft < c.Updated {
						maxDraft = c.Updated
					}
					haveDraft = true
				} else {
					haveReply = c.AuthorEmail() == selfEmail
					haveDraft = false
				}
				printComment(w, c, prefix, file.Name)
			}
		}
		flush()
	}
}

var (
	msgHdrRE = regexp.MustCompile(`(?m)^\t« (.*) \((?:.* )?([a-z0-9_]+)\) »`)
	draftRE  = regexp.MustCompile(`(?m)\n\S.*\n(([ \t]*|[^\t\n].*|\t[^¶«].*)\n)*`)
)

func updateChangeComments(change *ChangeComments, text string) (ok bool) {
	drafts := map[string]*GerritCommentInfo{}
	replyto := map[string]*GerritCommentInfo{}
	posts := map[string]*GerritCommentInfo{}
	var rewritten []*GerritCommentInfo
	var draftOrder []*GerritCommentInfo

	ok = true
	lostMessage := func(reason, message string) {
		fmt.Fprintf(stderr(), "%s\n\nOriginal message:\n%s\n\n", reason, message)
		ok = false
	}

	for _, rev := range change.Revs {
		for _, file := range rev.Files {
			for _, c := range file.Comments {
				if c.IsDraft() {
					draftOrder = append(draftOrder, c)
					drafts[c.ID] = c
					if c.InReplyTo != "" {
						replyto[c.InReplyTo] = c
					}
				} else {
					posts[c.ID] = c
				}
			}
		}
	}

	msgHdrs := msgHdrRE.FindAllStringSubmatchIndex(text, -1)
	for _, m := range draftRE.FindAllStringSubmatchIndex(text, -1) {
		message := strings.TrimSpace(text[m[0]:m[1]])
		if message == "<no reply>" || message == "<replied>" {
			continue
		}

		var hdr []int
		for len(msgHdrs) > 0 && msgHdrs[0][1] <= m[0] {
			hdr = msgHdrs[0]
			msgHdrs = msgHdrs[1:]
		}

		if len(hdr) == 0 {
			lostMessage("cannot find draft or replied-to post header", message)
			continue
		}

		id := text[hdr[4]:hdr[5]]
		var c *GerritCommentInfo
		if text[hdr[2]:hdr[3]] == "Draft" {
			c = drafts[id]
			if c != nil {
				delete(drafts, id)
			} else {
				lostMessage("cannot find comment ID mentioned in draft header", message)
				continue
			}
		} else {
			c = replyto[id]
			if c != nil {
				delete(replyto, id)
				delete(drafts, c.ID)
			} else {
				post := posts[id]
				if post == nil {
					lostMessage("cannot find comment ID mentioned in replied-to post header", message)
					continue
				}
				c = &GerritCommentInfo{
					revID:     post.revID,
					Path:      post.Path,
					Side:      post.Side,
					Line:      post.Line,
					Range:     post.Range,
					InReplyTo: post.ID,
				}
			}
		}

		if strings.TrimSpace(c.Message) == message {
			continue
		}

		c.Message = message
		rewritten = append(rewritten, c)
	}

	// Post new drafts.
	for _, c := range rewritten {
		url := "/a/changes/" + change.ID + "/revisions/" + c.revID + "/drafts"
		if c.ID != "" {
			url += "/" + c.ID
		}
		data, err := json.Marshal(c)
		if err != nil {
			lostMessage("cannot marshal JSON during PUT", c.Message)
			continue
		}
		if err := gerritAPI("PUT", url, data, nil); err != nil {
			lostMessage("failure writing to Gerrit: "+err.Error(), c.Message)
		}
	}

	// Delete drafts that were removed from the file.
	for _, c := range draftOrder {
		if drafts[c.ID] == c {
			url := "/a/changes/" + change.ID + "/revisions/" + c.revID + "/drafts/" + c.ID
			if err := gerritAPI("DELETE", url, nil, nil); err != nil {
				lostMessage("failed to delete message from Gerrit: "+err.Error(), c.Message)
			}
		}
	}

	return ok
}

func commentLine(rev *RevComments, file string, c *GerritCommentInfo) (addr string, text string) {
	var start, end int
	if c.Range != nil {
		start = c.Range.StartLine
		if c.Range.EndLine != c.Range.StartLine {
			end = c.Range.EndLine
		}
	} else {
		start = c.Line
	}

	ref := rev.Ref
	if c.Side == "PARENT" {
		ref += "^"
	}

	text = sourceText(ref, file, start, end)

	start = translateLine(ref, file, start)
	if end != 0 {
		end = translateLine(ref, file, end)
	}

	if end == 0 {
		addr = fmt.Sprintf("%s:%d", file, start)
	} else {
		addr = fmt.Sprintf("%s:%d,%d", file, start, end)
	}
	if c.Side == "PARENT" {
		addr += " (original copy)"
	}

	return addr, text
}

var chunkRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func translateLine(ref, file string, line int) int {
	if file == "/COMMIT_MSG" {
		return line
	}
	oldLine := 1
	newLine := 1
	for _, text := range lines(cmdOutput("git", "diff", "--no-ext-diff", ref, "--", file)) {
		m := chunkRE.FindStringSubmatch(text)
		if m != nil {
			chunkOld, _ := strconv.Atoi(m[1])
			chunkNew, _ := strconv.Atoi(m[3])
			if chunkOld == 0 || chunkNew == 0 {
				continue
			}
			if line < chunkOld {
				return line + newLine - oldLine
			}
			oldLine = chunkOld
			newLine = chunkNew
			continue
		}
		if line == oldLine {
			return newLine
		}
		switch {
		case strings.HasPrefix(text, "-"):
			oldLine++
		case strings.HasPrefix(text, "+"):
			newLine++
		default:
			oldLine++
			newLine++
		}
	}
	return line + newLine - oldLine
}

func sourceText(ref, file string, start, end int) string {
	var text []string
	if file == "/COMMIT_MSG" {
		text = lines(cmdOutput("git", "show", "-s", "--format=%B", ref))
	} else {
		text = lines(cmdOutput("git", "show", ref+":"+file))
	}
	if len(text) > 0 && text[len(text)-1] == "" {
		text = text[:len(text)-1]
	}
	if end == 0 {
		end = start
	}
	if end > len(text) {
		end = len(text)
	}
	start--
	if start < 0 {
		start = 0
	}
	if start > len(text) {
		start = len(text)
	}

	text = text[start:end]
	n := 1000
	for _, line := range text {
		m := len(line) - len(strings.TrimLeft(line, " \t\n"))
		if m < len(line) && n > m {
			n = m
		}
	}
	for i, line := range text {
		if len(line) < n {
			text[i] = ""
		} else {
			text[i] = line[n:]
		}
	}

	if len(text) == 0 {
		return ""
	}
	return strings.Join(text, "\n") + "\n"
}

func printComment(w io.Writer, c *GerritCommentInfo, prefix, name string) {
	if c.IsDraft() {
		fmt.Fprintf(w, "%s\n", strings.TrimSpace(c.Message))
	} else {
		fmt.Fprintf(w, "%s« %s (%s %s) »\n", prefix, c.AuthorName(), shortTime(c.Updated), c.ID)
		fmt.Fprintf(w, "%s%s\n", prefix, strings.Replace(strings.TrimSpace(c.Message), "\n", "\n"+prefix, -1))
	}
}

func shortTime(s string) string {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s
	}
	return s[:i]
}

type commentsByLine []*GerritCommentInfo

func (x commentsByLine) Len() int      { return len(x) }
func (x commentsByLine) Swap(i, j int) { x[i], x[j] = x[j], x[i] }
func (x commentsByLine) Less(i, j int) bool {
	line1 := x[i].Line
	if line1 == 0 && x[i].Range != nil {
		line1 = x[i].Range.StartLine
	}
	line2 := x[j].Line
	if line2 == 0 && x[j].Range != nil {
		line2 = x[j].Range.StartLine
	}
	return line1 < line2
}

type filesByName []*FileComments

func (x filesByName) Len() int      { return len(x) }
func (x filesByName) Swap(i, j int) { x[i], x[j] = x[j], x[i] }
func (x filesByName) Less(i, j int) bool {
	return x[i].Name < x[j].Name
}

type revsByID []*RevComments

func (x revsByID) Len() int      { return len(x) }
func (x revsByID) Swap(i, j int) { x[i], x[j] = x[j], x[i] }
func (x revsByID) Less(i, j int) bool {
	return x[i].ID < x[j].ID
}
