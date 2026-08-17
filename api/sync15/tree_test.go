package sync15

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

func TestParseLine(t *testing.T) {
	line := "hash:0:docid:0:993"
	entry, err := parseEntry(line)
	if err != nil {
		t.Error(err)
	}

	if entry.Hash != "hash" {
		t.Error("wrong hash")
	}
	if entry.DocumentID != "docid" {
		t.Error("wrong documentid")
	}

	if entry.Size != 993 {
		t.Error("wrong size")
	}
}

func TestParseIndex(t *testing.T) {
	index := `3
	0f83178c4ebe6a60fae0360b74916ee9e1faa5de1c56ab3481eccdc5cb98754f:0:fe0039fb-56a0-4561-a36f-a820f0009622.content:0:993
	17eca6c9a540c993f5f5506bb09b7a40993c02fa8f065b1a6a442e412cf2fd04:0:fe0039fb-56a0-4561-a36f-a820f0009622.metadata:0:320`
	entries, schema, err := parseIndex(strings.NewReader(index))
	if err != nil {
		t.Error(err)
		return
	}
	if len(entries) != 2 {
		t.Error("wrong number of entries")
		return
	}
	if schema != SchemaVersionV3 {
		t.Errorf("expected schema v3, got %s", schema)
		return
	}
}
func TestParseIndexV4(t *testing.T) {
	index := `4
	0:.:2:1823419036
	0f83178c4ebe6a60fae0360b74916ee9e1faa5de1c56ab3481eccdc5cb98754f:0:fe0039fb-56a0-4561-a36f-a820f0009622.content:0:993
	17eca6c9a540c993f5f5506bb09b7a40993c02fa8f065b1a6a442e412cf2fd04:0:fe0039fb-56a0-4561-a36f-a820f0009622.metadata:0:320`
	entries, schema, err := parseIndex(strings.NewReader(index))
	if err != nil {
		t.Error(err)
		return
	}
	if len(entries) != 2 {
		t.Error("wrong number of entries")
		return
	}
	if schema != SchemaVersionV4 {
		t.Errorf("expected schema v4, got %s", schema)
		return
	}
}

func TestCreateDocIndex(t *testing.T) {
	doc := &BlobDoc{
		Entry: Entry{
			Hash:       "somehash",
			DocumentID: "someid",
		},
	}
	file := &Entry{
		Hash:       "blah",
		DocumentID: "someid",
		Size:       10,
	}
	doc.AddFile(file)
	reader, err := doc.IndexReader()
	if err != nil {
		t.Error(err)
		return
	}
	index, err := io.ReadAll(reader)
	if err != nil {
		t.Error(err)
		return
	}
	expected := `3
blah:0:someid:0:10
`
	strIndex := string(index)

	if strIndex != expected {
		t.Errorf("index did not match %s", strIndex)
		return
	}
}

// TestRootIndexWritesV4WhenMirroredV3 is the regression test for the bug
// introduced in f5c7b9c: a tree whose SchemaVersion was read as v3 from the
// server must still produce a v4 root docSchema on write.
func TestRootIndexWritesV4WhenMirroredV3(t *testing.T) {
	tree := HashTree{
		SchemaVersion: SchemaVersionV3, // simulates a tree mirrored from a v3 root
	}
	doc := &BlobDoc{
		Entry: Entry{Hash: "somehash", DocumentID: "someid"},
	}
	doc.AddFile(&Entry{Hash: "filehash", DocumentID: "someid.pdf", Size: 100})
	tree.Add(doc)

	reader, err := tree.IndexReader()
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}

	_, schema, err := parseIndex(strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if schema != SchemaVersionV4 {
		t.Errorf("IndexReader() produced schema %s for v3-mirrored tree; want v4", schema)
	}

	if err := tree.Rehash(); err != nil {
		t.Fatal(err)
	}
	reader2, _ := tree.IndexReader()
	body2, _ := io.ReadAll(reader2)
	h := sha256.Sum256(body2)
	expectedHash := hex.EncodeToString(h[:])
	if tree.Hash != expectedHash {
		t.Errorf("Rehash() = %s; want sha256(IndexReader) = %s", tree.Hash, expectedHash)
	}
}

// docWithID builds a minimal valid BlobDoc for index tests.
func docWithID(id string) *BlobDoc {
	d := &BlobDoc{Entry: Entry{Hash: "h-" + id, DocumentID: id}}
	d.AddFile(&Entry{Hash: "f-" + id, DocumentID: id + ".metadata", Size: 1})
	return d
}

// indexDocIDs returns the document IDs in the order IndexReader emitted them.
func indexDocIDs(t *testing.T, tree *HashTree) []string {
	t.Helper()
	reader, err := tree.IndexReader()
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	entries, _, err := parseIndex(strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.DocumentID)
	}
	return ids
}

// TestRootIndexIsSortedByDocumentID is the regression test for ddvk/rmapi#75
// and #76: the cloud rejects a root docSchema whose entries are not sorted by
// document ID with 400 {"message":"invalid root schema"}. Add() appends, so a
// tree built in non-sorted order must still serialize sorted.
func TestRootIndexIsSortedByDocumentID(t *testing.T) {
	tree := HashTree{SchemaVersion: SchemaVersionV4}
	for _, id := range []string{"ccc", "aaa", "bbb"} {
		if err := tree.Add(docWithID(id)); err != nil {
			t.Fatal(err)
		}
	}

	got := indexDocIDs(t, &tree)
	want := []string{"aaa", "bbb", "ccc"}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("root index order = %v; want %v", got, want)
		}
	}
}

// TestRootIndexSortedAfterRemove covers the second way ordering breaks:
// Remove() swaps the final element into the removed slot, which unsorts the
// slice, and then only calls Rehash().
func TestRootIndexSortedAfterRemove(t *testing.T) {
	tree := HashTree{SchemaVersion: SchemaVersionV4}
	for _, id := range []string{"aaa", "bbb", "ccc", "ddd"} {
		if err := tree.Add(docWithID(id)); err != nil {
			t.Fatal(err)
		}
	}
	// Removing "bbb" moves "ddd" into its slot -> aaa, ddd, ccc.
	if err := tree.Remove("bbb"); err != nil {
		t.Fatal(err)
	}

	got := indexDocIDs(t, &tree)
	want := []string{"aaa", "ccc", "ddd"}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("root index order after Remove = %v; want %v", got, want)
		}
	}

	// The uploaded hash must still be sha256 of the emitted body.
	reader, _ := tree.IndexReader()
	body, _ := io.ReadAll(reader)
	h := sha256.Sum256(body)
	if tree.Hash != hex.EncodeToString(h[:]) {
		t.Errorf("Rehash() = %s; want sha256(IndexReader) = %s",
			tree.Hash, hex.EncodeToString(h[:]))
	}
}

func TestCreateRootIndex(t *testing.T) {
	tree := HashTree{
		SchemaVersion: SchemaVersionV4,
	}
	doc := &BlobDoc{
		Entry: Entry{
			Hash:       "somehash",
			DocumentID: "someid"},
	}
	file := &Entry{}
	doc.AddFile(file)
	tree.Add(doc)
	reader, err := tree.IndexReader()
	if err != nil {
		t.Error(err)
		return
	}
	index, err := io.ReadAll(reader)
	if err != nil {
		t.Error(err)
		return
	}
	expected := `4
0:.:1:0
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855:0:someid:1:0
`
	strIndex := string(index)

	if strIndex != expected {
		t.Errorf("index did not match %s", strIndex)
		return
	}

}
