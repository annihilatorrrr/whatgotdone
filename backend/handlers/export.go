package handlers

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/mtlynch/whatgotdone/backend/datastore"
	"github.com/mtlynch/whatgotdone/backend/dates"
	"github.com/mtlynch/whatgotdone/backend/types"
	"github.com/mtlynch/whatgotdone/backend/types/export"
)

func (s defaultServer) exportGet() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := mustGetUsernameFromContext(r.Context())

		d, err := s.exportUserData(username)
		if err != nil {
			log.Printf("failed to export user data: %v", err)
			http.Error(w, fmt.Sprintf("Failed to export user data: %s", err), http.StatusInternalServerError)
			return
		}

		respondOK(w, d)
	}
}

func (s defaultServer) exportMarkdownGet() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := mustGetUsernameFromContext(r.Context())

		log.Printf("exporting(%s): published entries", username)
		entries, err := s.datastore.ReadEntries(
			datastore.EntryFilter{
				ByUsers: []types.Username{username},
			})
		if err != nil {
			log.Printf("failed to retrieve user data: %v", err)
			http.Error(w, fmt.Sprintf("Failed to export user data: %s", err), http.StatusInternalServerError)
			return
		}

		filename := fmt.Sprintf("whatgotdone-%s-%s.zip", username, time.Now().Format("2006-01-02"))
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
		_, err = io.Copy(w, packageEntriesAsMarkdown(entries))
		if err != nil {
			log.Printf("failed to stream zip content: %v", err)
			http.Error(w, "Failed to download export", http.StatusInternalServerError)
			return
		}
	}
}

// packageEntriesAsMarkdown converts a slice of entries into a zip file containing entries
// converted to markdown files in the following file structure:
//
//	./2025-07-04/index.md
//	./2025-06-27/index.md
//	./2025-06-20/index.md
func packageEntriesAsMarkdown(entries []types.JournalEntry) io.Reader {
	var buf bytes.Buffer
	zipWriter := zip.NewWriter(&buf)

	for _, entry := range entries {
		// Create directory path based on entry date
		dirPath := fmt.Sprintf("%s/", string(entry.Date))
		filePath := dirPath + "index.md"

		// Create the file in the zip
		fileWriter, err := zipWriter.Create(filePath)
		if err != nil {
			log.Printf("failed to create file in zip: %v", err)
			continue
		}

		// Write the markdown content
		markdown := entryToMarkdown(entry)
		_, err = fileWriter.Write([]byte(markdown))
		if err != nil {
			log.Printf("failed to write content to zip file: %v", err)
			continue
		}
	}

	// Close the zip writer
	err := zipWriter.Close()
	if err != nil {
		log.Printf("failed to close zip writer: %v", err)
		return strings.NewReader("")
	}

	return bytes.NewReader(buf.Bytes())
}

func entryToMarkdown(entry types.JournalEntry) string {
	t := template.Must(template.New("entryexport").Parse(`---
date: {{ .Date }}
{{- if .ShowLastMod }}
lastmod: {{ .LastModDate }}
{{- end }}
---
{{ .Markdown }}`))

	// Check if lastmod date is different from entry date
	entryDate := string(entry.Date)
	lastModDate := entry.LastModified.Format("2006-01-02")
	showLastMod := entryDate != lastModDate

	data := struct {
		Date        types.EntryDate
		LastModDate string
		ShowLastMod bool
		Markdown    types.EntryContent
	}{
		Date:        entry.Date,
		LastModDate: lastModDate,
		ShowLastMod: showLastMod,
		Markdown:    entry.Markdown,
	}

	var buf strings.Builder
	err := t.Execute(&buf, data)
	if err != nil {
		log.Printf("failed to execute template: %v", err)
		return ""
	}

	return buf.String()
}

func (s defaultServer) exportUserData(username types.Username) (export.UserData, error) {
	log.Printf("starting export for %s", username)

	log.Printf("exporting(%s): unpublished drafts", username)
	drafts, err := s.exportUserDrafts(username)
	if err != nil {
		return export.UserData{}, err
	}

	log.Printf("exporting(%s): published entries", username)
	entries, err := s.datastore.ReadEntries(
		datastore.EntryFilter{
			ByUsers: []types.Username{username},
		})
	if err != nil {
		return export.UserData{}, err
	}

	log.Printf("exporting(%s): reactions", username)
	reactions, err := s.exportReactions(username, entries)
	if err != nil {
		return export.UserData{}, err
	}

	log.Printf("exporting(%s): preferences", username)
	prefs, err := s.datastore.GetPreferences(username)
	if _, ok := err.(datastore.PreferencesNotFoundError); ok {
		prefs = types.Preferences{}
	} else if err != nil {
		return export.UserData{}, err
	}

	log.Printf("exporting(%s): user profile", username)
	profile, err := s.datastore.GetUserProfile(username)
	if _, ok := err.(datastore.UserProfileNotFoundError); ok {
		profile = types.UserProfile{}
	} else if err != nil {
		return export.UserData{}, err
	}

	log.Printf("exporting(%s): followed users", username)
	following, err := s.datastore.Following(username)
	if err != nil {
		return export.UserData{}, err
	}

	log.Printf("finished export for %s", username)

	return export.UserData{
		Entries:   entriesToExportedEntries(entries, username),
		Reactions: reactions,
		Drafts:    entriesToExportedEntries(drafts, username),
		Following: following,
		Profile:   profileToExported(profile),
		Preferences: export.Preferences{
			EntryTemplate: string(prefs.EntryTemplate),
		},
	}, nil
}

func (s defaultServer) exportUserDrafts(username types.Username) ([]types.JournalEntry, error) {
	// Retrieve all the user's drafts by checking every possible draft date for an
	// entry. This is inefficient, and we could optimize/parallelize this, but
	// exporting isn't a very common or performance-sensitive code path.

	type result struct {
		draft types.JournalEntry
		err   error
	}
	c := make(chan result)
	var wg sync.WaitGroup

	// 2019-03-29 is the first ever post on What Got Done.
	currentDate := time.Date(2019, time.March, 29, 0, 0, 0, 0, time.UTC)
	for {
		if currentDate.After(dates.ThisFriday()) {
			break
		}
		wg.Add(1)
		go func(date types.EntryDate) {
			defer wg.Done()
			draft, err := s.datastore.GetDraft(username, date)
			c <- result{draft, err}
		}(types.EntryDate(currentDate.Format("2006-01-02")))

		// Increment to next Friday.
		currentDate = currentDate.AddDate(0, 0, 7)
	}

	go func() {
		wg.Wait()
		close(c)
	}()

	drafts := []types.JournalEntry{}
	for res := range c {
		if res.err == nil {
			drafts = append(drafts, res.draft)
			continue
		}
		if _, ok := res.err.(datastore.DraftNotFoundError); ok {
			continue
		}
		if res.err != nil {
			return []types.JournalEntry{}, res.err
		}
	}

	// Sort drafts in ascending order of date.
	sort.Slice(drafts, func(i, j int) bool {
		return drafts[i].Date < drafts[j].Date
	})
	return drafts, nil
}

func entriesToExportedEntries(entries []types.JournalEntry, author types.Username) []export.JournalEntry {
	p := []export.JournalEntry{}
	for _, entry := range entries {
		p = append(p, export.JournalEntry{
			Date:         entry.Date,
			Markdown:     string(entry.Markdown),
			LastModified: entry.LastModified.Format(time.RFC3339),
		})
	}
	return p
}

func (s defaultServer) exportReactions(username types.Username, entries []types.JournalEntry) (map[types.EntryDate][]export.Reaction, error) {
	type result struct {
		date      types.EntryDate
		reactions []export.Reaction
		err       error
	}
	c := make(chan result)
	var wg sync.WaitGroup
	wg.Add(len(entries))

	for _, entry := range entries {
		go func(date types.EntryDate) {
			defer wg.Done()
			reactions, err := s.datastore.GetReactions(username, date)
			if len(reactions) == 0 {
				return
			}
			// Sort reactions in ascending order of timestamp.
			sort.Slice(reactions, func(i, j int) bool {
				return reactions[i].Timestamp.Before(reactions[j].Timestamp)
			})
			var exportedReactions []export.Reaction
			for _, r := range reactions {
				exportedReactions = append(exportedReactions, export.Reaction{
					Username:  r.Username,
					Symbol:    r.Symbol,
					Timestamp: r.Timestamp.Format(time.RFC3339),
				})
			}
			c <- result{date, exportedReactions, err}
		}(entry.Date)
	}

	go func() {
		wg.Wait()
		close(c)
	}()

	reactions := map[types.EntryDate][]export.Reaction{}
	for res := range c {
		if res.err == nil {
			reactions[res.date] = res.reactions
			continue
		}
		if res.err != nil {
			return map[types.EntryDate][]export.Reaction{}, res.err
		}
	}

	return reactions, nil
}

func profileToExported(p types.UserProfile) export.UserProfile {
	return export.UserProfile{
		AboutMarkdown:   p.AboutMarkdown,
		TwitterHandle:   p.TwitterHandle,
		EmailAddress:    p.EmailAddress,
		MastodonAddress: p.MastodonAddress,
	}
}
