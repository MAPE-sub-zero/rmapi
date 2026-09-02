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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
			// Round 5, item R: lastModified never moves backwards.
			// preservationMetadata's own lastModified ("1700000001000") is
			// already far ahead of this test's now (42), so the published
			// stamp must be one past the existing value, not the literal now.
			assert.Equal(t, `"1700000001001"`, string(topLevelWithout(t, plan.Metadata, "")["lastModified"]))
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

// --- verifyMetadataSplice: an independent readback for .metadata (round 4, item F)

func TestVerifyMetadataSpliceAcceptsAFaithfulSplice(t *testing.T) {
	spliced, err := bumpMetadataLastModified([]byte(preservationMetadata), 42)
	require.NoError(t, err)
	assert.NoError(t, verifyMetadataSplice([]byte(preservationMetadata), spliced, 42))
}

func TestVerifyMetadataSpliceRejectsCollateralChange(t *testing.T) {
	original := []byte(`{"a":1,"visibleName":"before","lastModified":"1"}`)
	cases := map[string]string{
		"lastModified not the intended value": `{"a":1,"visibleName":"before","lastModified":"999"}`,
		"member changed":                      `{"a":1,"visibleName":"after","lastModified":"42"}`,
		"member lost":                         `{"a":1,"lastModified":"42"}`,
		"member invented":                     `{"a":1,"b":0,"visibleName":"before","lastModified":"42"}`,
		"not json":                            `{"a":1,`,
	}
	for name, spliced := range cases {
		assert.Error(t, verifyMetadataSplice(original, []byte(spliced), 42), name)
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
	putNames        []string // rm-filename header PUT, parallel to putBlobs
	rootPuts        int
	blobPutAttempts int // every blob PUT request received, successful or not

	dropPuts        bool   // accept blob PUTs but store nothing (simulates a lost write)
	failBlobPutN    int    // fail the Nth blob PUT (1-based) with a 500; 0 = never
	alwaysConflict  bool   // root PUTs always 412, regardless of generation
	failRootPutOnce bool   // the next root PUT returns 500 (a genuine, non-412 failure), then clears itself
	rootPutAckLost  bool   // the next root PUT lands (state updates) but the response is a 500 anyway
	afterRootPut    func() // run synchronously after any root PUT that changed state (landed or ack-lost), lock released

	// staleReadsRemaining makes GetRootIndex report staleRootHash/staleGen
	// (a snapshot a caller supplies, typically the pre-write state) instead
	// of the live root, this many more times, then falls back to live
	// reads -- simulating a read replica that lags the write path.
	staleReadsRemaining int
	staleRootHash       string
	staleGen            int64

	// emptyCloudAfterRootPuts, once rootPuts reaches it, makes GetRootIndex
	// report the empty-cloud shape (Hash="", Generation=0) instead of live
	// state -- round 5, item J: simulates Mirror observing an empty cloud
	// partway through a retry storm, independent of the server's real state.
	emptyCloudAfterRootPuts int

	// totalRequests counts every HTTP request the fake receives, of any
	// method or path -- round 5, item O: proves a refused call never reaches
	// the network at all.
	totalRequests int
}

func (c *fakeCloud) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/sync/v4/root", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.staleReadsRemaining > 0 {
			c.staleReadsRemaining--
			json.NewEncoder(w).Encode(model.BlobRootStorageResponse{Hash: c.staleRootHash, Generation: c.staleGen, Schema: 4})
			return
		}
		if c.emptyCloudAfterRootPuts > 0 && c.rootPuts >= c.emptyCloudAfterRootPuts {
			json.NewEncoder(w).Encode(model.BlobRootStorageResponse{Hash: "", Generation: 0, Schema: 4})
			return
		}
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
		newHash, newGen := c.rootHash, c.gen
		ackLost := c.rootPutAckLost
		c.rootPutAckLost = false
		hook := c.afterRootPut
		c.mu.Unlock()
		// The hook runs for both outcomes below: state has already changed
		// server-side either way, so a concurrent writer racing the response
		// is equally possible whether or not this response itself lands.
		if hook != nil {
			hook()
		}
		if ackLost {
			// The write lands (state above is already updated) but the
			// response never usably reaches the caller.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(model.BlobRootStorageResponse{Hash: newHash, Generation: newGen})
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
			c.putNames = append(c.putNames, r.Header.Get(transport.RmFileNameHeader))
			if !c.dropPuts {
				c.blobs[hash] = b
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.totalRequests++
		c.mu.Unlock()
		mux.ServeHTTP(w, r)
	})
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

func TestUpdateDocumentTagsNoOpSucceedsAfterRefreshWhenLocalCacheWasStale(t *testing.T) {
	// Round 4, item A: the closure now refreshes a stale local root before
	// doing anything else, so a no-op call whose cache merely lagged the
	// server succeeds cleanly instead of refusing to report a no-op.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	// Another writer adds a second document and syncs, without this ctx
	// refreshing — the local root hash is now stale, even though doc-1's own
	// tags are unaffected and this call would otherwise be a genuine no-op.
	other := cloud.seedDoc(t, "doc-2", `{}`, `{"type":"CollectionType","visibleName":"F","parent":"","lastModified":"1"}`)
	cloud.setRoot(t, doc, other)
	rootPutsBefore := cloud.rootPuts

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"ops"}, "")
	require.NoError(t, err)
	assert.False(t, res.Changed)
	assert.Equal(t, rootPutsBefore, cloud.rootPuts, "a no-op must not rewrite the root index")
	assert.Equal(t, cloud.rootHash, ctx.hashTree.Hash, "the refresh must have caught the tree up to the server root")
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

func TestUpdateDocumentTagsRefusesToWriteWhenALocalCacheEntryHasTheWrongHash(t *testing.T) {
	// Round 4, item A: assertRootContainment is now exhaustive -- every
	// local doc must match its server entry by hash, not just the target
	// doc. Corrupt a sibling's cached hash without moving the server root at
	// all, so the closure's own live-root refresh has nothing to refresh
	// (and so cannot silently paper over the corruption by re-mirroring it).
	cloud, ctx := newFakeCloudCtx(t)
	docA := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	docB := cloud.seedDoc(t, "doc-2", preservationContent, preservationMetadata)
	cloud.setRoot(t, docA, docB)
	ctx.mirrorFrom(t)

	for _, d := range ctx.hashTree.Docs {
		if d.DocumentID == "doc-2" {
			d.Hash = "corrupted0000000000000000000000000000000000000000000000000000"
		}
	}
	rootBefore := cloud.rootHash

	_, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match the server")
	assert.Contains(t, err.Error(), "delete tree.cache")
	assert.Equal(t, 0, cloud.rootPuts)
	assert.Empty(t, cloud.putBlobs)
	assert.Equal(t, rootBefore, cloud.rootHash)
}

func TestUpdateDocumentTagsRefusesToWriteWhenTheLocalCacheHasAnExtraDocument(t *testing.T) {
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	extra := cloud.seedDoc(t, "doc-2", preservationContent, preservationMetadata) // never added to the server root
	ctx.hashTree.Docs = append(ctx.hashTree.Docs, extra)
	rootBefore := cloud.rootHash

	_, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete tree.cache")
	assert.Equal(t, 0, cloud.rootPuts)
	assert.Empty(t, cloud.putBlobs)
	assert.Equal(t, rootBefore, cloud.rootHash)
}

// --- Live-root refresh at closure start (round 4, item A) -------------------

func TestUpdateDocumentTagsRefreshesAStaleLocalRootBeforeWriting(t *testing.T) {
	// A sibling document changes and syncs while this ctx never refreshes.
	// The old code would build a plan against the stale tree, upload three
	// blobs, get refused with a 412, mirror, and retry. The closure now
	// refreshes up front, so the write lands on the first attempt.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	other := cloud.seedDoc(t, "doc-2", `{}`, `{"type":"CollectionType","visibleName":"F","parent":"","lastModified":"1"}`)
	cloud.setRoot(t, doc, other)
	ctx.mirrorFrom(t)

	otherV2 := cloud.seedDoc(t, "doc-2", `{}`, `{"type":"CollectionType","visibleName":"F2","parent":"","lastModified":"2"}`)
	cloud.setRoot(t, doc, otherV2)
	rootPutsBefore := cloud.rootPuts

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.NoError(t, err)
	require.True(t, res.Changed)
	assert.Equal(t, rootPutsBefore+1, cloud.rootPuts, "the stale local root must be refreshed up front, not discovered via a wasted PUT + 412")

	rootReader, err := ctx.blobStorage.GetReader(cloud.rootHash, addExt("root", archive.DocSchemaExt))
	require.NoError(t, err)
	defer rootReader.Close()
	entries, _, err := parseIndex(rootReader)
	require.NoError(t, err)
	var doc2Entry *Entry
	for _, e := range entries {
		if e.DocumentID == "doc-2" {
			doc2Entry = e
		}
	}
	require.NotNil(t, doc2Entry, "the published root must still list doc-2")
	assert.Equal(t, otherV2.Hash, doc2Entry.Hash, "the published root must list doc-2 at its new hash, not the stale cached one")
}

func TestUpdateDocumentTagsSeesAConcurrentChangeInsideTheRetry(t *testing.T) {
	// Round 4, item A: the live-root refresh at the top of the closure makes
	// a concurrent change to docId itself visible on the very first attempt,
	// before any upload is even attempted -- there is no second attempt to
	// speak of, and no root PUT is wasted discovering the staleness.
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
	require.True(t, errors.As(err, &stale), "want StaleRevisionError, got %v", err)
	assert.Equal(t, seen, stale.Expected)
	assert.Equal(t, theirs.Hash, stale.Actual)
	assert.Equal(t, 0, cloud.rootPuts, "the refresh catches the staleness before any root write is attempted")
	// The other writer's tree is still the cloud's root.
	root, _, err := ctx.blobStorage.GetRootIndex()
	require.NoError(t, err)
	assert.Equal(t, cloud.rootHash, root)
	d, err := ctx.hashTree.FindDoc("doc-1")
	require.NoError(t, err)
	assert.Equal(t, theirs.Hash, d.Hash, "local tree now mirrors the other writer's document")
}

func TestUpdateDocumentTagsWithoutPreconditionReappliesOnTheFreshTree(t *testing.T) {
	// A blind write (no expected revision) refreshes against the mirrored
	// document up front, so the other writer's tag survives and ours is
	// added to it, landing on the first attempt.
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
	assert.Equal(t, 1, cloud.rootPuts, "the refresh at the top of the closure picks up the other writer's change before the first attempt, so nothing is refused")
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
	// Round 4, item C: UpdateDocumentTags now establishes, as a local fact
	// (ctx.hashTree.Hash == attemptedRoot), that this write's own root PUT
	// committed before readback ever runs -- so a later generation with
	// docId's entry disagreeing can only mean a later writer replaced it.
	// The error text says so plainly, not "ambiguous".
	assert.NotContains(t, err.Error(), "ambiguous")
	require.NotNil(t, res, "a superseded write must still return the populated result")
	assert.NotEmpty(t, res.AfterRevision)

	var superseded *SupersededError
	require.True(t, errors.As(err, &superseded), "want *SupersededError, got %T", err)
	assert.Equal(t, "doc-1", superseded.DocumentID)
}

// --- Sync error handling classified from a local fact (round 4, item C/D) ---

func TestUpdateDocumentTagsSucceedsWhenTheRootPutLandsButItsAckIsLost(t *testing.T) {
	// Round 4, item D: a network failure between the root PUT landing
	// server-side and its response reaching this process must not be
	// treated as a lost write -- the readback after a Sync error checks
	// whether the write actually landed before rolling anything back.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	cloud.rootPutAckLost = true
	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.NoError(t, err)
	require.True(t, res.Changed)
	assert.Equal(t, []string{"ops", "reviewed"}, res.AfterTags)

	d, err := ctx.hashTree.FindDoc("doc-1")
	require.NoError(t, err)
	assert.Equal(t, res.AfterRevision, d.Hash, "the tree must reflect the write that actually landed, not a rollback")
}

func TestUpdateDocumentTagsReadbackRetriesThroughReplicaLagThenSucceeds(t *testing.T) {
	// Round 4, item C: a root/entry mismatch at a generation that has not
	// advanced past the one this write just committed is a contradiction
	// (this write's own commit is a local fact already established), not
	// evidence the write never landed -- most likely a read replica that
	// has not caught up yet. Retried a few times before giving up.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)
	rootBefore, genBefore := cloud.rootHash, cloud.gen

	origDelays := readbackRetryDelays
	readbackRetryDelays = []time.Duration{0, 0, 0}
	t.Cleanup(func() { readbackRetryDelays = origDelays })

	cloud.afterRootPut = func() {
		cloud.mu.Lock()
		cloud.staleRootHash, cloud.staleGen = rootBefore, genBefore
		cloud.staleReadsRemaining = 2
		cloud.mu.Unlock()
	}

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.NoError(t, err)
	assert.True(t, res.Changed)
}

func TestUpdateDocumentTagsReadbackAtOwnCommittedGenerationIsLagNotSupersession(t *testing.T) {
	// The bar for "a later writer" is the generation this write's commit
	// PRODUCED, not the base it was planned against. A replica that reports
	// exactly our committed generation but still serves the pre-write root
	// is lagging, not superseded -- comparing against the base generation
	// would have misreported it as SupersededError on the first readback.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)
	rootBefore, genBefore := cloud.rootHash, cloud.gen

	origDelays := readbackRetryDelays
	readbackRetryDelays = []time.Duration{0, 0, 0}
	t.Cleanup(func() { readbackRetryDelays = origDelays })

	cloud.afterRootPut = func() {
		cloud.mu.Lock()
		// Pre-write root hash, but the generation our PUT just produced.
		cloud.staleRootHash, cloud.staleGen = rootBefore, genBefore+1
		cloud.staleReadsRemaining = 2
		cloud.mu.Unlock()
	}

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.NoError(t, err, "a readback at our own committed generation must be retried as lag, not reported as superseded")
	assert.True(t, res.Changed)
	assert.Equal(t, genBefore+1, ctx.hashTree.Generation, "Sync must have recorded the generation the commit produced")
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
	// Round 4, item C: the 10-conflict fail-open is classified from a local
	// fact, not a fresh readback -- Sync's own last retry already mirrored
	// the tree to the server's true (unchanged) root before giving up, so
	// no rollback is needed or performed, and this is never Superseded (that
	// would require another writer to have advanced the generation, which
	// alwaysConflict never lets happen).
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)
	rootBefore := cloud.rootHash

	cloud.alwaysConflict = true
	_, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not committed")

	var notCommitted *NotCommittedError
	require.True(t, errors.As(err, &notCommitted), "want *NotCommittedError, got %T", err)
	var superseded *SupersededError
	assert.False(t, errors.As(err, &superseded))

	assert.Equal(t, rootBefore, cloud.rootHash, "cloud root must be untouched")
	assert.Equal(t, cloud.rootHash, ctx.hashTree.Hash, "the tree must reflect the server's true root, left there by Sync's own last mirror")
	d, err := ctx.hashTree.FindDoc("doc-1")
	require.NoError(t, err)
	assert.Equal(t, doc.Hash, d.Hash, "doc-1 in the tree must equal the server's own unchanged revision -- no rollback needed")
	assert.Equal(t, doc.Hash, notCommitted.ActualRevision)
}

func TestUpdateDocumentTagsReadbackChecksTheServerNotTheCache(t *testing.T) {
	// A concurrent writer lands its own root write in the instant between our
	// root PUT succeeding and the readback that follows, and the fake never
	// catches back up -- a permanently stale read path. Round 4, item C:
	// since this write's own commit already succeeded (a local fact), this
	// is read as replica lag, not "not committed" -- and it is retried
	// before giving up with a plain error.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)
	rootBefore, genBefore := cloud.rootHash, cloud.gen

	origDelays := readbackRetryDelays
	readbackRetryDelays = []time.Duration{0, 0, 0}
	t.Cleanup(func() { readbackRetryDelays = origDelays })

	cloud.afterRootPut = func() {
		cloud.mu.Lock()
		cloud.rootHash = rootBefore
		cloud.gen = genBefore
		cloud.mu.Unlock()
	}

	_, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "replica lag")
	assert.NotContains(t, err.Error(), "not committed")
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

// --- Plan resolves .content/.metadata from the remote index, not a stale
// --- cached hash (round 4, item B) ------------------------------------------

func TestUpdateDocumentTagsPlansFromTheServersCurrentContentNotAStaleCachedHash(t *testing.T) {
	oldContent := strings.Replace(preservationContent, `"tags": [{"name": "ops", "timestamp": 1}]`, `"tags": [{"name": "before", "timestamp": 1}]`, 1)
	newContent := strings.Replace(preservationContent, `"tags": [{"name": "ops", "timestamp": 1}]`, `"tags": [{"name": "after", "timestamp": 1}]`, 1)
	require.NotEqual(t, oldContent, newContent)

	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", newContent, preservationMetadata) // the server's real content is "after"
	sumOld := sha256.Sum256([]byte(oldContent))
	oldHash := hex.EncodeToString(sumOld[:])
	cloud.blobs[oldHash] = []byte(oldContent) // still resolvable, so a stale hash pointing here doesn't 404

	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	// Corrupt the LOCAL cache's content entry to the old (stale) hash, while
	// leaving the document's own docSchema hash (d.Hash) untouched -- the
	// exact staleness fetching via d.Files, instead of the remote index,
	// used to be vulnerable to.
	d, err := ctx.hashTree.FindDoc("doc-1")
	require.NoError(t, err)
	for _, f := range d.Files {
		if f.DocumentID == "doc-1.content" {
			f.Hash = oldHash
		}
	}

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"after", "reviewed"}, res.AfterTags, "the plan must be built from the server's current content, not a stale cached hash")
}

func TestUpdateDocumentTagsRefusesWhenTheRemoteDocIndexDoesNotHashToItsOwnAddress(t *testing.T) {
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	// Tamper with the bytes stored under doc.Hash so they still parse as a
	// doc index, but no longer hash back to the address they are stored
	// under -- a corrupted or substituted doc index.
	tampered := &BlobDoc{Files: []*Entry{
		{DocumentID: doc.Files[0].DocumentID, Hash: "deadbeef", Type: FileType, Size: 1},
	}}
	idx, err := tampered.IndexReader()
	require.NoError(t, err)
	body, err := io.ReadAll(idx)
	require.NoError(t, err)
	cloud.blobs[doc.Hash] = body

	rootBefore := cloud.rootHash
	_, err = ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remote index for doc-1 hashes to")
	assert.Equal(t, 0, cloud.rootPuts)
	assert.Empty(t, cloud.putBlobs)
	assert.Equal(t, rootBefore, cloud.rootHash)
}

// --- now is hoisted out of Sync's retry loop (round 4, item E) --------------

func TestUpdateDocumentTagsReusesTheSameBlobHashesAcrossRetries(t *testing.T) {
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	cloud.alwaysConflict = true
	_, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)

	distinct := make(map[string]bool)
	for i, name := range cloud.putNames {
		if name == "root.docSchema" {
			continue
		}
		distinct[cloud.putBlobs[i]] = true
	}
	assert.Equal(t, 3, len(distinct), "hoisting now out of the closure means every retry re-PUTs the same content-addressed bytes (content/metadata/docSchema) instead of minting a fresh set each attempt")
}

// --- RMAPI_FORCE_SCHEMA_VERSION is refused on the write path (round 4, item H)

func TestUpdateDocumentTagsRefusesToWriteWithForcedSchemaVersion(t *testing.T) {
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	t.Setenv("RMAPI_FORCE_SCHEMA_VERSION", "3")

	_, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RMAPI_FORCE_SCHEMA_VERSION")
	assert.Equal(t, 0, cloud.rootPuts)
	assert.Empty(t, cloud.putBlobs)
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

// --- Round 5, item N: verify bytes against the hashes that named them ------
//
// Content addressing proves what a store SHOULD hold under a hash, never that
// the bytes a GET actually returned are those bytes. Every blob this write
// reads and edits (.content/.metadata via fetchDocFile, the root index via
// assertRootContainment and the post-commit readback walk) is now checked
// against its own hash before being trusted.

func TestFetchDocFileRefusesBytesThatDoNotHashToTheirOwnAddress(t *testing.T) {
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	// A stand-in for a stale/mixed-up CDN object: the bytes stored under the
	// .content blob's own hash are a different, validly-formed document.
	const wrongContent = `{"fileType":"notebook","pageCount":1,"pages":["OTHER"],"pageTags":[],"tags":[]}`
	var contentHash string
	for _, f := range doc.Files {
		if f.DocumentID == "doc-1.content" {
			contentHash = f.Hash
		}
	}
	require.NotEmpty(t, contentHash)
	cloud.blobs[contentHash] = []byte(wrongContent)

	_, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to splice")
	assert.Empty(t, cloud.putBlobs, "no upload may happen once the source bytes fail their own hash check")
}

func TestAssertRootContainmentRefusesATamperedRootBody(t *testing.T) {
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	// The root blob stored under the tree's own hash no longer hashes to that
	// address -- a corrupted or substituted root index.
	cloud.blobs[cloud.rootHash] = append(append([]byte{}, cloud.blobs[cloud.rootHash]...), '\n')

	_, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)
	assert.Equal(t, 0, cloud.rootPuts)
	assert.Empty(t, cloud.putBlobs)
}

// --- Round 5, item L: containment adopts the server's Size ------------------

func TestAssertRootContainmentAdoptsTheServersSizeForAnUntouchedSibling(t *testing.T) {
	cloud, ctx := newFakeCloudCtx(t)
	target := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	sibling := cloud.seedDoc(t, "doc-2", preservationContent, preservationMetadata)
	cloud.setRoot(t, target, sibling)
	ctx.mirrorFrom(t)

	// Corrupt the cached Size for the untouched sibling without moving its
	// hash: the server's root entry, not the corrupted local cache, must win
	// when the root is republished.
	d2, err := ctx.hashTree.FindDoc("doc-2")
	require.NoError(t, err)
	serverSize := d2.Size
	d2.Size = serverSize + 12345

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.NoError(t, err)
	require.True(t, res.Changed)

	rootReader, err := ctx.blobStorage.GetReader(cloud.rootHash, addExt("root", archive.DocSchemaExt))
	require.NoError(t, err)
	defer rootReader.Close()
	entries, _, err := parseIndex(rootReader)
	require.NoError(t, err)
	var doc2Entry *Entry
	for _, e := range entries {
		if e.DocumentID == "doc-2" {
			doc2Entry = e
		}
	}
	require.NotNil(t, doc2Entry)
	assert.Equal(t, serverSize, doc2Entry.Size, "the republished root must list the server's original size for the untouched sibling, not the corrupted local cache")
}

// --- Round 5, item J: local commit fact needs the generation conjunct ------

func TestUpdateDocumentTagsIsNotCommittedWhenFailOpenMirrorsAnEmptyCloud(t *testing.T) {
	// Sync's own 10-conflict fail-open mirrors the tree one last time before
	// giving up. Mirror's empty-cloud branch (tree.go) never assigns t.Hash,
	// only Docs/Generation -- so ctx.hashTree.Hash == attemptedRoot can hold
	// by coincidence with nothing actually committed. The generation
	// conjunct catches it: a genuinely empty cloud reports Generation 0,
	// never past plan.generation.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)

	cloud.alwaysConflict = true
	cloud.emptyCloudAfterRootPuts = 10 // Sync's own last mirror, after the 10th conflict

	_, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)

	var notCommitted *NotCommittedError
	require.True(t, errors.As(err, &notCommitted), "want *NotCommittedError, got %T", err)
	var superseded *SupersededError
	assert.False(t, errors.As(err, &superseded))
	assert.Empty(t, notCommitted.ActualRevision, "the empty-cloud mirror left the local tree with no doc-1 entry at all")

	_, ferr := ctx.hashTree.FindDoc("doc-1")
	assert.Error(t, ferr, "no tags may be reported as written when the write never committed")
}

func TestUpdateDocumentTagsReadbackAtOwnCommittedGenerationIsStillLagNotSupersessionAfterTheGenerationConjunct(t *testing.T) {
	// Round 5 regression guard for item J: a genuine Sync success must still
	// satisfy the new Generation > plan.generation conjunct on the very next
	// call, so the existing lag-vs-supersession classification is untouched.
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)
	rootBefore, genBefore := cloud.rootHash, cloud.gen

	origDelays := readbackRetryDelays
	readbackRetryDelays = []time.Duration{0, 0, 0}
	t.Cleanup(func() { readbackRetryDelays = origDelays })

	cloud.afterRootPut = func() {
		cloud.mu.Lock()
		cloud.staleRootHash, cloud.staleGen = rootBefore, genBefore+1
		cloud.staleReadsRemaining = 2
		cloud.mu.Unlock()
	}

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.NoError(t, err)
	assert.True(t, res.Changed)
}

// --- Round 5, item K: post-error readback follows the doc entry ------------

func TestUpdateDocumentTagsAckLostPlusUnrelatedRootMoveSucceeds(t *testing.T) {
	// The inversion of the reviewer's TestAdvE2ELostAckPlusUnrelatedRootMoveIsReportedAsFailure:
	// the root PUT lands, its ack is lost, and a third writer adds an
	// unrelated document before the post-error readback runs. doc-1 on the
	// server still carries exactly the revision this write produced, so this
	// must be reported as success with no rollback.
	cloud, ctx := newFakeCloudCtx(t)
	doc1 := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	doc2 := cloud.seedDoc(t, "doc-2", `{}`, `{"type":"CollectionType","visibleName":"F","parent":"","lastModified":"1"}`)
	cloud.setRoot(t, doc1, doc2)
	ctx.mirrorFrom(t)

	cloud.rootPutAckLost = true
	doc3 := cloud.seedDoc(t, "doc-3", `{}`, `{"type":"CollectionType","visibleName":"G","parent":"","lastModified":"1"}`)
	cloud.afterRootPut = func() {
		d1, err := ctx.hashTree.FindDoc("doc-1")
		require.NoError(t, err)
		d2, err := ctx.hashTree.FindDoc("doc-2")
		require.NoError(t, err)
		cloud.setRoot(t, d1, d2, doc3)
	}

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.NoError(t, err, "the write is on the server; an unrelated root move before the readback must not be reported as failure")
	require.True(t, res.Changed)

	d, err := ctx.hashTree.FindDoc("doc-1")
	require.NoError(t, err)
	assert.Equal(t, res.AfterRevision, d.Hash, "local tree must keep the tags this write applied, not roll back")
}

func TestUpdateDocumentTagsAckLostPlusStaleReadsOfThePreWriteRootSucceeds(t *testing.T) {
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	cloud.setRoot(t, doc)
	ctx.mirrorFrom(t)
	rootBefore, genBefore := cloud.rootHash, cloud.gen

	origDelays := readbackRetryDelays
	readbackRetryDelays = []time.Duration{0, 0, 0}
	t.Cleanup(func() { readbackRetryDelays = origDelays })

	cloud.rootPutAckLost = true
	cloud.afterRootPut = func() {
		cloud.mu.Lock()
		cloud.staleRootHash, cloud.staleGen = rootBefore, genBefore
		cloud.staleReadsRemaining = 2
		cloud.mu.Unlock()
	}

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.NoError(t, err)
	assert.True(t, res.Changed)
	d, err := ctx.hashTree.FindDoc("doc-1")
	require.NoError(t, err)
	assert.Equal(t, res.AfterRevision, d.Hash)
}

// --- Round 5, item P: a doc deleted after our commit is supersession -------

func TestUpdateDocumentTagsReportsSupersededWhenDocDeletedAfterCommit(t *testing.T) {
	cloud, ctx := newFakeCloudCtx(t)
	doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
	other := cloud.seedDoc(t, "doc-2", `{}`, `{"type":"CollectionType","visibleName":"F","parent":"","lastModified":"1"}`)
	cloud.setRoot(t, doc, other)
	ctx.mirrorFrom(t)

	cloud.afterRootPut = func() {
		d2, err := ctx.hashTree.FindDoc("doc-2")
		require.NoError(t, err)
		cloud.setRoot(t, d2) // doc-1 deleted by a concurrent writer
	}

	res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
	require.Error(t, err)
	require.NotNil(t, res, "a superseded write must still return the populated result")
	assert.NotEmpty(t, res.AfterRevision)

	var superseded *SupersededError
	require.True(t, errors.As(err, &superseded), "want *SupersededError, got %T", err)
	assert.Equal(t, "", superseded.ActualRevision)
	assert.Contains(t, err.Error(), "deleted")
}

// --- Round 5, item O: the API refuses empty tag lists for every op ---------

func TestUpdateDocumentTagsRefusesEmptyTagListsAtTheApiLayer(t *testing.T) {
	cases := []struct {
		name    string
		op      TagOp
		tags    []string
		wantErr string
	}{
		{"replace with nil", TagOpReplace, nil, "refusing to clear every tag"},
		{"replace with empty slice", TagOpReplace, []string{}, "refusing to clear every tag"},
		{"add with no tags", TagOpAdd, nil, "no tags given"},
		{"remove with no tags", TagOpRemove, nil, "no tags given"},
		{"a blank tag name", TagOpAdd, []string{"   "}, "empty tag name"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cloud, ctx := newFakeCloudCtx(t)
			doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)
			cloud.setRoot(t, doc)
			ctx.mirrorFrom(t)
			before := cloud.totalRequests

			_, err := ctx.UpdateDocumentTags("doc-1", tc.op, tc.tags, "")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Equal(t, before, cloud.totalRequests, "an invalid tag operation must not reach the network")
		})
	}
}

// --- Round 5, item Q: the doc-index schema is threaded through -------------

func TestUpdateDocumentTagsPreservesTheDocIndexSchemaVersion(t *testing.T) {
	for _, schema := range []string{SchemaVersionV3, SchemaVersionV4} {
		t.Run("schema "+schema, func(t *testing.T) {
			cloud, ctx := newFakeCloudCtx(t)
			doc := cloud.seedDoc(t, "doc-1", preservationContent, preservationMetadata)

			// Rewrite doc-1's index blob under its own (unchanged) hash in
			// the given schema, as a device-written index actually is --
			// seedDoc always writes v3.
			body := schema + "\n"
			if schema == SchemaVersionV4 {
				body += "0:.:" + strconv.Itoa(len(doc.Files)) + ":" + strconv.FormatInt(doc.Size, 10) + "\n"
			}
			for _, f := range doc.Files {
				body += f.Line() + "\n"
			}
			cloud.blobs[doc.Hash] = []byte(body)
			cloud.setRoot(t, doc)
			ctx.mirrorFrom(t)

			res, err := ctx.UpdateDocumentTags("doc-1", TagOpAdd, []string{"reviewed"}, "")
			require.NoError(t, err)
			require.True(t, res.Changed)

			published := string(cloud.blobs[res.AfterRevision])
			require.True(t, strings.HasPrefix(published, schema+"\n"), "published doc index must open with schema %q, got %q", schema, published)
		})
	}
}

func TestTagUpdatePlanDocHashDoesNotDependOnSchema(t *testing.T) {
	// The doc hash a write publishes is fixed the moment planTagUpdate
	// returns -- schema is set on the plan afterward (item Q) and affects
	// only indexReader()'s output body, never plan.Result.AfterRevision:
	// HashEntries (common.go) hashes each entry's own Hash bytes only, never
	// the schema line.
	doc := loadPreservationDoc(t)
	plan, err := planOn(t, doc, TagOpAdd, []string{"reviewed"}, "")
	require.NoError(t, err)
	require.True(t, plan.Result.Changed)
	wantHash, err := HashEntries(plan.files)
	require.NoError(t, err)
	require.Equal(t, wantHash, plan.Result.AfterRevision)

	for _, schema := range []string{SchemaVersionV3, SchemaVersionV4} {
		plan.schema = schema
		r, err := plan.indexReader()
		require.NoError(t, err)
		body, err := io.ReadAll(r)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(string(body), schema+"\n"))
		assert.Equal(t, wantHash, plan.Result.AfterRevision, "changing plan.schema must never change the already-computed doc hash")
	}
}

// --- Round 5, item R: lastModified never goes backwards ---------------------

func TestPlanTagUpdateLastModifiedStampNeverMovesBackwards(t *testing.T) {
	newMetadata := func(existing string) string {
		if existing == "" {
			return `{"type":"DocumentType","visibleName":"Field notes","parent":""}`
		}
		return `{"type":"DocumentType","visibleName":"Field notes","parent":"","lastModified":"` + existing + `"}`
	}

	cases := []struct {
		name      string
		existing  string
		now       int64
		wantStamp int64
	}{
		{"existing ahead of now", "9999999999999", 42, 9999999999999 + 1},
		{"existing behind now", "1", 9999999999999, 9999999999999},
		{"existing absent", "", 42, 42},
	}

	doc := loadPreservationDoc(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metadata := newMetadata(tc.existing)
			plan, err := planTagUpdate(doc, doc.Files, []byte(preservationContent), []byte(metadata), TagOpAdd, []string{"reviewed"}, "", tc.now)
			require.NoError(t, err)
			require.True(t, plan.Result.Changed)

			want := strconv.FormatInt(tc.wantStamp, 10)
			assert.Equal(t, want, plan.lastModified)

			var got struct {
				LastModified string `json:"lastModified"`
			}
			require.NoError(t, json.Unmarshal(plan.Metadata, &got))
			assert.Equal(t, want, got.LastModified)

			assert.NoError(t, verifyMetadataSplice([]byte(metadata), plan.Metadata, tc.wantStamp))
		})
	}
}
