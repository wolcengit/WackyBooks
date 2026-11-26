package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrowseDirectoriesRequiresAdmin(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Books"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("ignored"), 0600); err != nil {
		t.Fatal(err)
	}

	expires := time.Now().Add(time.Minute)
	s := &Server{sessions: map[string]Session{
		"reader": {Expires: expires},
		"admin":  {Admin: true, Expires: expires},
	}}
	request := func(path, session string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if session != "" {
			req.AddCookie(&http.Cookie{Name: "wacky_session", Value: session})
		}
		recorder := httptest.NewRecorder()
		s.api(recorder, req)
		return recorder
	}

	if got := request("/api/directories", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request status = %d, want %d", got, http.StatusUnauthorized)
	}
	if got := request("/api/directories", "reader").Code; got != http.StatusForbidden {
		t.Fatalf("reader request status = %d, want %d", got, http.StatusForbidden)
	}

	path := "/api/directories?path=" + url.QueryEscape(root)
	recorder := request(path, "admin")
	if recorder.Code != http.StatusOK {
		t.Fatalf("admin request status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body)
	}
	var listing directoryListing
	if err := json.NewDecoder(recorder.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if listing.Path != root || len(listing.Directories) != 1 || listing.Directories[0].Name != "Books" {
		t.Fatalf("unexpected directory listing: %#v", listing)
	}
	if got := request("/api/directories?path=relative", "admin").Code; got != http.StatusBadRequest {
		t.Fatalf("relative path status = %d, want %d", got, http.StatusBadRequest)
	}
}

func TestScanLibraryOnlyUpdatesSelectedLibrary(t *testing.T) {
	root := t.TempDir()
	firstDirectory := filepath.Join(root, "first")
	secondDirectory := filepath.Join(root, "second")
	for _, directory := range []string{firstDirectory, secondDirectory} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(directory, "subdir"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	firstBook := filepath.Join(firstDirectory, "first.txt")
	secondBook := filepath.Join(secondDirectory, "second.txt")
	if err := os.WriteFile(firstBook, []byte("first book"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondBook, []byte("second book"), 0600); err != nil {
		t.Fatal(err)
	}

	db, err := openStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	addLibrary := func(key, name, directory string, sequence int) Library {
		result, err := db.Exec(`INSERT INTO libraries (public_key,name,directory,sequence) VALUES (?,?,?,?)`, key, name, directory, sequence)
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return Library{ID: id, Name: name, Directory: directory, key: key}
	}
	first := addLibrary("first-library", "First", firstDirectory, 0)
	second := addLibrary("second-library", "Second", secondDirectory, 1)
	s := &Server{
		db:       db,
		state:    State{Libraries: []Library{first, second}},
		books:    map[int64]Book{},
		sessions: map[string]Session{"admin": {Admin: true, Expires: time.Now().Add(time.Minute)}},
	}
	if err := s.scan(); err != nil {
		t.Fatal(err)
	}
	if len(s.books) != 2 {
		t.Fatalf("initial scan found %d books, want 2", len(s.books))
	}
	if err := os.Remove(secondBook); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(firstDirectory, "added.txt"), []byte("new book"), 0600); err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(struct{ ID int64 }{first.ID})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/library/scan", strings.NewReader(string(body)))
	request.AddCookie(&http.Cookie{Name: "wacky_session", Value: "admin"})
	recorder := httptest.NewRecorder()
	s.api(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("scoped scan endpoint status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body)
	}
	var firstActive, secondActive int
	if err := db.QueryRow(`SELECT COUNT(*) FROM books WHERE library_id=? AND active=1`, first.ID).Scan(&firstActive); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM books WHERE library_id=? AND active=1`, second.ID).Scan(&secondActive); err != nil {
		t.Fatal(err)
	}
	if firstActive != 2 || secondActive != 1 {
		t.Fatalf("active books after scoped scan = (%d, %d), want (2, 1)", firstActive, secondActive)
	}
	if len(s.books) != 3 {
		t.Fatalf("in-memory scan contains %d books, want 3", len(s.books))
	}
	if len(s.directories[second.ID]) != 1 || s.directories[second.ID][0] != "subdir" {
		t.Fatalf("directory index for the other library was lost: %#v", s.directories)
	}
}

func TestScanLibraryRequiresAdmin(t *testing.T) {
	expires := time.Now().Add(time.Minute)
	s := &Server{sessions: map[string]Session{
		"reader": {Expires: expires},
		"admin":  {Admin: true, Expires: expires},
	}}
	request := func(session string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/library/scan", strings.NewReader(`{"ID":1}`))
		req.Header.Set("Content-Type", "application/json")
		if session != "" {
			req.AddCookie(&http.Cookie{Name: "wacky_session", Value: session})
		}
		recorder := httptest.NewRecorder()
		s.api(recorder, req)
		return recorder.Code
	}
	if got := request(""); got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated rescan status = %d, want %d", got, http.StatusUnauthorized)
	}
	if got := request("reader"); got != http.StatusForbidden {
		t.Fatalf("reader rescan status = %d, want %d", got, http.StatusForbidden)
	}
}
