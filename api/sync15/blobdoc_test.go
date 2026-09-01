package sync15

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
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

// --- Tag algebra: replace / add / remove, with idempotency ------------------

func tagNames(tags []archive.Tag) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		out = append(out, t.Name)
	}
	return out
}

func TestApplyTagsReplaceKeepsTimestampsOfSurvivingTags(t *testing.T) {
	existing := []archive.Tag{{Name: "ops", Timestamp: 100}, {Name: "old", Timestamp: 200}}
	got, changed := ApplyTags(existing, TagOpReplace, []string{"ops", "new"}, 999)
	if !changed {
		t.Fatal("replacing with a different set must report changed")
	}
	if diff := strings.Join(tagNames(got), ","); diff != "ops,new" {
		t.Fatalf("names = %q, want \"ops,new\"", diff)
	}
	if got[0].Timestamp != 100 {
		t.Errorf("surviving tag must keep its timestamp, got %d", got[0].Timestamp)
	}
	if got[1].Timestamp != 999 {
		t.Errorf("new tag must take the supplied timestamp, got %d", got[1].Timestamp)
	}
}

func TestApplyTagsIsIdempotent(t *testing.T) {
	existing := []archive.Tag{{Name: "ops", Timestamp: 100}}
	for _, op := range []TagOp{TagOpReplace, TagOpAdd} {
		got, changed := ApplyTags(existing, op, []string{"ops"}, 999)
		if changed {
			t.Errorf("op %v: re-applying the same tag must report changed=false", op)
		}
		if got[0].Timestamp != 100 {
			t.Errorf("op %v: timestamp must not churn on a no-op, got %d", op, got[0].Timestamp)
		}
	}
	got, changed := ApplyTags(existing, TagOpRemove, []string{"absent"}, 999)
	if changed {
		t.Error("removing an absent tag must report changed=false")
	}
	if len(got) != 1 {
		t.Errorf("removing an absent tag must not alter the set, got %v", tagNames(got))
	}
}

func TestApplyTagsAddAppendsOnlyMissing(t *testing.T) {
	existing := []archive.Tag{{Name: "ops", Timestamp: 100}}
	got, changed := ApplyTags(existing, TagOpAdd, []string{"ops", "review", "review"}, 999)
	if !changed {
		t.Fatal("adding a new tag must report changed")
	}
	if names := strings.Join(tagNames(got), ","); names != "ops,review" {
		t.Fatalf("names = %q, want \"ops,review\" (existing kept, input de-duplicated)", names)
	}
}

func TestApplyTagsRemoveDropsOnlyNamed(t *testing.T) {
	existing := []archive.Tag{{Name: "ops", Timestamp: 100}, {Name: "review", Timestamp: 200}}
	got, changed := ApplyTags(existing, TagOpRemove, []string{"review"}, 999)
	if !changed {
		t.Fatal("removing a present tag must report changed")
	}
	if names := strings.Join(tagNames(got), ","); names != "ops" {
		t.Fatalf("names = %q, want \"ops\"", names)
	}
	if got[0].Timestamp != 100 {
		t.Errorf("untouched tag must keep its timestamp, got %d", got[0].Timestamp)
	}
}

func TestApplyDocumentTagsLeavesPageTagsAndTimestampAloneOnNoOp(t *testing.T) {
	doc := &BlobDoc{}
	doc.Content.DocumentTags = []archive.Tag{{Name: "ops", Timestamp: 100}}
	doc.Content.PageTags = []archive.PageTag{{Name: "star", PageID: "p1", Timestamp: 50}}
	doc.Metadata.LastModified = "12345"

	changed := doc.ApplyDocumentTags(TagOpAdd, []string{"ops"}, 999)
	if changed {
		t.Fatal("no-op must report changed=false")
	}
	if doc.Metadata.LastModified != "12345" {
		t.Errorf("a no-op must not bump LastModified, got %s", doc.Metadata.LastModified)
	}
	if len(doc.Content.PageTags) != 1 || doc.Content.PageTags[0].Name != "star" {
		t.Errorf("page tags must survive untouched, got %+v", doc.Content.PageTags)
	}
}

func TestApplyDocumentTagsPreservesPageTagsOnChange(t *testing.T) {
	doc := &BlobDoc{}
	doc.Content.PageTags = []archive.PageTag{{Name: "star", PageID: "p1", Timestamp: 50}}

	if changed := doc.ApplyDocumentTags(TagOpReplace, []string{"ops"}, 999); !changed {
		t.Fatal("adding a tag to an empty set must report changed")
	}
	if len(doc.Content.PageTags) != 1 || doc.Content.PageTags[0] != (archive.PageTag{Name: "star", PageID: "p1", Timestamp: 50}) {
		t.Errorf("page tags must be byte-identical after a document-tag write, got %+v", doc.Content.PageTags)
	}
	if doc.Metadata.LastModified != "999" {
		t.Errorf("a real change must bump LastModified, got %s", doc.Metadata.LastModified)
	}
}

// --- Stale-revision precondition, no-op detection, structured result -------

func docWithTags(revision string, tags ...archive.Tag) *BlobDoc {
	d := &BlobDoc{}
	d.Hash = revision
	d.DocumentID = "doc-1"
	d.Content.DocumentTags = tags
	d.Content.PageTags = []archive.PageTag{{Name: "star", PageID: "p1", Timestamp: 50}}
	d.Files = []*Entry{
		{DocumentID: "doc-1.content", Type: FileType},
		{DocumentID: "doc-1.metadata", Type: FileType},
	}
	return d
}

func TestPlanTagUpdateRejectsAStaleRevision(t *testing.T) {
	doc := docWithTags("actual-rev", archive.Tag{Name: "ops", Timestamp: 100})
	_, err := PlanTagUpdate(doc, TagOpAdd, []string{"review"}, "revision-the-caller-saw", 999)

	var stale *StaleRevisionError
	if !errors.As(err, &stale) {
		t.Fatalf("want StaleRevisionError, got %v", err)
	}
	if stale.Expected != "revision-the-caller-saw" || stale.Actual != "actual-rev" {
		t.Errorf("error must name both revisions, got %+v", stale)
	}
	if len(doc.Content.DocumentTags) != 1 {
		t.Error("a rejected precondition must leave the document unmodified")
	}
}

func TestPlanTagUpdateAcceptsAMatchingRevision(t *testing.T) {
	doc := docWithTags("rev-1", archive.Tag{Name: "ops", Timestamp: 100})
	plan, err := PlanTagUpdate(doc, TagOpAdd, []string{"review"}, "rev-1", 999)
	if err != nil {
		t.Fatalf("matching revision must be accepted: %v", err)
	}
	if !plan.Result.Changed {
		t.Error("adding a new tag must report Changed")
	}
}

func TestPlanTagUpdateSkipsThePreconditionWhenUnset(t *testing.T) {
	doc := docWithTags("rev-1", archive.Tag{Name: "ops", Timestamp: 100})
	if _, err := PlanTagUpdate(doc, TagOpAdd, []string{"review"}, "", 999); err != nil {
		t.Fatalf("an empty expected revision must skip the check: %v", err)
	}
}

func TestPlanTagUpdateReportsANoOpWithoutTouchingTheDocument(t *testing.T) {
	doc := docWithTags("rev-1", archive.Tag{Name: "ops", Timestamp: 100})
	doc.Metadata.LastModified = "12345"

	plan, err := PlanTagUpdate(doc, TagOpAdd, []string{"ops"}, "", 999)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Result.Changed {
		t.Error("re-adding an existing tag must report Changed=false")
	}
	if plan.ContentReader != nil || plan.MetadataReader != nil {
		t.Error("a no-op must produce nothing to upload")
	}
	if doc.Metadata.LastModified != "12345" {
		t.Error("a no-op must not bump LastModified")
	}
	if plan.Result.BeforeRevision != plan.Result.AfterRevision {
		t.Error("a no-op must not change the revision")
	}
}

func TestPlanTagUpdateReportsBeforeAndAfterState(t *testing.T) {
	doc := docWithTags("rev-1", archive.Tag{Name: "ops", Timestamp: 100})
	plan, err := PlanTagUpdate(doc, TagOpAdd, []string{"review"}, "", 999)
	if err != nil {
		t.Fatal(err)
	}
	r := plan.Result
	if strings.Join(r.BeforeTags, ",") != "ops" {
		t.Errorf("BeforeTags = %v, want [ops]", r.BeforeTags)
	}
	if strings.Join(r.AfterTags, ",") != "ops,review" {
		t.Errorf("AfterTags = %v, want [ops review]", r.AfterTags)
	}
	if r.BeforeRevision != "rev-1" {
		t.Errorf("BeforeRevision = %q, want rev-1", r.BeforeRevision)
	}
	if r.AfterRevision == "" || r.AfterRevision == r.BeforeRevision {
		t.Errorf("AfterRevision must be recomputed, got %q", r.AfterRevision)
	}
	if r.ContentHash == "" || r.MetadataHash == "" {
		t.Error("result must carry the uploaded blob hashes for verification")
	}
	if r.PageTagCount != 1 {
		t.Errorf("PageTagCount = %d, want 1", r.PageTagCount)
	}
	if plan.ContentReader == nil || plan.MetadataReader == nil {
		t.Error("a real change must produce both blobs to upload")
	}
}

func TestVerifyTagWritePayloadDetectsAMismatchedContentBlob(t *testing.T) {
	// Readback: what we are about to upload must deserialise to what we intended.
	good, _ := json.Marshal(archive.Content{DocumentTags: []archive.Tag{{Name: "ops", Timestamp: 1}}})
	if err := VerifyTagWritePayload(good, []string{"ops"}, 0); err != nil {
		t.Fatalf("a matching payload must verify: %v", err)
	}
	if err := VerifyTagWritePayload(good, []string{"ops", "review"}, 0); err == nil {
		t.Error("a payload whose tags differ from the intent must fail verification")
	}
	bad, _ := json.Marshal(archive.Content{
		DocumentTags: []archive.Tag{{Name: "ops", Timestamp: 1}},
		PageTags:     []archive.PageTag{{Name: "star", PageID: "p1"}},
	})
	if err := VerifyTagWritePayload(bad, []string{"ops"}, 0); err == nil {
		t.Error("a payload that gained page tags must fail verification")
	}
}
