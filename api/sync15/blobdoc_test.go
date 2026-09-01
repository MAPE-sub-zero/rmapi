package sync15

import (
	"encoding/json"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/juruen/rmapi/archive"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpdateDocumentTags(t *testing.T) {
	docID := "test-doc-uuid"
	doc := NewBlobDoc("test-doc", docID, "DocumentType", "")

	// Set initial page tags and old document tags
	initialPageTags := []archive.PageTag{
		{
			Name:      "important-page",
			PageID:    "page-uuid-1",
			Timestamp: 1600000000000,
		},
		{
			Name:      "review-later",
			PageID:    "page-uuid-2",
			Timestamp: 1600000005000,
		},
	}
	doc.Content.PageTags = initialPageTags
	doc.Content.DocumentTags = []archive.Tag{
		{
			Name:      "old-tag",
			Timestamp: 1500000000000,
		},
	}

	// Add .content and .metadata entries in d.Files
	doc.AddFile(&Entry{
		DocumentID: docID + ".content",
		Hash:       "initial-content-hash",
		Size:       100,
		Type:       FileType,
	})
	doc.AddFile(&Entry{
		DocumentID: docID + ".metadata",
		Hash:       "initial-metadata-hash",
		Size:       50,
		Type:       FileType,
	})

	beforeTime := time.Now().UnixMilli()
	newTags := []string{"urgent", "work", "research"}

	err := doc.UpdateDocumentTags(newTags)
	require.NoError(t, err)
	afterTime := time.Now().UnixMilli()

	// Verify DocumentTags
	require.Len(t, doc.Content.DocumentTags, 3)
	for i, tag := range doc.Content.DocumentTags {
		assert.Equal(t, newTags[i], tag.Name)
		assert.GreaterOrEqual(t, tag.Timestamp, beforeTime)
		assert.LessOrEqual(t, tag.Timestamp, afterTime)
	}

	// Verify PageTags are preserved
	require.Len(t, doc.Content.PageTags, 2)
	assert.Equal(t, "important-page", doc.Content.PageTags[0].Name)
	assert.Equal(t, "page-uuid-1", doc.Content.PageTags[0].PageID)
	assert.Equal(t, int64(1600000000000), doc.Content.PageTags[0].Timestamp)
	assert.Equal(t, "review-later", doc.Content.PageTags[1].Name)
	assert.Equal(t, "page-uuid-2", doc.Content.PageTags[1].PageID)
	assert.Equal(t, int64(1600000005000), doc.Content.PageTags[1].Timestamp)

	// Verify Metadata LastModified is updated to Unix millisecond timestamp
	lastMod, err := strconv.ParseInt(doc.Metadata.LastModified, 10, 64)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, lastMod, beforeTime)
	assert.LessOrEqual(t, lastMod, afterTime)
	assert.True(t, doc.Metadata.MetadataModified)

	// Verify JSON serialization of Content
	_, contentReader, err := doc.ContentHashAndReader()
	require.NoError(t, err)
	contentBytes, err := io.ReadAll(contentReader)
	require.NoError(t, err)

	var rawMap map[string]interface{}
	err = json.Unmarshal(contentBytes, &rawMap)
	require.NoError(t, err)

	// Verify "tags" JSON field
	rawTags, ok := rawMap["tags"].([]interface{})
	require.True(t, ok, "tags field should be a JSON array")
	require.Len(t, rawTags, 3)

	for i, rawTag := range rawTags {
		tagMap, ok := rawTag.(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, newTags[i], tagMap["name"])
		ts, ok := tagMap["timestamp"].(float64)
		require.True(t, ok)
		assert.GreaterOrEqual(t, int64(ts), beforeTime)
		assert.LessOrEqual(t, int64(ts), afterTime)
	}

	// Verify "pageTags" JSON field preservation
	rawPageTags, ok := rawMap["pageTags"].([]interface{})
	require.True(t, ok, "pageTags field should be a JSON array")
	require.Len(t, rawPageTags, 2)

	pageTag0 := rawPageTags[0].(map[string]interface{})
	assert.Equal(t, "important-page", pageTag0["name"])
	assert.Equal(t, "page-uuid-1", pageTag0["pageId"])
	assert.Equal(t, float64(1600000000000), pageTag0["timestamp"])

	// Verify doc.ToDocument() reflects new tags
	mDoc := doc.ToDocument()
	assert.Equal(t, newTags, mDoc.Tags)
}

func TestSetDocumentTagsEmpty(t *testing.T) {
	doc := NewBlobDoc("test-doc", "test-id", "DocumentType", "")
	doc.Content.PageTags = []archive.PageTag{
		{Name: "keep-me", PageID: "p1", Timestamp: 12345},
	}
	doc.Content.DocumentTags = []archive.Tag{
		{Name: "remove-me", Timestamp: 54321},
	}

	doc.SetDocumentTags([]string{})
	assert.Empty(t, doc.Content.DocumentTags)
	require.Len(t, doc.Content.PageTags, 1)
	assert.Equal(t, "keep-me", doc.Content.PageTags[0].Name)
}
