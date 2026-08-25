package drive_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SphericalKat/telemirror/internal/drive"
)

func newLister(t *testing.T, svc drive.Service, cfg drive.Config) *drive.Lister {
	t.Helper()
	l, err := drive.NewLister(svc, cfg)
	if err != nil {
		t.Fatalf("NewLister() error = %v", err)
	}
	return l
}

// childAt returns a file child modified base+i minutes, so a higher i is newer.
func childAt(i int, base time.Time) drive.Child {
	return drive.Child{
		ID:           string(rune('a' + i)),
		Name:         string(rune('a' + i)),
		ModifiedTime: base.Add(time.Duration(i) * time.Minute),
	}
}

func TestListExpandsUpstreamSearchVariants(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "single word stays unchanged",
			query: "movie",
			want:  []string{"movie"},
		},
		{
			name:  "one space tries all four separators",
			query: "movie night",
			want:  []string{"movie night", "movie.night", "movie-night", "movie_night"},
		},
		{
			name:  "variants replace earlier separators too",
			query: "my file.name-x",
			want:  []string{"my file.name-x", "my.file.name-x", "my-file-name-x", "my_file_name_x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeService{}
			l := newLister(t, svc, drive.Config{ParentFolderID: "parent-0"})

			if _, err := l.List(context.Background(), tc.query); err != nil {
				t.Fatalf("List() error = %v", err)
			}

			parentID, names := svc.lastSearch()
			if parentID != "parent-0" {
				t.Errorf("search parent = %q, want %q", parentID, "parent-0")
			}
			if !equalStrings(names, tc.want) {
				t.Errorf("search names = %v, want %v", names, tc.want)
			}
		})
	}
}

func TestListOrdersNewestFirstAndLimitsToTwenty(t *testing.T) {
	svc := &fakeService{}
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	// The service returns 25 matches in reverse order, oldest candidate first.
	all := make([]drive.Child, 0, 25)
	for i := 24; i >= 0; i-- {
		all = append(all, childAt(i, base))
	}
	svc.listResults = all

	l := newLister(t, svc, drive.Config{ParentFolderID: "parent-0"})
	got, err := l.List(context.Background(), "match")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if len(got) != 20 {
		t.Fatalf("results = %d, want 20", len(got))
	}
	// The newest child is 24 minutes past base; the cap keeps the newest 20,
	// so the oldest kept child is 5 minutes past base.
	for i, wantMinute := 0, 24; i < len(got); i, wantMinute = i+1, wantMinute-1 {
		wantID := string(rune('a' + wantMinute))
		if got[i].ID != wantID {
			t.Errorf("results[%d] = %s, want %s (all: %v)", i, got[i].ID, wantID, ids(got))
		}
		if !got[i].ModifiedTime.Equal(base.Add(time.Duration(wantMinute) * time.Minute)) {
			t.Errorf("results[%d] is not ordered by newest modification first", i)
		}
	}
}

func TestListDerivesUpstreamLinks(t *testing.T) {
	svc := &fakeService{}
	svc.listResults = []drive.Child{
		{ID: "file-9", Name: "movie.mkv"},
		{ID: "folder-3", Name: "pack", IsFolder: true},
	}

	l := newLister(t, svc, drive.Config{ParentFolderID: "parent-0"})
	got, err := l.List(context.Background(), "pack")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("results = %d, want 2", len(got))
	}
	if got[0].Link != drive.FileLink("file-9") {
		t.Errorf("file link = %q, want %q", got[0].Link, drive.FileLink("file-9"))
	}
	if got[1].Link != drive.FolderLink("folder-3") {
		t.Errorf("folder link = %q, want %q", got[1].Link, drive.FolderLink("folder-3"))
	}
}

func TestListReturnsServiceError(t *testing.T) {
	svc := &fakeService{listErr: errors.New("quota exceeded")}
	l := newLister(t, svc, drive.Config{ParentFolderID: "parent-0"})

	_, err := l.List(context.Background(), "movie")
	if err == nil {
		t.Fatal("List() error = nil, want failure")
	}
	if !errors.Is(err, svc.listErr) {
		t.Errorf("List() error = %v, want it to wrap the service cause", err)
	}
}

func TestListerFolderLinkTargetsConfiguredFolder(t *testing.T) {
	l := newLister(t, &fakeService{}, drive.Config{ParentFolderID: "parent-7"})

	if got, want := l.FolderLink(), drive.FolderLink("parent-7"); got != want {
		t.Errorf("FolderLink() = %q, want %q", got, want)
	}
}

func TestNewListerRequiresServiceAndParentFolder(t *testing.T) {
	if _, err := drive.NewLister(nil, drive.Config{ParentFolderID: "parent-0"}); err == nil {
		t.Error("NewLister(nil service) error = nil, want failure")
	}
	if _, err := drive.NewLister(&fakeService{}, drive.Config{}); err == nil {
		t.Error("NewLister(empty parent) error = nil, want failure")
	}
}

func ids(children []drive.Child) []string {
	out := make([]string, 0, len(children))
	for _, child := range children {
		out = append(out, child.ID)
	}
	return out
}
