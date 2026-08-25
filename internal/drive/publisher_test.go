package drive_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SphericalKat/telemirror/internal/drive"
)

// fakeService records Drive operations instead of calling Google. It stands in
// for the Drive adapter at the boundary Telemirror owns.
type fakeService struct {
	mu sync.Mutex

	ops    []driveOp
	folder int
	file   int

	// uploadFailure names the file whose upload must fail.
	uploadFailure string
	uploadErr     error
	folderErr     error
	publicErr     error
	readerErr     error

	// blockUploads makes uploads wait for context cancellation.
	blockUploads bool
}

type driveOp struct {
	kind   string // "folder", "file", "public", "reader"
	name   string // folder or file name; email for reader ops
	parent string
	id     string
	path   string
	mime   string
	size   int64
}

func (f *fakeService) CreateFolder(_ context.Context, name, parentID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.folderErr != nil {
		return "", f.folderErr
	}
	f.folder++
	id := fmt.Sprintf("folder-%d", f.folder)
	f.ops = append(f.ops, driveOp{kind: "folder", name: name, parent: parentID, id: id})
	return id, nil
}

func (f *fakeService) UploadFile(ctx context.Context, path, name, mimeType, parentID string, size int64, onProgress func(sent int64)) (string, error) {
	f.mu.Lock()
	if f.blockUploads {
		f.mu.Unlock()
		<-ctx.Done()
		return "", ctx.Err()
	}
	if f.uploadErr != nil && name == f.uploadFailure {
		err := f.uploadErr
		f.mu.Unlock()
		return "", err
	}
	f.file++
	id := fmt.Sprintf("file-%d", f.file)
	f.ops = append(f.ops, driveOp{kind: "file", name: name, parent: parentID, id: id, path: path, mime: mimeType, size: size})
	f.mu.Unlock()

	if onProgress != nil && size > 0 {
		onProgress(size / 3)
		onProgress(size)
	}
	return id, nil
}

func (f *fakeService) GrantPublicRead(_ context.Context, fileID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.publicErr != nil {
		return f.publicErr
	}
	f.ops = append(f.ops, driveOp{kind: "public", id: fileID})
	return nil
}

func (f *fakeService) GrantReadAccess(_ context.Context, fileID, email string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readerErr != nil {
		return f.readerErr
	}
	f.ops = append(f.ops, driveOp{kind: "reader", id: fileID, name: email})
	return nil
}

func (f *fakeService) snapshot() []driveOp {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]driveOp(nil), f.ops...)
}

// summary renders each operation as a short string for order checks.
func summary(ops []driveOp) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		switch op.kind {
		case "public":
			out = append(out, fmt.Sprintf("public:%s", op.id))
		case "reader":
			out = append(out, fmt.Sprintf("reader:%s:%s", op.id, op.name))
		default:
			out = append(out, fmt.Sprintf("%s:%s[%s]", op.kind, op.name, op.parent))
		}
	}
	return out
}

func newPublisher(t *testing.T, svc drive.Service, cfg drive.Config) *drive.Publisher {
	t.Helper()
	p, err := drive.NewPublisher(svc, cfg)
	if err != nil {
		t.Fatalf("NewPublisher() error = %v", err)
	}
	return p
}

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestPublishFileUploadsToConfiguredFolder(t *testing.T) {
	svc := &fakeService{}
	p := newPublisher(t, svc, drive.Config{ParentFolderID: "parent-0"})

	root := t.TempDir()
	file := filepath.Join(root, "movie.mkv")
	writeFile(t, file, 512)

	res, err := p.Publish(context.Background(), file, nil)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	want := []string{"file:movie.mkv[parent-0]", "public:file-1"}
	if got := summary(svc.snapshot()); !equalStrings(got, want) {
		t.Fatalf("operations = %v, want %v", got, want)
	}
	if res.DriveID != "file-1" {
		t.Errorf("DriveID = %q, want %q", res.DriveID, "file-1")
	}
	if res.Name != "movie.mkv" {
		t.Errorf("Name = %q, want %q", res.Name, "movie.mkv")
	}
	if res.IsFolder {
		t.Error("IsFolder = true, want false")
	}
	if want := drive.FileLink("file-1"); res.Link != want {
		t.Errorf("Link = %q, want %q", res.Link, want)
	}
}

func TestPublishDirectoryUploadsRecursivelyInOrder(t *testing.T) {
	svc := &fakeService{}
	p := newPublisher(t, svc, drive.Config{ParentFolderID: "parent-0"})

	root := t.TempDir()
	show := filepath.Join(root, "show")
	season1 := filepath.Join(show, "season1")
	for _, dir := range []string{season1, filepath.Join(show, "empty")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeFile(t, filepath.Join(season1, "b.txt"), 10)
	writeFile(t, filepath.Join(season1, "a.txt"), 10)
	writeFile(t, filepath.Join(show, "zz.bin"), 10)
	writeFile(t, filepath.Join(show, "aa.txt"), 10)

	res, err := p.Publish(context.Background(), show, nil)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	// The root folder is created first, then entries are processed one at a
	// time in lexical order, depth first. Only the root folder is shared.
	want := []string{
		"folder:show[parent-0]",
		"file:aa.txt[folder-1]",
		"folder:empty[folder-1]",
		"folder:season1[folder-1]",
		"file:a.txt[folder-3]",
		"file:b.txt[folder-3]",
		"file:zz.bin[folder-1]",
		"public:folder-1",
	}
	if got := summary(svc.snapshot()); !equalStrings(got, want) {
		t.Fatalf("operations = %v\nwant %v", got, want)
	}
	if !res.IsFolder {
		t.Error("IsFolder = false, want true")
	}
	if res.DriveID != "folder-1" {
		t.Errorf("DriveID = %q, want %q", res.DriveID, "folder-1")
	}
	if res.Name != "show" {
		t.Errorf("Name = %q, want %q", res.Name, "show")
	}
	if want := drive.FolderLink("folder-1"); res.Link != want {
		t.Errorf("Link = %q, want %q", res.Link, want)
	}
}

func TestPublishReportsCumulativeProgress(t *testing.T) {
	svc := &fakeService{}
	p := newPublisher(t, svc, drive.Config{ParentFolderID: "parent-0"})

	root := t.TempDir()
	pack := filepath.Join(root, "pack")
	if err := os.MkdirAll(pack, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(pack, "one.bin"), 300)
	writeFile(t, filepath.Join(pack, "two.bin"), 600)

	var got []drive.Progress
	_, err := p.Publish(context.Background(), pack, func(pr drive.Progress) {
		got = append(got, pr)
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	// The fake reports one third, then the full size of each file.
	want := []drive.Progress{
		{UploadedBytes: 100, TotalBytes: 900},
		{UploadedBytes: 300, TotalBytes: 900},
		{UploadedBytes: 500, TotalBytes: 900},
		{UploadedBytes: 900, TotalBytes: 900},
	}
	if len(got) != len(want) {
		t.Fatalf("progress = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("progress[%d] = %+v, want %+v (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestPublishPrivateSharingGrantsReaderAccessToConfiguredEmails(t *testing.T) {
	svc := &fakeService{}
	cfg := drive.Config{
		ParentFolderID: "parent-0",
		Private: drive.PrivateSharing{
			Enabled: true,
			Emails:  []string{"one@example.com", "two@example.com"},
		},
	}
	p := newPublisher(t, svc, cfg)

	root := t.TempDir()
	file := filepath.Join(root, "doc.pdf")
	writeFile(t, file, 100)

	res, err := p.Publish(context.Background(), file, nil)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	want := []string{
		"file:doc.pdf[parent-0]",
		"reader:file-1:one@example.com",
		"reader:file-1:two@example.com",
	}
	if got := summary(svc.snapshot()); !equalStrings(got, want) {
		t.Fatalf("operations = %v, want %v", got, want)
	}
	if res.Link != drive.FileLink("file-1") {
		t.Errorf("Link = %q, want %q", res.Link, drive.FileLink("file-1"))
	}
}

func TestPublishSharedDriveSkipsFolderSharingButReturnsFolderLink(t *testing.T) {
	svc := &fakeService{}
	p := newPublisher(t, svc, drive.Config{ParentFolderID: "parent-0", SharedDrive: true})

	root := t.TempDir()
	pack := filepath.Join(root, "pack")
	if err := os.MkdirAll(pack, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(pack, "data.bin"), 100)

	res, err := p.Publish(context.Background(), pack, nil)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	want := []string{
		"folder:pack[parent-0]",
		"file:data.bin[folder-1]",
	}
	if got := summary(svc.snapshot()); !equalStrings(got, want) {
		t.Fatalf("operations = %v, want %v (folder must not be shared)", got, want)
	}
	if res.Link != drive.FolderLink("folder-1") {
		t.Errorf("Link = %q, want %q", res.Link, drive.FolderLink("folder-1"))
	}
}

func TestPublishSharedDriveStillSharesFiles(t *testing.T) {
	svc := &fakeService{}
	p := newPublisher(t, svc, drive.Config{ParentFolderID: "parent-0", SharedDrive: true})

	root := t.TempDir()
	file := filepath.Join(root, "solo.bin")
	writeFile(t, file, 100)

	if _, err := p.Publish(context.Background(), file, nil); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	want := []string{"file:solo.bin[parent-0]", "public:file-1"}
	if got := summary(svc.snapshot()); !equalStrings(got, want) {
		t.Fatalf("operations = %v, want %v", got, want)
	}
}

func TestPublishDetectsMIMETypes(t *testing.T) {
	svc := &fakeService{}
	p := newPublisher(t, svc, drive.Config{ParentFolderID: "parent-0"})

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "note.txt"), 10)
	writeFile(t, filepath.Join(root, "blob"), 10)

	if _, err := p.Publish(context.Background(), filepath.Join(root, "note.txt"), nil); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if _, err := p.Publish(context.Background(), filepath.Join(root, "blob"), nil); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	ops := svc.snapshot()
	var mimes []string
	for _, op := range ops {
		if op.kind == "file" {
			mimes = append(mimes, op.mime)
		}
	}
	if len(mimes) != 2 {
		t.Fatalf("file uploads = %d, want 2 (all operations: %v)", len(mimes), summary(ops))
	}
	if !strings.HasPrefix(mimes[0], "text/plain") {
		t.Errorf("note.txt mime = %q, want prefix text/plain", mimes[0])
	}
	if mimes[1] != "application/octet-stream" {
		t.Errorf("blob mime = %q, want application/octet-stream", mimes[1])
	}
}

func TestPublishEmptyFileWithoutProgress(t *testing.T) {
	svc := &fakeService{}
	p := newPublisher(t, svc, drive.Config{ParentFolderID: "parent-0"})

	root := t.TempDir()
	file := filepath.Join(root, "empty.bin")
	writeFile(t, file, 0)

	var reports int
	res, err := p.Publish(context.Background(), file, func(drive.Progress) { reports++ })
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	ops := svc.snapshot()
	if len(ops) != 2 || ops[0].kind != "file" || ops[0].size != 0 {
		t.Fatalf("operations = %v, want one empty-file upload and one share", summary(ops))
	}
	if reports != 0 {
		t.Errorf("progress reports = %d, want 0", reports)
	}
	if res.Link != drive.FileLink("file-1") {
		t.Errorf("Link = %q, want %q", res.Link, drive.FileLink("file-1"))
	}
}

func TestPublishUploadFailureReturnsClearErrorAndDoesNotShare(t *testing.T) {
	svc := &fakeService{
		uploadFailure: "second.bin",
		uploadErr:     errors.New("disk quota exceeded"),
	}
	p := newPublisher(t, svc, drive.Config{ParentFolderID: "parent-0"})

	root := t.TempDir()
	pack := filepath.Join(root, "pack")
	if err := os.MkdirAll(pack, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(pack, "first.bin"), 10)
	writeFile(t, filepath.Join(pack, "second.bin"), 10)

	_, err := p.Publish(context.Background(), pack, nil)
	if err == nil {
		t.Fatal("Publish() error = nil, want failure")
	}
	if !strings.Contains(err.Error(), "second.bin") || !strings.Contains(err.Error(), "disk quota exceeded") {
		t.Errorf("error = %v, want it to name second.bin and the cause", err)
	}
	for _, op := range svc.snapshot() {
		if op.kind == "public" || op.kind == "reader" {
			t.Fatalf("share attempted after upload failure: %v", summary(svc.snapshot()))
		}
	}
}

func TestPublishMissingPathFailsWithoutDriveCalls(t *testing.T) {
	svc := &fakeService{}
	p := newPublisher(t, svc, drive.Config{ParentFolderID: "parent-0"})

	_, err := p.Publish(context.Background(), filepath.Join(t.TempDir(), "absent"), nil)
	if err == nil {
		t.Fatal("Publish() error = nil, want failure")
	}
	if ops := svc.snapshot(); len(ops) != 0 {
		t.Fatalf("operations = %v, want none", summary(ops))
	}
}

func TestPublishShareFailureReturnsClearError(t *testing.T) {
	svc := &fakeService{publicErr: errors.New("sharing disabled")}
	p := newPublisher(t, svc, drive.Config{ParentFolderID: "parent-0"})

	root := t.TempDir()
	file := filepath.Join(root, "movie.mkv")
	writeFile(t, file, 100)

	_, err := p.Publish(context.Background(), file, nil)
	if err == nil {
		t.Fatal("Publish() error = nil, want failure")
	}
	if !strings.Contains(err.Error(), "sharing disabled") {
		t.Errorf("error = %v, want it to include the sharing cause", err)
	}
}

func TestPublishStopsWhenContextCancelled(t *testing.T) {
	svc := &fakeService{blockUploads: true}
	p := newPublisher(t, svc, drive.Config{ParentFolderID: "parent-0"})

	root := t.TempDir()
	file := filepath.Join(root, "movie.mkv")
	writeFile(t, file, 100)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := p.Publish(ctx, file, nil)
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Publish() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Publish() did not return after cancellation")
	}
	for _, op := range svc.snapshot() {
		if op.kind == "public" || op.kind == "reader" {
			t.Fatal("share attempted after cancellation")
		}
	}
}

func TestNewPublisherRequiresParentFolder(t *testing.T) {
	if _, err := drive.NewPublisher(&fakeService{}, drive.Config{}); err == nil {
		t.Fatal("NewPublisher() error = nil, want failure for empty parent folder")
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
