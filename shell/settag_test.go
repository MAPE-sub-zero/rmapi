package shell

import (
	"encoding/json"
	"io"
	"os"
	"syscall"
	"testing"

	"github.com/juruen/rmapi/api"
	"github.com/juruen/rmapi/api/sync15"
	"github.com/juruen/rmapi/filetree"
	"github.com/juruen/rmapi/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureStdout runs fn with file descriptor 1 redirected to a pipe and
// returns everything written to it. Reassigning the os.Stdout *package
// variable* is not enough here: the readline library RunShell's ishell.Shell
// depends on binds its own default writer to os.Stdout once, at that
// package's init time (github.com/abiosoft/readline/std.go), so it already
// holds the original *os.File and never observes a later os.Stdout
// reassignment. Redirecting the underlying fd with dup2 affects that
// captured reference too, since it is the same OS file descriptor.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)

	fd := int(os.Stdout.Fd())
	savedFd, err := syscall.Dup(fd)
	require.NoError(t, err)
	require.NoError(t, syscall.Dup2(int(w.Fd()), fd))

	fn()

	require.NoError(t, w.Close())
	require.NoError(t, syscall.Dup2(savedFd, fd))
	require.NoError(t, syscall.Close(savedFd))

	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(out)
}

func TestParseTags(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: []string{},
		},
		{
			name:     "whitespace only",
			input:    "   ",
			expected: []string{},
		},
		{
			name:     "single tag",
			input:    "inbox",
			expected: []string{"inbox"},
		},
		{
			name:     "single tag with whitespace",
			input:    "  inbox  ",
			expected: []string{"inbox"},
		},
		{
			name:     "multiple tags comma-separated",
			input:    "work,urgent,todo",
			expected: []string{"work", "urgent", "todo"},
		},
		{
			name:     "multiple tags with surrounding whitespace",
			input:    " work , urgent , todo ",
			expected: []string{"work", "urgent", "todo"},
		},
		{
			name:     "tags with spaces within tag name",
			input:    "Project Alpha, Reading List, 2026 Goals",
			expected: []string{"Project Alpha", "Reading List", "2026 Goals"},
		},
		{
			name:     "empty items between commas and trailing comma",
			input:    "tag1,,tag2,  ,tag3,",
			expected: []string{"tag1", "tag2", "tag3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseTags(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseSettagArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    *settagArgs
		wantErr string
	}{
		{
			name: "default op is replace",
			args: []string{"test-note", "inbox,urgent"},
			want: &settagArgs{
				Op:               sync15.TagOpReplace,
				Tags:             []string{"inbox", "urgent"},
				Path:             "test-note",
				ExpectedRevision: "",
			},
		},
		{
			name: "--add sets TagOpAdd",
			args: []string{"--add", "test-note", "inbox"},
			want: &settagArgs{
				Op:   sync15.TagOpAdd,
				Tags: []string{"inbox"},
				Path: "test-note",
			},
		},
		{
			name: "--remove sets TagOpRemove",
			args: []string{"--remove", "test-note", "inbox"},
			want: &settagArgs{
				Op:   sync15.TagOpRemove,
				Tags: []string{"inbox"},
				Path: "test-note",
			},
		},
		{
			name: "--if-revision captured",
			args: []string{"--if-revision=abc123", "test-note", "inbox"},
			want: &settagArgs{
				Op:               sync15.TagOpReplace,
				Tags:             []string{"inbox"},
				Path:             "test-note",
				ExpectedRevision: "abc123",
			},
		},
		{
			name: "flags in one order",
			args: []string{"--if-revision=rev1", "--add", "test-note", "inbox,urgent"},
			want: &settagArgs{
				Op:               sync15.TagOpAdd,
				Tags:             []string{"inbox", "urgent"},
				Path:             "test-note",
				ExpectedRevision: "rev1",
			},
		},
		{
			name: "same flags in a different order",
			args: []string{"--add", "--if-revision=rev1", "test-note", "inbox,urgent"},
			want: &settagArgs{
				Op:               sync15.TagOpAdd,
				Tags:             []string{"inbox", "urgent"},
				Path:             "test-note",
				ExpectedRevision: "rev1",
			},
		},
		{
			name: "tags containing spaces still parse",
			args: []string{"test-note", "Project Alpha, Reading List"},
			want: &settagArgs{
				Op:   sync15.TagOpReplace,
				Tags: []string{"Project Alpha", "Reading List"},
				Path: "test-note",
			},
		},
		{
			name: "flags after positionals still parse (pflag interspersed)",
			args: []string{"test-note", "inbox", "--add"},
			want: &settagArgs{
				Op:   sync15.TagOpAdd,
				Tags: []string{"inbox"},
				Path: "test-note",
			},
		},
		{
			name:    "two op flags is an error",
			args:    []string{"--add", "--remove", "test-note", "inbox"},
			wantErr: "choose one of --add, --remove",
		},
		{
			name:    "--replace is not a flag anymore; replace is the default",
			args:    []string{"--replace", "test-note", "inbox"},
			wantErr: "unknown flag: --replace",
		},
		{
			name:    "--if-revision with no value is an error",
			args:    []string{"--add", "--if-revision"},
			wantErr: "flag needs an argument: --if-revision",
		},
		{
			name:    "missing path and tags",
			args:    []string{},
			wantErr: settagUsage,
		},
		{
			name:    "missing tags",
			args:    []string{"test-note"},
			wantErr: settagUsage,
		},
		{
			name:    "too many positional arguments; tags are not joined",
			args:    []string{"test-note", "a", "b"},
			wantErr: settagUsage,
		},
		{
			name:    "--json is not a settag flag; JSON output is chosen globally",
			args:    []string{"--json", "test-note", "inbox"},
			wantErr: "unknown flag: --json",
		},
		{
			name:    "unknown flag is an error",
			args:    []string{"--bogus", "test-note", "inbox"},
			wantErr: "unknown flag: --bogus",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSettagArgs(tt.args)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tt.wantErr, err.Error())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

type mockApiCtx struct {
	ft                 *filetree.FileTreeCtx
	syncCompleteCalled bool

	called           bool
	docID            string
	op               sync15.TagOp
	tags             []string
	expectedRevision string
	result           *sync15.TagUpdateResult
	err              error
}

func (m *mockApiCtx) Filetree() *filetree.FileTreeCtx           { return m.ft }
func (m *mockApiCtx) FetchDocument(docId, dstPath string) error { return nil }
func (m *mockApiCtx) CreateDir(parentId, name string, notify bool) (*model.Document, error) {
	return nil, nil
}
func (m *mockApiCtx) UploadDocument(parentId string, sourceDocPath string, notify bool, coverpage *int, currentPage *int, pageCount *int, contrastFilter *string) (*model.Document, error) {
	return nil, nil
}
func (m *mockApiCtx) ReplaceDocumentFile(docId, sourceDocPath string, notify bool) error { return nil }
func (m *mockApiCtx) MoveEntry(src, dstDir *model.Node, name string) (*model.Node, error) {
	return nil, nil
}
func (m *mockApiCtx) DeleteEntry(node *model.Node, recursive, notify bool) error { return nil }
func (m *mockApiCtx) UpdateDocumentTags(docId string, op sync15.TagOp, tags []string, expectedRevision string) (*sync15.TagUpdateResult, error) {
	m.called = true
	m.docID = docId
	m.op = op
	m.tags = tags
	m.expectedRevision = expectedRevision

	if m.err != nil {
		// The real ApiCtx.UpdateDocumentTags returns (result, err) together on
		// a superseded/not-committed readback failure, so callers keep
		// AfterRevision; m.result lets a test simulate that.
		return m.result, m.err
	}
	if m.result != nil {
		return m.result, nil
	}
	return &sync15.TagUpdateResult{
		DocumentID:     docId,
		Operation:      op.String(),
		Changed:        true,
		BeforeRevision: "before-rev",
		AfterRevision:  "after-rev",
		BeforeTags:     []string{},
		AfterTags:      tags,
		PageTagCount:   0,
	}, nil
}
func (m *mockApiCtx) SyncComplete() error {
	m.syncCompleteCalled = true
	return nil
}
func (m *mockApiCtx) Nuke() error                     { return nil }
func (m *mockApiCtx) Refresh() (string, int64, error) { return "", 0, nil }

func newSettagTestFixture() (*filetree.FileTreeCtx, *mockApiCtx, *api.UserInfo) {
	ft := filetree.CreateFileTreeCtx()
	ft.AddDocument(&model.Document{
		ID:   "doc-uuid-123",
		Name: "test-note",
		Type: model.DocumentType,
	})
	ft.AddDocument(&model.Document{
		ID:   "folder-uuid-456",
		Name: "a-folder",
		Type: model.DirectoryType,
	})
	ft.FinishAdd()

	mock := &mockApiCtx{
		ft: &ft,
	}

	userInfo := &api.UserInfo{
		User:        "test@example.com",
		SyncVersion: api.Version15,
	}

	return &ft, mock, userInfo
}

func TestSettagCommand(t *testing.T) {
	ft, mock, userInfo := newSettagTestFixture()

	err := RunShell(mock, userInfo, []string{"settag", "test-note", "inbox,urgent"}, false)
	require.NoError(t, err)

	require.True(t, mock.called)
	assert.Equal(t, "doc-uuid-123", mock.docID)
	assert.Equal(t, sync15.TagOpReplace, mock.op)
	assert.Equal(t, []string{"inbox", "urgent"}, mock.tags)
	assert.Equal(t, "", mock.expectedRevision)
	assert.True(t, mock.syncCompleteCalled)

	node, err := ft.NodeByPath("test-note", ft.Root())
	require.NoError(t, err)
	assert.Equal(t, []string{"inbox", "urgent"}, node.Document.Tags)
}

func TestSettagCommandPassesOpAndRevision(t *testing.T) {
	_, mock, userInfo := newSettagTestFixture()

	err := RunShell(mock, userInfo, []string{"settag", "--add", "--if-revision=rev-before", "test-note", "urgent"}, false)
	require.NoError(t, err)

	require.True(t, mock.called)
	assert.Equal(t, sync15.TagOpAdd, mock.op)
	assert.Equal(t, []string{"urgent"}, mock.tags)
	assert.Equal(t, "rev-before", mock.expectedRevision)
}

func TestSettagCommandNoOpDoesNotSync(t *testing.T) {
	_, mock, userInfo := newSettagTestFixture()
	mock.result = &sync15.TagUpdateResult{
		DocumentID:     "doc-uuid-123",
		Operation:      "replace",
		Changed:        false,
		BeforeRevision: "same-rev",
		AfterRevision:  "same-rev",
		BeforeTags:     []string{"inbox"},
		AfterTags:      []string{"inbox"},
	}

	err := RunShell(mock, userInfo, []string{"settag", "test-note", "inbox"}, false)
	require.NoError(t, err)

	assert.False(t, mock.syncCompleteCalled)
}

func TestSettagCommandChangedCallsSync(t *testing.T) {
	_, mock, userInfo := newSettagTestFixture()
	mock.result = &sync15.TagUpdateResult{
		DocumentID:     "doc-uuid-123",
		Operation:      "replace",
		Changed:        true,
		BeforeRevision: "rev-a",
		AfterRevision:  "rev-b",
		BeforeTags:     []string{},
		AfterTags:      []string{"inbox"},
	}

	err := RunShell(mock, userInfo, []string{"settag", "test-note", "inbox"}, false)
	require.NoError(t, err)

	assert.True(t, mock.syncCompleteCalled)
}

func TestSettagCommandJSONOutput(t *testing.T) {
	_, mock, userInfo := newSettagTestFixture()
	mock.result = &sync15.TagUpdateResult{
		DocumentID:     "doc-uuid-123",
		Operation:      "replace",
		Changed:        true,
		BeforeRevision: "rev-a",
		AfterRevision:  "rev-b",
		BeforeTags:     nil, // must still serialize as [], not null
		AfterTags:      []string{"inbox"},
		PageTagCount:   0,
		ContentHash:    "c0c0",
		MetadataHash:   "0a0a",
	}

	var runErr error
	out := captureStdout(t, func() {
		runErr = RunShell(mock, userInfo, []string{"settag", "test-note", "inbox"}, true)
	})
	require.NoError(t, runErr)

	require.True(t, mock.called)
	assert.True(t, mock.syncCompleteCalled)

	for _, field := range []string{
		`"documentId"`, `"operation"`, `"changed"`, `"beforeRevision"`, `"afterRevision"`,
		`"beforeTags"`, `"afterTags"`, `"pageTagCount"`, `"contentHash"`, `"metadataHash"`,
	} {
		assert.Contains(t, out, field, "JSON output must contain the %s field", field)
	}
	assert.Contains(t, out, `"beforeTags": []`, "an empty tag list must serialize as [], not null")
	assert.NotContains(t, out, `"beforeTags": null`)
}

func TestSettagCommandErrorNonJSONPrintsFailedToSetTags(t *testing.T) {
	_, mock, userInfo := newSettagTestFixture()
	mock.err = &sync15.StaleRevisionError{DocumentID: "doc-uuid-123", Expected: "rev-a", Actual: "rev-b"}

	err := RunShell(mock, userInfo, []string{"settag", "test-note", "inbox"}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to set tags")
}

func TestSettagCommandErrorJSONClassifiesSupersededAndNotCommittedAndKeepsTheResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		kind string
	}{
		{"superseded", &sync15.SupersededError{DocumentID: "doc-uuid-123", ExpectedRevision: "mine", ActualRevision: "theirs"}, "superseded"},
		{"not committed", &sync15.NotCommittedError{DocumentID: "doc-uuid-123", ExpectedRevision: "mine", ActualRevision: "original"}, "not_committed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, mock, userInfo := newSettagTestFixture()
			mock.err = tc.err
			mock.result = &sync15.TagUpdateResult{DocumentID: "doc-uuid-123", AfterRevision: "mine"}

			var runErr error
			out := captureStdout(t, func() {
				runErr = RunShell(mock, userInfo, []string{"settag", "test-note", "inbox"}, true)
			})
			require.Error(t, runErr)

			var got map[string]interface{}
			require.NoError(t, json.Unmarshal([]byte(out), &got), "stdout must be one JSON object: %s", out)
			assert.Equal(t, tc.kind, got["kind"])
			result, ok := got["result"].(map[string]interface{})
			require.True(t, ok, "result must still be populated so callers keep AfterRevision")
			assert.Equal(t, "mine", result["afterRevision"])
		})
	}
}

func TestSettagCommandErrorJSONPrintsStructuredErrorObject(t *testing.T) {
	_, mock, userInfo := newSettagTestFixture()
	mock.err = &sync15.StaleRevisionError{DocumentID: "doc-uuid-123", Expected: "rev-a", Actual: "rev-b"}

	var runErr error
	out := captureStdout(t, func() {
		runErr = RunShell(mock, userInfo, []string{"settag", "test-note", "inbox"}, true)
	})
	require.Error(t, runErr)

	var got map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &got), "stdout must be one JSON object: %s", out)
	assert.Equal(t, "stale_revision", got["kind"])
	assert.Equal(t, "doc-uuid-123", got["documentId"])
	assert.Equal(t, "rev-a", got["expectedRevision"])
	assert.Equal(t, "rev-b", got["actualRevision"])
	assert.Contains(t, got["error"], "doc-uuid-123")
}

func TestSettagCommandRefusesAFolder(t *testing.T) {
	_, mock, userInfo := newSettagTestFixture()

	err := RunShell(mock, userInfo, []string{"settag", "a-folder", "inbox"}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot set tags on a folder")

	assert.False(t, mock.called, "the API must not be called for a folder")
}
