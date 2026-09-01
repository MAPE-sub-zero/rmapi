package shell

import (
	"testing"

	"github.com/juruen/rmapi/api"
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

type mockApiCtx struct {
	ft                 *filetree.FileTreeCtx
	updatedDocID       string
	updatedTags        []string
	syncCompleteCalled bool
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
func (m *mockApiCtx) SyncComplete() error {
	m.syncCompleteCalled = true
	return nil
}
func (m *mockApiCtx) Nuke() error                     { return nil }
func (m *mockApiCtx) Refresh() (string, int64, error) { return "", 0, nil }

func TestSettagCommand(t *testing.T) {
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

	err := RunShell(mock, userInfo, []string{"settag", "test-note", "inbox,urgent"}, false)
	require.NoError(t, err)

	assert.Equal(t, "doc-uuid-123", mock.updatedDocID)
	assert.Equal(t, []string{"inbox", "urgent"}, mock.updatedTags)
	assert.True(t, mock.syncCompleteCalled)

	node, err := ft.NodeByPath("test-note", ft.Root())
	require.NoError(t, err)
	assert.Equal(t, []string{"inbox", "urgent"}, node.Document.Tags)
}
