package sync15

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/juruen/rmapi/archive"
	"github.com/juruen/rmapi/config"
	"github.com/juruen/rmapi/model"
	"github.com/juruen/rmapi/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Tag algebra: replace / add / remove, with idempotency ------------------

func TestApplyTagsReplaceKeepsTimestampsOfSurvivingTags(t *testing.T) {
	existing := []archive.Tag{{Name: "ops", Timestamp: 100}, {Name: "old", Timestamp: 200}}
	got, changed := applyTags(existing, TagOpReplace, []string{"ops", "new"}, 999)
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
		got, changed := applyTags(existing, op, []string{"ops"}, 999)
		if changed {
			t.Errorf("op %v: re-applying the same tag must report changed=false", op)
		}
		if got[0].Timestamp != 100 {
			t.Errorf("op %v: timestamp must not churn on a no-op, got %d", op, got[0].Timestamp)
		}
	}
	got, changed := applyTags(existing, TagOpRemove, []string{"absent"}, 999)
	if changed {
		t.Error("removing an absent tag must report changed=false")
	}
	if len(got) != 1 {
		t.Errorf("removing an absent tag must not alter the set, got %v", tagNames(got))
	}
}

func TestApplyTagsAddAppendsOnlyMissing(t *testing.T) {
	existing := []archive.Tag{{Name: "ops", Timestamp: 100}}
	got, changed := applyTags(existing, TagOpAdd, []string{"ops", "review", "review"}, 999)
	if !changed {
		t.Fatal("adding a new tag must report changed")
	}
	if names := strings.Join(tagNames(got), ","); names != "ops,review" {
		t.Fatalf("names = %q, want \"ops,review\" (existing kept, input de-duplicated)", names)
	}
}

func TestApplyTagsRemoveDropsOnlyNamed(t *testing.T) {
	existing := []archive.Tag{{Name: "ops", Timestamp: 100}, {Name: "review", Timestamp: 200}}
	got, changed := applyTags(existing, TagOpRemove, []string{"review"}, 999)
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

// --- Fixtures -----------------------------------------------------------------
//
// A tag write must leave every byte of the document it did not mean to change
// exactly as it found it. rmapi's archive.Content / archive.MetadataFile are
// partial models of the on-device files, so serialising them back over an
// existing document drops every key the model does not know (customZoom*,
// documentMetadata, formatVersion, per-page template, createdTime, source, ...)
// and invents keys the file never had. These fixtures are authored from the
// public firmware-3.x file layout, not copied from any real document.

const preservationContent = `{
    "cPages": {
        "lastOpened": {"timestamp": "1:2", "value": "pg-1"},
        "original": {"timestamp": "1:1", "value": -1},
        "pages": [
            {"id": "pg-1", "idx": {"timestamp": "1:2", "value": "ba"}, "template": {"timestamp": "1:1", "value": "Blank"}}
        ],
        "uuids": [{"first": "abc", "second": 1}]
    },
    "coverPageNumber": 0,
    "customZoomCenterX": 0,
    "customZoomCenterY": 936,
    "customZoomOrientation": "portrait",
    "customZoomPageHeight": 1872,
    "customZoomPageWidth": 1404,
    "customZoomScale": 1,
    "documentMetadata": {"authors": ["Sample Author"], "title": "Field notes"},
    "extraMetadata": {},
    "fileType": "notebook",
    "fontName": "",
    "formatVersion": 2,
    "lineHeight": -1,
    "margins": 125,
    "orientation": "portrait",
    "pageCount": 1,
    "pageTags": [{"name": "urgent", "pageId": "pg-1", "timestamp": 5}],
    "sizeInBytes": "12345",
    "tags": [{"name": "ops", "timestamp": 1}],
    "textAlignment": "justify",
    "textScale": 1,
    "zoomMode": "bestFit"
}`

const preservationMetadata = `{
    "createdTime": "1700000000000",
    "deleted": false,
    "lastModified": "1700000001000",
    "lastOpened": "1700000002000",
    "lastOpenedPage": 0,
    "metadatamodified": false,
    "modified": false,
    "new": false,
    "parent": "",
    "pinned": false,
    "source": "com.remarkable.macos",
    "synced": true,
    "type": "DocumentType",
    "version": 3,
    "visibleName": "Field notes"
}`

type fakeRemote struct{ blobs map[string]string }

func (f fakeRemote) GetRootIndex() (string, int64, error) { return "", 0, nil }
func (f fakeRemote) GetReader(hash, name string) (io.ReadCloser, error) {
	b, ok := f.blobs[hash]
	if !ok {
		return nil, errors.New("no blob " + hash)
	}
	return io.NopCloser(strings.NewReader(b)), nil
}

// topLevelWithout parses a JSON object into key → raw value bytes and drops
// one key, so two documents can be compared on everything except the member a
// write was supposed to change.
func topLevelWithout(t *testing.T, raw []byte, drop string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m), "document must parse: %s", raw)
	delete(m, drop)
	return m
}

// contentTags parses the top-level "tags" member of a .content blob.
func contentTags(t *testing.T, raw []byte) []archive.Tag {
	t.Helper()
	var tags []archive.Tag
	require.NoError(t, json.Unmarshal(topLevelWithout(t, raw, "")["tags"], &tags))
	return tags
}

// loadPreservationDoc builds a BlobDoc the way the tree does after mirroring:
// entries with hashes, plus ReadMetadata run over each file so the parsed
// Content / Metadata are populated exactly as they would be for a real
// document.
func loadPreservationDoc(t *testing.T) *BlobDoc {
	t.Helper()
	doc := &BlobDoc{Entry: Entry{DocumentID: "doc-1", Hash: "ffff", Type: "DocumentType"}}
	doc.Files = []*Entry{
		{DocumentID: "doc-1.content", Hash: "c0c0", Size: int64(len(preservationContent))},
		{DocumentID: "doc-1.metadata", Hash: "0a0a", Size: int64(len(preservationMetadata))},
		{DocumentID: "doc-1/pg-1.rm", Hash: "0b0b", Size: 4096},
	}
	remote := fakeRemote{blobs: map[string]string{
		"c0c0": preservationContent,
		"0a0a": preservationMetadata,
	}}
	for _, f := range doc.Files {
		require.NoError(t, doc.ReadMetadata(f, remote))
	}
	return doc
}

func planOn(t *testing.T, doc *BlobDoc, op TagOp, names []string, expected string) (*tagUpdatePlan, error) {
	t.Helper()
	return planTagUpdate(doc, doc.Files, []byte(preservationContent), []byte(preservationMetadata), op, names, expected, 42)
}

// --- Write-path preservation (the RMAPI-SETTAG-001 "never destroy ink" rule) ---

func TestTagWritePreservesEveryOtherKeyInContentAndMetadata(t *testing.T) {
	var compacted bytes.Buffer
	require.NoError(t, json.Compact(&compacted, []byte(preservationContent)))

	cases := []struct {
		name    string
		content []byte
		tag     string
	}{
		{"pretty-printed fixture", []byte(preservationContent), "reviewed"},
		{"compacted fixture", compacted.Bytes(), "reviewed"},
		{"non-ASCII tag name", []byte(preservationContent), "réunion·2026"},
		{"tag name with an embedded quote", []byte(preservationContent), `a"b`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := loadPreservationDoc(t)
			plan, err := planTagUpdate(doc, doc.Files, tc.content, []byte(preservationMetadata), TagOpAdd, []string{tc.tag}, "", 42)
			require.NoError(t, err)
			require.True(t, plan.Result.Changed)
			require.True(t, json.Valid(plan.Content), "spliced content must still parse")

			// .content: only "tags" may differ, and it must say what we meant.
			assert.Equal(t,
				topLevelWithout(t, tc.content, "tags"),
				topLevelWithout(t, plan.Content, "tags"),
				".content: a tag write changed something other than tags")
			assert.Contains(t, tagNames(contentTags(t, plan.Content)), tc.tag)

			// .metadata: only "lastModified" may differ (Matt, 2026-09-01: a
			// tag write bumps lastModified the way the official app does).
			assert.Equal(t,
				topLevelWithout(t, []byte(preservationMetadata), "lastModified"),
				topLevelWithout(t, plan.Metadata, "lastModified"),
				".metadata: a tag write changed something other than lastModified")
			assert.Equal(t, `"42"`, string(topLevelWithout(t, plan.Metadata, "")["lastModified"]))
		})
	}
}

func TestTagWriteIsByteIdenticalOutsideTheSplicedMember(t *testing.T) {
	// Map-level equality would accept re-serialised bytes (reordered keys,
	// collapsed whitespace). The rule is stronger: nothing outside the one
	// member moves at all.
	doc := loadPreservationDoc(t)
	plan, err := planOn(t, doc, TagOpAdd, []string{"reviewed"}, "")
	require.NoError(t, err)

	const tagsValue = `[{"name": "ops", "timestamp": 1}]`
	tagsStart := strings.Index(preservationContent, `"tags": `+tagsValue)
	require.True(t, tagsStart > 0)
	prefix := preservationContent[:tagsStart+len(`"tags": `)]
	suffix := preservationContent[tagsStart+len(`"tags": `)+len(tagsValue):]
	assert.True(t, bytes.HasPrefix(plan.Content, []byte(prefix)), "bytes before the tags member changed")
	assert.True(t, bytes.HasSuffix(plan.Content, []byte(suffix)), "bytes after the tags member changed")

	lmStart := strings.Index(preservationMetadata, `"lastModified": `)
	require.True(t, lmStart > 0)
	mPrefix := preservationMetadata[:lmStart+len(`"lastModified": `)]
	mSuffix := preservationMetadata[lmStart+len(`"lastModified": "1700000001000"`):]
	assert.True(t, bytes.HasPrefix(plan.Metadata, []byte(mPrefix)), "bytes before lastModified changed")
	assert.True(t, bytes.HasSuffix(plan.Metadata, []byte(mSuffix)), "bytes after lastModified changed")
}

func TestTagWriteKeepsSurvivingTagBytesVerbatim(t *testing.T) {
	// A surviving tag may carry fields this rmapi does not model. They ride
	// through untouched, as do the survivor's original timestamp and spacing.
	raw := []byte(`{"tags":[{"name":"ops","timestamp":1, "colour":"red"},{"name":"old","timestamp":2}],"pageTags":[]}`)
	got, err := replaceContentTags(raw, TagOpReplace, []string{"ops", "new"}, 42)
	require.NoError(t, err)
	require.True(t, got.Changed)
	assert.Contains(t, string(got.Bytes), `{"name":"ops","timestamp":1, "colour":"red"}`)
	assert.NotContains(t, string(got.Bytes), `"old"`)
	assert.Equal(t, []string{"ops", "new"}, tagNames(contentTags(t, got.Bytes)))
	assert.Equal(t, int64(42), contentTags(t, got.Bytes)[1].Timestamp)
}

func TestTagWriteLeavesPageTagsUntouched(t *testing.T) {
	doc := loadPreservationDoc(t)
	plan, err := planOn(t, doc, TagOpReplace, []string{}, "")
	require.NoError(t, err)
	require.True(t, plan.Result.Changed, "clearing the only tag is a change")
	assert.Empty(t, contentTags(t, plan.Content))
	assert.Equal(t,
		string(topLevelWithout(t, []byte(preservationContent), "")["pageTags"]),
		string(topLevelWithout(t, plan.Content, "")["pageTags"]))
	assert.Equal(t, 1, plan.Result.PageTagCount)
}

func TestTagWriteAddsTheMemberWhenTheDocumentHasNone(t *testing.T) {
	// Older documents have no "tags" member at all; the write must add one and
	// nothing else. Both an empty object and a populated one.
	for _, raw := range []string{`{}`, `{ }`, `{"fileType":"pdf","pageTags":[]}`, "{\n  \"fileType\": \"pdf\"\n}"} {
		got, err := replaceContentTags([]byte(raw), TagOpAdd, []string{"x"}, 7)
		require.NoError(t, err, raw)
		require.True(t, got.Changed, raw)
		assert.Equal(t, []string{"x"}, tagNames(contentTags(t, got.Bytes)), raw)
		assert.Equal(t,
			topLevelWithout(t, []byte(raw), "tags"),
			topLevelWithout(t, got.Bytes, "tags"), raw)
		assert.Empty(t, got.BeforeTags, raw)
	}
}

func TestTagWriteFailsClosedOnUnparseableInput(t *testing.T) {
	cases := map[string]string{
		"truncated":       `{"tags":[{"name":"ops"}`,
		"not an object":   `[1,2,3]`,
		"tags not array":  `{"tags":{"name":"ops"}}`,
		"tag not object":  `{"tags":["ops"]}`,
		"duplicate tags":  `{"tags":[],"pageTags":[],"tags":[{"name":"x","timestamp":1}]}`,
		"pageTags scalar": `{"tags":[],"pageTags":3}`,
	}
	for name, raw := range cases {
		_, err := replaceContentTags([]byte(raw), TagOpAdd, []string{"x"}, 7)
		assert.Error(t, err, name)
	}
	_, err := bumpMetadataLastModified([]byte(`{"lastModified":"1"`), 7)
	assert.Error(t, err, "truncated metadata")
	_, err = bumpMetadataLastModified([]byte(`{"lastModified":"1","lastModified":"2"}`), 7)
	assert.Error(t, err, "duplicate lastModified")
}

func TestTagWritePreservesDuplicateNamedElementsByteForByte(t *testing.T) {
	// ddvk's repro: two "x" tag elements with distinct bytes. A name-keyed
	// splice collides them; the write must keep each occurrence separate.
	raw := []byte(`{"tags":[{"name":"x","timestamp":1,"colour":"red"},{"name":"x","timestamp":2,"colour":"blue"}],"pageTags":[]}`)

	added, err := replaceContentTags(raw, TagOpAdd, []string{"z"}, 42)
	require.NoError(t, err)
	require.True(t, added.Changed)
	assert.Equal(t, []string{"x", "z"}, added.AfterTags, "AfterTags is the de-duplicated name list")
	addedElems, err := jsonArrayMember(added.Bytes, "tags")
	require.NoError(t, err)
	require.Len(t, addedElems, 3)
	assert.JSONEq(t, `{"name":"x","timestamp":1,"colour":"red"}`, string(addedElems[0]))
	assert.JSONEq(t, `{"name":"x","timestamp":2,"colour":"blue"}`, string(addedElems[1]))

	removed, err := replaceContentTags(raw, TagOpRemove, []string{"x"}, 42)
	require.NoError(t, err)
	require.True(t, removed.Changed)
	removedElems, err := jsonArrayMember(removed.Bytes, "tags")
	require.NoError(t, err)
	assert.Empty(t, removedElems)

	replaced, err := replaceContentTags(raw, TagOpReplace, []string{"x"}, 42)
	require.NoError(t, err)
	assert.False(t, replaced.Changed, "replacing with the same names, both duplicates already present, is a no-op")
	replacedElems, err := jsonArrayMember(replaced.Bytes, "tags")
	require.NoError(t, err)
	require.Len(t, replacedElems, 2)
	assert.JSONEq(t, `{"name":"x","timestamp":1,"colour":"red"}`, string(replacedElems[0]))
	assert.JSONEq(t, `{"name":"x","timestamp":2,"colour":"blue"}`, string(replacedElems[1]))
}

func TestVerifyTagSpliceMatchesSurvivorsByOccurrenceNotJustName(t *testing.T) {
	original := []byte(`{"tags":[{"name":"x","timestamp":1,"colour":"red"},{"name":"x","timestamp":2,"colour":"blue"}],"pageTags":[]}`)
	// A splice that keeps two "x" elements but swaps their bytes must be
	// rejected: name-only comparison would accept this, byte-by-occurrence
	// comparison must not.
	swapped := []byte(`{"tags":[{"name":"x","timestamp":2,"colour":"blue"},{"name":"x","timestamp":1,"colour":"red"}],"pageTags":[]}`)
	assert.Error(t, verifyTagSplice(original, swapped, []string{"x"}))

	faithful, err := replaceContentTags(original, TagOpAdd, []string{"z"}, 42)
	require.NoError(t, err)
	assert.NoError(t, verifyTagSplice(original, faithful.Bytes, []string{"x", "z"}))
}

// --- Plan from the remote doc index, not the cache (RMAPI-SETTAG-001 round 3, item A) ---

func TestPlanTagUpdateBuildsFilesFromTheSuppliedBaseListNotTheCache(t *testing.T) {
	doc := loadPreservationDoc(t)
	base := []*Entry{
		{DocumentID: "doc-1.content", Hash: "c0c0", Size: int64(len(preservationContent))},
		{DocumentID: "doc-1.metadata", Hash: "0a0a", Size: int64(len(preservationMetadata))},
		{DocumentID: "doc-1/pg-1.rm", Hash: "5e57e5", Type: FileType, Size: 9999},
	}
	plan, err := planTagUpdate(doc, base, []byte(preservationContent), []byte(preservationMetadata), TagOpAdd, []string{"review"}, "", 42)
	require.NoError(t, err)
	require.True(t, plan.Result.Changed)

	var rm *Entry
	for _, f := range plan.files {
		if f.DocumentID == "doc-1/pg-1.rm" {
			rm = f
		}
	}
	require.NotNil(t, rm)
	assert.Equal(t, *base[2], *rm, "the plan's untouched entry must equal the supplied base list's entry, not doc.Files' cached one")
}

func TestPlanTagUpdateFailsClosedWhenTheBaseListLacksContentOrMetadata(t *testing.T) {
	doc := loadPreservationDoc(t)
	base := []*Entry{
		{DocumentID: "doc-1.metadata", Hash: "0a0a", Size: int64(len(preservationMetadata))},
		{DocumentID: "doc-1/pg-1.rm", Hash: "0b0b", Size: 4096},
	}
	_, err := planTagUpdate(doc, base, []byte(preservationContent), []byte(preservationMetadata), TagOpAdd, []string{"x"}, "", 42)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no content entry")

	base2 := []*Entry{
		{DocumentID: "doc-1.content", Hash: "c0c0", Size: int64(len(preservationContent))},
		{DocumentID: "doc-1/pg-1.rm", Hash: "0b0b", Size: 4096},
	}
	_, err = planTagUpdate(doc, base2, []byte(preservationContent), []byte(preservationMetadata), TagOpAdd, []string{"x"}, "", 42)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no metadata entry")
}

func TestTagWriteRefusesADocumentWithoutTheFilesItMustRewrite(t *testing.T) {
	doc := &BlobDoc{Entry: Entry{DocumentID: "doc-1", Hash: "ffff", Type: "DocumentType"}}
	doc.Files = []*Entry{{DocumentID: "doc-1.metadata", Hash: "0a0a"}}
	_, err := planOn(t, doc, TagOpAdd, []string{"x"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no content entry")
}

// --- JSON member splice -----------------------------------------------------

func TestSpliceJSONMemberReplacesExactlyOneSpan(t *testing.T) {
	cases := []struct{ name, raw, key, value, want string }{
		{"number", `{"a":1,"b":2}`, "a", `9`, `{"a":9,"b":2}`},
		{"literal", `{"a":true,"b":null}`, "b", `false`, `{"a":true,"b":false}`},
		{"string with colon and brace", `{"a":"x:{}","b":2}`, "b", `3`, `{"a":"x:{}","b":3}`},
		{"escaped quote in string", `{"a":"say \"hi\"","b":2}`, "b", `3`, `{"a":"say \"hi\"","b":3}`},
		{"nested object", `{"a":{"tags":[1]},"tags":[2]}`, "tags", `[3]`, `{"a":{"tags":[1]},"tags":[3]}`},
		{"nested array holding the key", `{"a":[{"tags":1}],"tags":2}`, "tags", `0`, `{"a":[{"tags":1}],"tags":0}`},
		{"whitespace around value", "{\n \"a\" :  1 ,\n \"b\": 2\n}", "a", `9`, "{\n \"a\" :  9 ,\n \"b\": 2\n}"},
		{"last member", `{"a":1,"b":2}`, "b", `[]`, `{"a":1,"b":[]}`},
		{"absent, empty object", `{}`, "t", `1`, `{"t":1}`},
		{"absent, empty object with space", `{ }`, "t", `1`, `{ "t":1}`},
		{"absent, appended", `{"a":1}`, "t", `1`, `{"a":1,"t":1}`},
		{"absent, appended after newline", "{\"a\":1\n}", "t", `1`, "{\"a\":1\n,\"t\":1}"},
	}
	for _, c := range cases {
		got, err := spliceJSONMember([]byte(c.raw), c.key, []byte(c.value))
		require.NoError(t, err, c.name)
		assert.Equal(t, c.want, string(got), c.name)
	}
}

func TestSpliceJSONMemberRefusesAmbiguousOrInvalidInput(t *testing.T) {
	_, err := spliceJSONMember([]byte(`{"t":1,"t":2}`), "t", []byte(`3`))
	assert.Error(t, err, "duplicate key")
	_, err = spliceJSONMember([]byte(`[1]`), "t", []byte(`3`))
	assert.Error(t, err, "array, not object")
	_, err = spliceJSONMember([]byte(`{"t":1`), "t", []byte(`3`))
	assert.Error(t, err, "truncated")
	_, err = spliceJSONMember([]byte(`{"t":1}`), "t", []byte(`{`))
	assert.Error(t, err, "invalid replacement value must not reach the wire")
}

// --- Readback ---------------------------------------------------------------

func TestVerifyTagSpliceAcceptsAFaithfulSplice(t *testing.T) {
	got, err := replaceContentTags([]byte(preservationContent), TagOpAdd, []string{"reviewed"}, 42)
	require.NoError(t, err)
	assert.NoError(t, verifyTagSplice([]byte(preservationContent), got.Bytes, []string{"ops", "reviewed"}))
}

func TestVerifyTagSpliceRejectsCollateralChange(t *testing.T) {
	original := []byte(`{"a":1,"pageTags":[{"name":"p"}],"tags":[]}`)
	cases := map[string]string{
		"tags differ from intent": `{"a":1,"pageTags":[{"name":"p"}],"tags":[{"name":"other"}]}`,
		"member lost":             `{"a":1,"tags":[{"name":"x"}]}`,
		"member changed":          `{"a":2,"pageTags":[{"name":"p"}],"tags":[{"name":"x"}]}`,
		"member invented":         `{"a":1,"b":0,"pageTags":[{"name":"p"}],"tags":[{"name":"x"}]}`,
		"page tags altered":       `{"a":1,"pageTags":[],"tags":[{"name":"x"}]}`,
		"not json":                `{"a":1,`,
	}
	for name, spliced := range cases {
		assert.Error(t, verifyTagSplice(original, []byte(spliced), []string{"x"}), name)
	}
}

// --- Stale-revision precondition, no-op detection, structured result -------

func TestPlanTagUpdateRejectsAStaleRevision(t *testing.T) {
	doc := loadPreservationDoc(t)
	_, err := planOn(t, doc, TagOpAdd, []string{"review"}, "revision-the-caller-saw")

	var stale *StaleRevisionError
	require.True(t, errors.As(err, &stale), "want StaleRevisionError, got %v", err)
	assert.Equal(t, "revision-the-caller-saw", stale.Expected)
	assert.Equal(t, "ffff", stale.Actual)
	assert.Equal(t, "ffff", doc.Hash, "a rejected precondition must leave the document unmodified")
	assert.Equal(t, "c0c0", doc.Files[0].Hash)
}

func TestPlanTagUpdateAcceptsAMatchingRevision(t *testing.T) {
	doc := loadPreservationDoc(t)
	plan, err := planOn(t, doc, TagOpAdd, []string{"review"}, "ffff")
	require.NoError(t, err)
	assert.True(t, plan.Result.Changed)
}

func TestPlanTagUpdateSkipsThePreconditionWhenUnset(t *testing.T) {
	doc := loadPreservationDoc(t)
	_, err := planOn(t, doc, TagOpAdd, []string{"review"}, "")
	require.NoError(t, err)
}

func TestPlanTagUpdateReportsANoOpWithoutTouchingTheDocument(t *testing.T) {
	doc := loadPreservationDoc(t)
	for _, tc := range []struct {
		op    TagOp
		names []string
	}{
		{TagOpAdd, []string{"ops"}},
		{TagOpReplace, []string{"ops"}},
		{TagOpRemove, []string{"absent"}},
	} {
		plan, err := planOn(t, doc, tc.op, tc.names, "")
		require.NoError(t, err, tc.op)
		assert.False(t, plan.Result.Changed, "%v %v must report Changed=false", tc.op, tc.names)
		assert.Nil(t, plan.Content, "a no-op must produce nothing to upload")
		assert.Nil(t, plan.Metadata, "a no-op must produce nothing to upload")
		assert.Equal(t, plan.Result.BeforeRevision, plan.Result.AfterRevision)
		assert.Equal(t, "ffff", doc.Hash, "a no-op must not rehash the document")
		assert.Equal(t, "c0c0", doc.Files[0].Hash)
		assert.Equal(t, "0a0a", doc.Files[1].Hash)
		assert.Equal(t, []string{"ops"}, plan.Result.BeforeTags)
		assert.Equal(t, []string{"ops"}, plan.Result.AfterTags)
	}
}

func TestPlanTagUpdateReportsBeforeAndAfterState(t *testing.T) {
	doc := loadPreservationDoc(t)
	plan, err := planOn(t, doc, TagOpAdd, []string{"review"}, "")
	require.NoError(t, err)

	r := plan.Result
	assert.Equal(t, "doc-1", r.DocumentID)
	assert.Equal(t, "add", r.Operation)
	assert.Equal(t, []string{"ops"}, r.BeforeTags)
	assert.Equal(t, []string{"ops", "review"}, r.AfterTags)
	assert.Equal(t, "ffff", r.BeforeRevision)
	assert.NotEmpty(t, r.AfterRevision)
	assert.NotEqual(t, r.BeforeRevision, r.AfterRevision, "AfterRevision must be recomputed")
	assert.Equal(t, "ffff", doc.Hash, "planTagUpdate must not mutate the document")
	assert.Equal(t, plan.docHash, r.AfterRevision)
	assert.Equal(t, 1, r.PageTagCount)

	// The hashes in the report are the hashes of the bytes that will be sent,
	// and the plan's copied files point at them with the right sizes. The
	// document's own entries, and the .rm entry, are untouched.
	assert.Equal(t, plan.files[0].Hash, r.ContentHash)
	assert.Equal(t, plan.files[1].Hash, r.MetadataHash)
	assert.Equal(t, int64(len(plan.Content)), plan.files[0].Size)
	assert.Equal(t, int64(len(plan.Metadata)), plan.files[1].Size)
	assert.Equal(t, "0b0b", plan.files[2].Hash)
	assert.Equal(t, int64(4096), plan.files[2].Size)
	assert.Equal(t, "c0c0", doc.Files[0].Hash, "document's own files entry must be untouched")
	assert.Equal(t, "0a0a", doc.Files[1].Hash, "document's own files entry must be untouched")
}

func TestPlanTagUpdateDoesNotDependOnTheParsedStructs(t *testing.T) {
	// The parsed archive.Content on the doc is stale on purpose: the write
	// must work from the raw bytes it was given, never from the struct.
	doc := loadPreservationDoc(t)
	doc.Content.DocumentTags = []archive.Tag{{Name: "struct-only", Timestamp: 1}}
	plan, err := planOn(t, doc, TagOpAdd, []string{"review"}, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"ops", "review"}, plan.Result.AfterTags)
	assert.NotContains(t, string(plan.Content), "struct-only")
}

func TestPlanTagUpdateDoesNotMutateTheDocument(t *testing.T) {
	doc := loadPreservationDoc(t)
	before, err := json.Marshal(doc)
	require.NoError(t, err)

	plan, err := planOn(t, doc, TagOpAdd, []string{"review"}, "")
	require.NoError(t, err)
	require.True(t, plan.Result.Changed)

	after, err := json.Marshal(doc)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "planTagUpdate must not mutate the document")

	plan.apply(doc)
	assert.Equal(t, plan.docHash, doc.Hash)
	assert.Equal(t, plan.docSize, doc.Size)
	assert.Equal(t, plan.Result.ContentHash, doc.Files[0].Hash)
	assert.Equal(t, plan.Result.MetadataHash, doc.Files[1].Hash)
	assert.Equal(t, []string{"ops", "review"}, tagNames(doc.Content.DocumentTags))
	assert.Equal(t, plan.lastModified, doc.Metadata.LastModified)
}

// --- End to end against a fake sync15 cloud --------------------------------
//
// fakeCloud is the smallest server the write path talks to: content-addressed
// blobs, a root pointer with a generation, and the 412 the real service sends
// when the generation a writer names is not the current one. It lets the whole
// of ApiCtx.UpdateDocumentTags run — Sync's retry loop, Mirror, the closure,
// upload, readback — with no network and no real account.

type fakeCloud struct {
	mu              sync.Mutex
	blobs           map[string][]byte
	rootHash        string
	gen             int64
	putBlobs        []string // hashes PUT, in order
	rootPuts        int
	blobPutAttempts int // every blob PUT request received, successful or not

	dropPuts        bool   // accept blob PUTs but store nothing (simulates a lost write)
	failBlobPutN    int    // fail the Nth blob PUT (1-based) with a 500; 0 = never
	alwaysConflict  bool   // root PUTs always 412, regardless of generation
	failRootPutOnce bool   // the next root PUT returns 500 (a genuine, non-412 failure), then clears itself
	afterRootPut    func() // run synchronously after a successful root PUT, lock released
}

func (c *fakeCloud) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/sync/v4/root", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		json.NewEncoder(w).Encode(model.BlobRootStorageResponse{Hash: c.rootHash, Generation: c.gen, Schema: 4})
	})
	mux.HandleFunc("/sync/v3/root", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.rootPuts++
		if c.failRootPutOnce {
			c.failRootPutOnce = false
			c.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if c.alwaysConflict {
			c.mu.Unlock()
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		var req model.BlobRootStorageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			c.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Generation != c.gen {
			c.mu.Unlock()
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		c.rootHash = req.Hash
		c.gen++
		resp := model.BlobRootStorageResponse{Hash: c.rootHash, Generation: c.gen}
		hook := c.afterRootPut
		c.mu.Unlock()
		if hook != nil {
			hook()
		}
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/sync/v3/files/", func(w http.ResponseWriter, r *http.Request) {
		hash := strings.TrimPrefix(r.URL.Path, "/sync/v3/files/")
		c.mu.Lock()
		defer c.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			b, ok := c.blobs[hash]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(b)
		case http.MethodPut:
			c.blobPutAttempts++
			if c.failBlobPutN > 0 && c.blobPutAttempts == c.failBlobPutN {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			b, _ := io.ReadAll(r.Body)
			c.putBlobs = append(c.putBlobs, hash)
			if !c.dropPuts {
				c.blobs[hash] = b
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	return mux
}

// seedDoc stores a complete document (content, metadata, one ink file, and its
// docSchema index) and returns the BlobDoc as the tree would hold it.
func (c *fakeCloud) seedDoc(t *testing.T, id, content, metadata string) *BlobDoc {
	t.Helper()
	ink := []byte("ink-bytes-" + id)
	doc := &BlobDoc{Entry: Entry{DocumentID: id}} // Type is the index column, filled by Mirror
	for _, f := range []struct {
		name string
		body []byte
	}{
		{id + ".content", []byte(content)},
		{id + ".metadata", []byte(metadata)},
		{id + "/pg-1.rm", ink},
	} {
		sum := sha256.Sum256(f.body)
		h := hex.EncodeToString(sum[:])
		c.blobs[h] = f.body
		doc.Files = append(doc.Files, &Entry{DocumentID: f.name, Hash: h, Type: FileType, Size: int64(len(f.body))})
	}
	require.NoError(t, doc.Rehash())
	idx, err := doc.IndexReader()
	require.NoError(t, err)
	c.blobs[doc.Hash], _ = io.ReadAll(idx)
	return doc
}

// setRoot makes docs the cloud's current tree and advances the generation,
// exactly as another writer's successful sync would.
func (c *fakeCloud) setRoot(t *testing.T, docs ...*BlobDoc) *HashTree {
	t.Helper()
	tree := &HashTree{Docs: docs, SchemaVersion: SchemaVersionV4}
	require.NoError(t, tree.Rehash())
	idx, err := tree.IndexReader()
	require.NoError(t, err)
	c.mu.Lock()
	c.blobs[tree.Hash], _ = io.ReadAll(idx)
	c.rootHash = tree.Hash
	c.gen++
	c.mu.Unlock()
	return tree
}

// newFakeCloudCtx starts the fake, points rmapi's URLs at it, mirrors the
// tree the way CreateCtx does, and returns an ApiCtx bound to it.
func newFakeCloudCtx(t *testing.T) (*fakeCloud, *ApiCtx) {
	t.Helper()
	cloud := &fakeCloud{blobs: map[string][]byte{}}
	srv := httptest.NewServer(cloud.handler())
	t.Cleanup(srv.Close)

	blobURL, rootGet, rootPut := config.BlobUrl, config.RootGet, config.RootPut
	config.BlobUrl = srv.URL + "/sync/v3/files/"
	config.RootGet = srv.URL + "/sync/v4/root"
	config.RootPut = srv.URL + "/sync/v3/root"
	t.Cleanup(func() { config.BlobUrl, config.RootGet, config.RootPut = blobURL, rootGet, rootPut })
	t.Setenv("HOME", t.TempDir()) // saveTree writes under the user cache dir

	httpCtx := transport.CreateHttpClientCtx(model.AuthTokens{})
	storage := NewBlobStorage(&httpCtx)
	return cloud, &ApiCtx{Http: &httpCtx, blobStorage: storage, hashTree: &HashTree{}}
}

func (ctx *ApiCtx) mirrorFrom(t *testing.T) {
	t.Helper()
	require.NoError(t, ctx.hashTree.Mirror(ctx.blobStorage, 1))
	ctx.ft = DocumentsFileTree(ctx.hashTree)
}

func TestUpdateDocumentTagsWritesOnlyTheTwoBlobsAndTheIndexes(t *testing.T) {
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)
	before := doc.Hash

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, before)
	require.NoError(t, err)
	require.True(t, res.Changed)
	assert.Equal(t, []string{"ops", "reviewed"}, res.AfterTags)

	// What the cloud now holds under the new content hash is the original
	// document with one member changed — never a re-serialisation.
	stored := cloud.blobs[res.ContentHash]
	require.NotNil(t, stored, "content blob must be uploaded under the hash the report names")
	assert.Equal(t,
		topLevelWithout(t, []byte(preservationContent), "tags"),
		topLevelWithout(t, stored, "tags"))
	assert.Equal(t, []string{"ops", "reviewed"}, tagNames(contentTags(t, stored)))
	assert.Equal(t,
		topLevelWithout(t, []byte(preservationMetadata), "lastModified"),
		topLevelWithout(t, cloud.blobs[res.MetadataHash], "lastModified"))

	// Exactly four uploads: content, metadata, the doc index, the root index.
	// The ink file is never touched.
	assert.Len(t, cloud.putBlobs, 4)
	assert.Equal(t, res.AfterRevision, cloud.putBlobs[2], "doc index uploaded under the new doc hash")
	assert.Equal(t, cloud.rootHash, cloud.putBlobs[3], "root index uploaded under the new root hash")
	d, err := ctx.hashTree.FindDoc("doc-1")
	require.NoError(t, err)
	assert.Equal(t, res.AfterRevision, d.Hash)
	assert.Equal(t, int64(2), cloud.gen, "one successful root write")
}

func TestUpdateDocumentTagsNoOpWritesNothing(t *testing.T) {
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)
	rootBefore, genBefore := cloud.rootHash, cloud.gen

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"ops"}, "")
	require.NoError(t, err)
	assert.False(t, res.Changed)
	assert.Empty(t, cloud.putBlobs, "a no-op must upload nothing")
	assert.Equal(t, 0, cloud.rootPuts, "a no-op must not rewrite the root index")
	assert.Equal(t, rootBefore, cloud.rootHash)
	assert.Equal(t, genBefore, cloud.gen)
}

func TestUpdateDocumentTagsNoOpErrorsWhenLocalCacheDisagreesWithServerRoot(t *testing.T) {
	// Round 3, item C: a no-op is a claim about the server's state, not just
	// the local cache's. If the cache is stale, "nothing to do" would be a
	// lie — the call must fail instead of returning a silent nil.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	// Another writer adds a second document and syncs, without this ctx
	// refreshing — the local root hash is now stale, even though doc-1's own
	// tags are unaffected and this call would otherwise be a genuine no-op.
	other := cloud.seedDoc(t, "doc-2", `{}`, `{"type":"CollectionType","visibleName":"F","parent":"","lastModified":"1"}`)
	cloud.setRoot(t, doc, other)

	_, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"ops"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match the server root")
}

func TestUpdateDocumentTagsRefusesAFolder(t *testing.T) {
	cloud, ctx := newFakeCloudCtx(t)
	folder := cloud.seedDoc(t, "dir-1", `{}`, `{"type":"CollectionType","visibleName":"F","parent":"","lastModified":"1"}`)
	cloud.setRoot(t, folder)
	ctx.mirrorFrom(t)

	_, err := ctx.UpdateDocumentTags("dir-1", TagOpAdd, []string{"x"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a document")
	assert.Empty(t, cloud.putBlobs)
}

// --- Pre-commit root containment guard (round 3, item B) --------------------

func TestUpdateDocumentTagsRefusesToWriteWhenTheLocalCacheIsMissingDocuments(t *testing.T) {
	cloud, ctx := newFakeCloudCtx(t)
	docA := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	docB := cloud.seedDoc(t, "doc-2", preservationContent, preservationMetadata)
	cloud.setRoot(t, docA, docB)
	ctx.mirrorFrom(t)

	// Simulate a truncated local cache: doc-2 is gone from the in-memory
	// tree, but the tree's hash still points at the two-document root the
	// server holds — a corrupted-cache scenario, not a live-drift one (the
	// server's root has not actually changed).
	require.Len(t, ctx.hashTree.Docs, 2)
	for i, d := range ctx.hashTree.Docs {
		if d.DocumentID == "doc-2" {
			ctx.hashTree.Docs = append(ctx.hashTree.Docs[:i], ctx.hashTree.Docs[i+1:]...)
			break
		}
	}
	require.Len(t, ctx.hashTree.Docs, 1)

	_, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing")
	assert.Contains(t, err.Error(), "delete tree.cache")
	assert.Equal(t, 0, cloud.rootPuts)
	assert.Empty(t, cloud.putBlobs)
}

func TestUpdateDocumentTagsSeesAConcurrentChangeInsideTheRetry(t *testing.T) {
	// The precondition is evaluated inside Sync's closure on purpose: when the
	// first root write is refused with 412, Sync mirrors the remote tree and
	// runs the closure again, and only then is the concurrent change visible.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)
	seen := doc.Hash

	// Another writer replaces the tag set and syncs first.
	otherContent := strings.Replace(preservationContent, `"tags": [{"name": "ops", "timestamp": 1}]`, `"tags": [{"name": "theirs", "timestamp": 9}]`, 1)
	require.NotEqual(t, preservationContent, otherContent)
	theirs := cloud.seedDoc(t, "doc-1", otherContent, preservationMetadata)
	cloud.setRoot(t, theirs)

	_, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"mine"}, seen)
	var stale *StaleRevisionError
	require.True(t, errors.As(err, &stale), "want StaleRevisionError after the retry, got %v", err)
	assert.Equal(t, seen, stale.Expected)
	assert.Equal(t, theirs.Hash, stale.Actual)
	assert.Equal(t, 1, cloud.rootPuts, "the first root write was refused; no second attempt after the precondition failed")
	// The other writer's tree is still the cloud's root.
	root, _, err := ctx.blobStorage.GetRootIndex()
	require.NoError(t, err)
	assert.Equal(t, cloud.rootHash, root)
	d, err := ctx.hashTree.FindDoc("doc-1")
	require.NoError(t, err)
	assert.Equal(t, theirs.Hash, d.Hash, "local tree now mirrors the other writer's document")
}

func TestUpdateDocumentTagsWithoutPreconditionReappliesOnTheFreshTree(t *testing.T) {
	// A blind write (no expected revision) retries against the mirrored
	// document, so the other writer's tag survives and ours is added to it.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	otherContent := strings.Replace(preservationContent, `"tags": [{"name": "ops", "timestamp": 1}]`, `"tags": [{"name": "theirs", "timestamp": 9}]`, 1)
	theirs := cloud.seedDoc(t, "doc-1", otherContent, preservationMetadata)
	cloud.setRoot(t, theirs)

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"mine"}, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"theirs"}, res.BeforeTags)
	assert.Equal(t, []string{"theirs", "mine"}, res.AfterTags)
	assert.Equal(t, 2, cloud.rootPuts, "refused once, then written")
	assert.Equal(t, []string{"theirs", "mine"}, tagNames(contentTags(t, cloud.blobs[res.ContentHash])))
}

func TestUpdateDocumentTagsFailsWhenABlobPutIsAckedButDropped(t *testing.T) {
	// Round 3 follow-up: the root-hash fast path trusts the root pointer
	// (content-addressing proves which bytes it names), but it does not
	// prove those bytes are actually retrievable. A server that
	// acknowledges a blob PUT and silently discards it (what dropPuts
	// simulates) leaves the root pointing at a doc index whose .content and
	// .metadata 404 — a broken notebook reported as success. The doc-level
	// write-set readback (verifyWriteSetLanded) exists to catch exactly
	// this, with three small GETs of only the bytes this write sent.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)
	cloud.dropPuts = true

	_, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "readback")

	// Not one of the sentinel-carrying types: the shell layer's kind
	// classification falls through to the generic "error" kind for this.
	var stale *StaleRevisionError
	var superseded *SupersededError
	var notCommitted *NotCommittedError
	assert.False(t, errors.As(err, &stale))
	assert.False(t, errors.As(err, &superseded))
	assert.False(t, errors.As(err, &notCommitted))
}

func TestUpdateDocumentTagsReadbackLandsWhenAnotherDocumentMovedTheRoot(t *testing.T) {
	// The slow path: the live root hash no longer matches what we uploaded
	// (something else changed immediately after), but our own document's
	// entry within that new root still matches what we wrote — our write
	// landed, and the readback must say so without erroring.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	cloud.afterRootPut = func() {
		other := cloud.seedDoc(t, "doc-2", `{}`, `{"type":"CollectionType","visibleName":"F","parent":"","lastModified":"1"}`)
		d1, err := ctx.hashTree.FindDoc("doc-1")
		require.NoError(t, err)
		cloud.setRoot(t, d1, other)
	}

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.NoError(t, err)
	assert.True(t, res.Changed)
}

func TestUpdateDocumentTagsReadbackReportsSupersededWhenALaterWriterAdvances(t *testing.T) {
	// The slow path's failure branch: the live root no longer matches, and
	// our own document's entry no longer matches what we wrote either — a
	// later writer really did move the document again, at a higher
	// generation than ours committed at.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	cloud.afterRootPut = func() {
		otherContent := strings.Replace(preservationContent, `"tags": [{"name": "ops", "timestamp": 1}]`, `"tags": [{"name": "theirs", "timestamp": 9}]`, 1)
		other := cloud.seedDoc(t, "doc-1", otherContent, preservationMetadata)
		cloud.setRoot(t, other)
	}

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "superseded")
	// The type name and kind string stay "superseded", but the server gives
	// no way to distinguish "this write landed, then got replaced" from
	// "this write never landed" — the error text must say so plainly rather
	// than assert the write landed.
	assert.Contains(t, err.Error(), "ambiguous")
	require.NotNil(t, res, "a superseded write must still return the populated result")
	assert.NotEmpty(t, res.AfterRevision)

	var superseded *SupersededError
	require.True(t, errors.As(err, &superseded), "want *SupersededError, got %T", err)
	assert.Equal(t, "doc-1", superseded.DocumentID)
}

func TestUpdateDocumentTagsLeavesTheTreeUntouchedWhenAnUploadFails(t *testing.T) {
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)
	rootBefore := cloud.rootHash

	before, err := json.Marshal(ctx.hashTree)
	require.NoError(t, err)

	cloud.failBlobPutN = 1 // the first blob PUT (content) fails
	_, err = ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)

	after, err := json.Marshal(ctx.hashTree)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "tree must be untouched after a failed upload")
	assert.Equal(t, rootBefore, cloud.rootHash, "cloud root must be untouched after a failed upload")

	cloud.failBlobPutN = 0
	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.NoError(t, err)
	assert.True(t, res.Changed)
}

func TestUpdateDocumentTagsLeavesTheTreeUntouchedWhenTheDocIndexUploadFails(t *testing.T) {
	// Content and metadata land; the doc-index PUT (the third upload) fails.
	// The tree must stay untouched, and the root must never be written.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	cloud.failBlobPutN = 3
	_, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)

	assert.Len(t, cloud.putBlobs, 2, "content and metadata must have landed before the doc-index PUT failed")
	assert.Equal(t, 0, cloud.rootPuts, "the root must never be written when the doc index failed to upload")
	d, err := ctx.hashTree.FindDoc("doc-1")
	require.NoError(t, err)
	assert.Equal(t, doc.Hash, d.Hash, "tree must be untouched")
}

func TestUpdateDocumentTagsRollsBackOnAPostClosureSyncFailure(t *testing.T) {
	// ddvk's repro (round 3, item C): the content/metadata/doc-index blobs
	// land, the closure applies the plan and returns nil, but the root
	// pointer write itself then fails with a genuine (non-412) error. The
	// write never reached the server; the local cache must not show it as if
	// it had.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	d0, err := ctx.hashTree.FindDoc("doc-1")
	require.NoError(t, err)
	hashBefore := d0.Hash
	filesBefore := make([]Entry, len(d0.Files))
	for i, f := range d0.Files {
		filesBefore[i] = *f
	}

	cloud.failRootPutOnce = true
	_, err = ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)

	d, err := ctx.hashTree.FindDoc("doc-1")
	require.NoError(t, err)
	assert.Equal(t, hashBefore, d.Hash, "a failed root write must roll back the document hash")
	filesAfter := make([]Entry, len(d.Files))
	for i, f := range d.Files {
		filesAfter[i] = *f
	}
	assert.Equal(t, filesBefore, filesAfter, "a failed root write must roll back the file entries")
	assert.Equal(t, []string{"ops"}, d.ToDocument().Tags, "a failed root write must roll back the parsed tags")

	// A second call with the same tags must not silently report
	// Changed=false, err=nil — the document is genuinely unrolled-back and
	// unchanged remotely, so it must either succeed with Changed=true and a
	// root PUT, or error. Our fake only fails the root PUT once, so this
	// attempt should succeed for real.
	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	if err == nil {
		require.NotNil(t, res)
		assert.True(t, res.Changed, "second attempt must not silently report a no-op after a rolled-back write")
		assert.True(t, cloud.rootPuts > 0)
	}
}

func TestUpdateDocumentTagsFailsWhenTheServerNeverAcceptsTheRoot(t *testing.T) {
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)
	rootBefore := cloud.rootHash

	cloud.alwaysConflict = true
	_, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not committed")
	assert.Equal(t, rootBefore, cloud.rootHash, "cloud root must be untouched")
}

func TestUpdateDocumentTagsReadbackChecksTheServerNotTheCache(t *testing.T) {
	// A concurrent writer lands its own root write in the instant between our
	// root PUT succeeding and the readback that follows. The readback must
	// see their root, not the hash our own Sync call believed it wrote.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)
	rootBefore, genBefore := cloud.rootHash, cloud.gen

	cloud.afterRootPut = func() {
		cloud.mu.Lock()
		cloud.rootHash = rootBefore
		cloud.gen = genBefore
		cloud.mu.Unlock()
	}

	_, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not committed")
}

func TestUpdateDocumentTagsRefreshesTheCachedDocument(t *testing.T) {
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.NoError(t, err)
	require.True(t, res.Changed)

	d, err := ctx.hashTree.FindDoc("doc-1")
	require.NoError(t, err)
	assert.Equal(t, res.AfterTags, d.ToDocument().Tags)

	var meta struct {
		LastModified string `json:"lastModified"`
	}
	require.NoError(t, json.Unmarshal(cloud.blobs[res.MetadataHash], &meta))
	assert.Equal(t, meta.LastModified, d.Metadata.LastModified)

	var wantSize int64
	for _, f := range d.Files {
		wantSize += f.Size
	}
	assert.Equal(t, wantSize, d.Size)

	// What saveTree persists still shows the new tags after a round trip.
	raw, err := json.Marshal(ctx.hashTree)
	require.NoError(t, err)
	var reloaded HashTree
	require.NoError(t, json.Unmarshal(raw, &reloaded))
	rd, err := reloaded.FindDoc("doc-1")
	require.NoError(t, err)
	assert.Equal(t, res.AfterTags, rd.ToDocument().Tags)
}

// --- Mirror must track remote Size, not just Hash (upstream BlobDoc.Mirror) -

func TestMirrorUpdatesSizeAlongsideHash(t *testing.T) {
	newBody := bytes.Repeat([]byte("x"), 200000)
	sum := sha256.Sum256(newBody)
	newRmHash := hex.EncodeToString(sum[:])

	newEntry := &Entry{DocumentID: "doc-1/pg-1.rm", Hash: newRmHash, Type: FileType, Size: 200000}
	idxReader, err := (&BlobDoc{Files: []*Entry{newEntry}}).IndexReader()
	require.NoError(t, err)
	idxBytes, err := io.ReadAll(idxReader)
	require.NoError(t, err)

	remote := fakeRemote{blobs: map[string]string{"new-doc-index": string(idxBytes)}}

	doc := &BlobDoc{Entry: Entry{DocumentID: "doc-1", Hash: "old-doc-index"}}
	doc.Files = []*Entry{{DocumentID: "doc-1/pg-1.rm", Hash: "old-rm-hash", Size: 4096}}

	require.NoError(t, doc.Mirror(&Entry{DocumentID: "doc-1", Hash: "new-doc-index"}, remote))

	require.Len(t, doc.Files, 1)
	assert.Equal(t, newRmHash, doc.Files[0].Hash)
	assert.Equal(t, int64(200000), doc.Files[0].Size, "Mirror must copy the remote entry's Size, not just its Hash")
}

// --- Structured output details (round 3, item H) ----------------------------

func TestTagUpdateResultMarshalsEmptyTagSlicesAsEmptyArraysNotNull(t *testing.T) {
	r := TagUpdateResult{DocumentID: "doc-1", Operation: "replace"}
	b, err := json.Marshal(r)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"beforeTags":[]`)
	assert.Contains(t, string(b), `"afterTags":[]`)
	assert.NotContains(t, string(b), `"beforeTags":null`)
	assert.NotContains(t, string(b), `"afterTags":null`)

	// A pointer must marshal the same way (json.MarshalIndent(result, ...)
	// in the shell layer marshals *TagUpdateResult, not the value).
	b2, err := json.Marshal(&r)
	require.NoError(t, err)
	assert.Contains(t, string(b2), `"beforeTags":[]`)
	assert.Contains(t, string(b2), `"afterTags":[]`)
}

func TestUpdateDocumentTagsPostWriteReadbackMatchesTheServerDocIndex(t *testing.T) {
	// The post-write remote doc index must still list the untouched .rm file
	// at its original hash and size — the invariant this whole patch exists
	// to prove, checked directly against what the server holds.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	var rmBefore *Entry
	for _, f := range doc.Files {
		if f.DocumentID == "doc-1/pg-1.rm" {
			rmBefore = f
		}
	}
	require.NotNil(t, rmBefore)

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.NoError(t, err)
	require.True(t, res.Changed)

	docIdxReader, err := ctx.blobStorage.GetReader(res.AfterRevision, addExt("doc-1", archive.DocSchemaExt))
	require.NoError(t, err)
	defer docIdxReader.Close()
	entries, _, err := parseIndex(docIdxReader)
	require.NoError(t, err)

	var rmAfter *Entry
	for _, e := range entries {
		if e.DocumentID == "doc-1/pg-1.rm" {
			rmAfter = e
		}
	}
	require.NotNil(t, rmAfter, "the published doc index must still list doc-1/pg-1.rm")
	assert.Equal(t, rmBefore.Hash, rmAfter.Hash)
	assert.Equal(t, rmBefore.Size, rmAfter.Size)
}
