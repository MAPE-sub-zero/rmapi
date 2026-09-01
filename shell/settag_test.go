package shell

import (
	"testing"

	"github.com/juruen/rmapi/api"
	"github.com/juruen/rmapi/api/sync15"
	"github.com/juruen/rmapi/filetree"
	"github.com/juruen/rmapi/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
			result := ParseTags(tt.input)
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
				JSON:             false,
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
			name: "--replace sets TagOpReplace explicitly",
			args: []string{"--replace", "test-note", "inbox"},
			want: &settagArgs{
				Op:   sync15.TagOpReplace,
				Tags: []string{"inbox"},
				Path: "test-note",
			},
		},
		{
			name: "--if-revision captured",
			args: []string{"--if-revision", "abc123", "test-note", "inbox"},
			want: &settagArgs{
				Op:               sync15.TagOpReplace,
				Tags:             []string{"inbox"},
				Path:             "test-note",
				ExpectedRevision: "abc123",
			},
		},
		{
			name: "--json captured",
			args: []string{"--json", "test-note", "inbox"},
			want: &settagArgs{
				Op:   sync15.TagOpReplace,
				Tags: []string{"inbox"},
				Path: "test-note",
				JSON: true,
			},
		},
		{
			name: "flags in one order",
			args: []string{"--json", "--if-revision", "rev1", "--add", "test-note", "inbox,urgent"},
			want: &settagArgs{
				Op:               sync15.TagOpAdd,
				Tags:             []string{"inbox", "urgent"},
				Path:             "test-note",
				ExpectedRevision: "rev1",
				JSON:             true,
			},
		},
		{
			name: "same flags in a different order",
			args: []string{"--add", "--json", "--if-revision", "rev1", "test-note", "inbox,urgent"},
			want: &settagArgs{
				Op:               sync15.TagOpAdd,
				Tags:             []string{"inbox", "urgent"},
				Path:             "test-note",
				ExpectedRevision: "rev1",
				JSON:             true,
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
			name:    "two op flags is an error",
			args:    []string{"--add", "--remove", "test-note", "inbox"},
			wantErr: "choose one of --add, --remove, --replace",
		},
		{
			name:    "--replace combined with --add is also an error",
			args:    []string{"--replace", "--add", "test-note", "inbox"},
			wantErr: "choose one of --add, --remove, --replace",
		},
		{
			name:    "--if-revision with no value is an error",
			args:    []string{"--add", "--if-revision"},
			wantErr: "--if-revision needs a revision",
		},
		{
			name:    "missing path and tags",
			args:    []string{},
			wantErr: "missing path and/or tags",
		},
		{
			name:    "missing tags",
			args:    []string{"test-note"},
			wantErr: "missing path and/or tags",
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
	updatedDocID       string
	updatedTags        []string
	syncCompleteCalled bool

	withOptionsCalled           bool
	withOptionsDocID            string
	withOptionsOp               sync15.TagOp
	withOptionsTags             []string
	withOptionsExpectedRevision string
	withOptionsResult           *sync15.TagUpdateResult
	withOptionsErr              error
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
func (m *mockApiCtx) UpdateDocumentTags(docId string, tags []string) error {
	m.updatedDocID = docId
	m.updatedTags = tags
	return nil
}
func (m *mockApiCtx) UpdateDocumentTagsWithOptions(docId string, op sync15.TagOp, tags []string, expectedRevision string) (*sync15.TagUpdateResult, error) {
	m.withOptionsCalled = true
	m.withOptionsDocID = docId
	m.withOptionsOp = op
	m.withOptionsTags = tags
	m.withOptionsExpectedRevision = expectedRevision

	if m.withOptionsErr != nil {
		return nil, m.withOptionsErr
	}
	if m.withOptionsResult != nil {
		return m.withOptionsResult, nil
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

	require.True(t, mock.withOptionsCalled)
	assert.Equal(t, "doc-uuid-123", mock.withOptionsDocID)
	assert.Equal(t, sync15.TagOpReplace, mock.withOptionsOp)
	assert.Equal(t, []string{"inbox", "urgent"}, mock.withOptionsTags)
	assert.Equal(t, "", mock.withOptionsExpectedRevision)
	assert.True(t, mock.syncCompleteCalled)

	node, err := ft.NodeByPath("test-note", ft.Root())
	require.NoError(t, err)
	assert.Equal(t, []string{"inbox", "urgent"}, node.Document.Tags)
}

func TestSettagCommandPassesOpAndRevision(t *testing.T) {
	_, mock, userInfo := newSettagTestFixture()

	err := RunShell(mock, userInfo, []string{"settag", "--add", "--if-revision", "rev-before", "test-note", "urgent"}, false)
	require.NoError(t, err)

	require.True(t, mock.withOptionsCalled)
	assert.Equal(t, sync15.TagOpAdd, mock.withOptionsOp)
	assert.Equal(t, []string{"urgent"}, mock.withOptionsTags)
	assert.Equal(t, "rev-before", mock.withOptionsExpectedRevision)
}

func TestSettagCommandNoOpDoesNotSync(t *testing.T) {
	_, mock, userInfo := newSettagTestFixture()
	mock.withOptionsResult = &sync15.TagUpdateResult{
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
	mock.withOptionsResult = &sync15.TagUpdateResult{
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
