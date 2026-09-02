package sync15

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juruen/rmapi/archive"
	"github.com/juruen/rmapi/log"
	"github.com/juruen/rmapi/model"
)

type BlobDoc struct {
	Files []*Entry
	Entry
	Metadata archive.MetadataFile
	Content  archive.Content
}

func NewBlobDoc(name, documentId, colType, parentId string) *BlobDoc {
	return &BlobDoc{
		Metadata: archive.MetadataFile{
			DocName:        name,
			CollectionType: colType,
			LastModified:   archive.UnixTimestamp(),
			Parent:         parentId,
		},
		Entry: Entry{
			DocumentID: documentId,
		},
	}

}

func (d *BlobDoc) Rehash() error {

	hash, err := HashEntries(d.Files)
	if err != nil {
		return err
	}
	log.Trace.Println("New doc hash: ", hash)
	d.Hash = hash
	return nil
}

// TagUpdateResult is the structured before/after report for a tag write.
type TagUpdateResult struct {
	DocumentID     string   `json:"documentId"`
	Operation      string   `json:"operation"`
	Changed        bool     `json:"changed"`
	BeforeRevision string   `json:"beforeRevision"`
	AfterRevision  string   `json:"afterRevision"`
	BeforeTags     []string `json:"beforeTags"`
	AfterTags      []string `json:"afterTags"`
	PageTagCount   int      `json:"pageTagCount"`
	ContentHash    string   `json:"contentHash,omitempty"`
	MetadataHash   string   `json:"metadataHash,omitempty"`
}

// MarshalJSON normalizes nil BeforeTags/AfterTags to an empty array: a JSON
// consumer should not have to distinguish "no tags" from "field omitted".
func (r TagUpdateResult) MarshalJSON() ([]byte, error) {
	type alias TagUpdateResult
	out := alias(r)
	if out.BeforeTags == nil {
		out.BeforeTags = []string{}
	}
	if out.AfterTags == nil {
		out.AfterTags = []string{}
	}
	return json.Marshal(out)
}

// tagUpdateSnapshot is doc's state immediately before apply overwrote it, so
// a write that applied locally but never landed on the server can be undone.
type tagUpdateSnapshot struct {
	Hash         string
	Size         int64
	Files        []*Entry
	DocumentTags []archive.Tag
	LastModified string
}

// tagUpdatePlan is a prepared tag write: the report, the exact bytes to
// upload, and the document state to apply once every upload has landed. A
// no-op plan has Result.Changed false and nothing else set.
type tagUpdatePlan struct {
	Result   *TagUpdateResult
	Content  []byte
	Metadata []byte

	files        []*Entry
	docHash      string
	docSize      int64
	tags         []archive.Tag
	lastModified string

	// generation is the tree generation Sync's closure saw when this plan
	// was built — the readback's basis for telling "superseded" (someone
	// advanced past it) from "not committed" (nobody has).
	generation int64
	snapshot   *tagUpdateSnapshot
}

// StaleRevisionError reports that the document moved under the caller.
type StaleRevisionError struct {
	DocumentID string
	Expected   string
	Actual     string
}

func (e *StaleRevisionError) Error() string {
	return fmt.Sprintf(
		"document %s is at revision %s, not the expected %s; refusing to overwrite a concurrent change",
		e.DocumentID, e.Actual, e.Expected)
}

// tagNames returns just the names of a tag set, in order.
func tagNames(tags []archive.Tag) []string {
	names := make([]string, 0, len(tags))
	for _, t := range tags {
		names = append(names, t.Name)
	}
	return names
}

// planTagUpdate enforces the stale-revision precondition (empty
// expectedRevision skips it, for a blind write) and computes a tagUpdatePlan
// for op against rawContent/rawMetadata. It never modifies doc; call
// plan.apply(doc) once every upload the plan describes has succeeded.
//
// remoteFiles is the document's file list as read from the server at doc's
// current hash, not doc.Files: sizes and membership come from what the
// server holds, not a possibly-stale cache. Every entry other than the two
// files this write may touch (.content, .metadata) survives byte-identical —
// see assertBaseContainment.
//
// rawContent/rawMetadata are the raw blobs as stored, not the parsed
// archive.Content / archive.MetadataFile: those structs model only part of
// what the device writes, so serialising them back would drop unknown keys.
// The write replaces one member of each file and leaves every other byte.
//
// The precondition is checked inside the Sync closure that calls this: Sync
// re-runs the closure on a generation conflict, which is the concurrent
// change the precondition exists to catch.
func planTagUpdate(doc *BlobDoc, remoteFiles []*Entry, rawContent, rawMetadata []byte, op TagOp, names []string, expectedRevision string, now int64) (*tagUpdatePlan, error) {
	before := doc.Hash
	if expectedRevision != "" && expectedRevision != before {
		return nil, &StaleRevisionError{
			DocumentID: doc.DocumentID,
			Expected:   expectedRevision,
			Actual:     before,
		}
	}

	contentName := addExt(doc.DocumentID, archive.ContentExt)
	metadataName := addExt(doc.DocumentID, archive.MetadataExt)
	var hasContent, hasMetadata bool
	for _, e := range remoteFiles {
		hasContent = hasContent || e.DocumentID == contentName
		hasMetadata = hasMetadata || e.DocumentID == metadataName
	}
	if !hasContent {
		return nil, fmt.Errorf("document %s has no %s entry", doc.DocumentID, archive.ContentExt)
	}
	if !hasMetadata {
		return nil, fmt.Errorf("document %s has no %s entry", doc.DocumentID, archive.MetadataExt)
	}

	content, err := replaceContentTags(rawContent, op, names, now)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", contentName, err)
	}

	result := &TagUpdateResult{
		DocumentID:     doc.DocumentID,
		Operation:      op.String(),
		BeforeRevision: before,
		AfterRevision:  before,
		BeforeTags:     content.BeforeTags,
		AfterTags:      content.AfterTags,
		PageTagCount:   content.PageTagCount,
	}
	if !content.Changed {
		return &tagUpdatePlan{Result: result}, nil
	}

	metadata, err := bumpMetadataLastModified(rawMetadata, now)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", metadataName, err)
	}

	files := make([]*Entry, len(remoteFiles))
	for i, f := range remoteFiles {
		c := *f
		files[i] = &c
	}

	contentHash, err := setEntryBlob(files, doc.DocumentID, archive.ContentExt, content.Bytes)
	if err != nil {
		return nil, err
	}
	metadataHash, err := setEntryBlob(files, doc.DocumentID, archive.MetadataExt, metadata)
	if err != nil {
		return nil, err
	}

	if err := assertBaseContainment(files, remoteFiles, doc.DocumentID); err != nil {
		return nil, err
	}

	docHash, err := HashEntries(files)
	if err != nil {
		return nil, err
	}
	var docSize int64
	for _, f := range files {
		docSize += f.Size
	}

	result.Changed = true
	result.AfterRevision = docHash
	result.ContentHash = contentHash
	result.MetadataHash = metadataHash

	return &tagUpdatePlan{
		Result:       result,
		Content:      content.Bytes,
		Metadata:     metadata,
		files:        files,
		docHash:      docHash,
		docSize:      docSize,
		tags:         content.AfterTagSet,
		lastModified: strconv.FormatInt(now, 10),
	}, nil
}

// assertBaseContainment is the load-bearing half of "never destroy ink": a
// tag write may change only .content and .metadata. It checks that files (the
// plan's copy) preserves every other entry of base byte-identical, with
// exactly the same membership.
func assertBaseContainment(files, base []*Entry, docID string) error {
	contentName := addExt(docID, archive.ContentExt)
	metadataName := addExt(docID, archive.MetadataExt)

	if len(files) != len(base) {
		return fmt.Errorf("document %s: plan has %d files, base list has %d", docID, len(files), len(base))
	}
	byName := make(map[string]*Entry, len(files))
	for _, f := range files {
		byName[f.DocumentID] = f
	}
	for _, b := range base {
		if b.DocumentID == contentName || b.DocumentID == metadataName {
			continue
		}
		f, ok := byName[b.DocumentID]
		if !ok {
			return fmt.Errorf("document %s: plan is missing %s", docID, b.DocumentID)
		}
		if *f != *b {
			return fmt.Errorf("document %s: plan changed %s, which a tag write must not touch", docID, b.DocumentID)
		}
	}
	return nil
}

// indexReader returns the doc-index body for the plan's files.
func (p *tagUpdatePlan) indexReader() (io.Reader, error) {
	return (&BlobDoc{Files: p.files}).IndexReader()
}

// apply commits the plan to doc. It is the only place a tag write may mutate
// the tree, and must run only after every blob the plan names has been
// uploaded. It snapshots doc's prior state first, so a write that applied
// locally but never landed on the server can be undone with rollback.
func (p *tagUpdatePlan) apply(doc *BlobDoc) {
	p.snapshot = &tagUpdateSnapshot{
		Hash:         doc.Hash,
		Size:         doc.Size,
		Files:        doc.Files,
		DocumentTags: doc.Content.DocumentTags,
		LastModified: doc.Metadata.LastModified,
	}
	doc.Files = p.files
	doc.Size = p.docSize
	doc.Hash = p.docHash
	doc.Content.DocumentTags = p.tags
	doc.Metadata.LastModified = p.lastModified
}

// rollback undoes apply: it restores doc to the state apply snapshotted. A
// plan that was never applied has no snapshot and rollback is a no-op.
func (p *tagUpdatePlan) rollback(doc *BlobDoc) {
	if p.snapshot == nil {
		return
	}
	doc.Hash = p.snapshot.Hash
	doc.Size = p.snapshot.Size
	doc.Files = p.snapshot.Files
	doc.Content.DocumentTags = p.snapshot.DocumentTags
	doc.Metadata.LastModified = p.snapshot.LastModified
}

// setEntryBlob points files' entry for docID's ext at blob's hash and size,
// matching by the exact name addExt(docID, ext) produces.
func setEntryBlob(files []*Entry, docID string, ext archive.RmExt, blob []byte) (string, error) {
	name := addExt(docID, ext)
	sum := sha256.Sum256(blob)
	hash := hex.EncodeToString(sum[:])
	for _, f := range files {
		if f.DocumentID == name {
			f.Hash = hash
			f.Size = int64(len(blob))
			return hash, nil
		}
	}
	return "", fmt.Errorf("document %s has no %s entry", docID, ext)
}

// contentTagsReplacement is the outcome of replaceContentTags.
type contentTagsReplacement struct {
	Bytes        []byte
	BeforeTags   []string
	AfterTags    []string
	AfterTagSet  []archive.Tag
	PageTagCount int
	Changed      bool
}

// contentTagElement is one element of the "tags" array, kept by occurrence
// rather than collapsed by name, so a document with duplicate-named tag
// elements (the device allows this) keeps every occurrence's original bytes.
type contentTagElement struct {
	Bytes []byte
	Tag   archive.Tag
}

// replaceContentTags applies op to the top-level "tags" member of a .content
// blob. Every byte outside that member is unchanged, and tags that survive
// the operation keep their original bytes, timestamps included — matched by
// occurrence, so two elements sharing a name are not collapsed into one. The
// document is treated as opaque JSON: keys this version of rmapi does not
// know survive because they are never parsed into a struct.
func replaceContentTags(raw []byte, op TagOp, names []string, now int64) (*contentTagsReplacement, error) {
	if !json.Valid(raw) {
		return nil, errors.New("content blob is not valid JSON")
	}

	rawElements, err := jsonArrayMember(raw, "tags")
	if err != nil {
		return nil, fmt.Errorf("tags: %w", err)
	}
	existing := make([]contentTagElement, 0, len(rawElements))
	for _, e := range rawElements {
		var t archive.Tag
		if err := json.Unmarshal(e, &t); err != nil {
			return nil, fmt.Errorf("tags: element does not parse as a tag: %w", err)
		}
		existing = append(existing, contentTagElement{Bytes: e, Tag: t})
	}

	pageTags, err := jsonArrayMember(raw, "pageTags")
	if err != nil {
		return nil, fmt.Errorf("pageTags: %w", err)
	}

	updated, err := applyTagElements(existing, op, names, now)
	if err != nil {
		return nil, err
	}
	changed := !tagElementsEqual(existing, updated)

	out := &contentTagsReplacement{
		Bytes:        raw,
		BeforeTags:   elementNames(existing),
		AfterTags:    dedupPreserveOrder(elementNames(updated)),
		AfterTagSet:  elementTags(updated),
		PageTagCount: len(pageTags),
		Changed:      changed,
	}
	if !changed {
		return out, nil
	}

	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, e := range updated {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(e.Bytes)
	}
	buf.WriteByte(']')

	out.Bytes, err = spliceJSONMember(raw, "tags", buf.Bytes())
	if err != nil {
		return nil, err
	}
	return out, nil
}

// applyTagElements computes the new element list for op. A surviving element
// keeps its original bytes by occurrence, not by name, so duplicate-named
// elements are never collided into one. Names being added are freshly
// marshaled with now as the timestamp.
func applyTagElements(existing []contentTagElement, op TagOp, names []string, now int64) ([]contentTagElement, error) {
	wanted := dedupNames(names)
	wantedSet := make(map[string]bool, len(wanted))
	for _, n := range wanted {
		wantedSet[n] = true
	}
	present := make(map[string]bool, len(existing))
	for _, e := range existing {
		present[e.Tag.Name] = true
	}

	var result []contentTagElement
	switch op {
	case TagOpAdd:
		result = append(result, existing...)
		for _, n := range wanted {
			if present[n] {
				continue
			}
			e, err := newTagElement(n, now)
			if err != nil {
				return nil, err
			}
			result = append(result, e)
		}
	case TagOpRemove:
		for _, e := range existing {
			if !wantedSet[e.Tag.Name] {
				result = append(result, e)
			}
		}
	default: // TagOpReplace
		for _, n := range wanted {
			kept := false
			for _, e := range existing {
				if e.Tag.Name == n {
					result = append(result, e)
					kept = true
				}
			}
			if !kept {
				e, err := newTagElement(n, now)
				if err != nil {
					return nil, err
				}
				result = append(result, e)
			}
		}
	}
	return result, nil
}

func newTagElement(name string, now int64) (contentTagElement, error) {
	tag := archive.Tag{Name: name, Timestamp: now}
	enc, err := json.Marshal(tag)
	if err != nil {
		return contentTagElement{}, err
	}
	return contentTagElement{Bytes: enc, Tag: tag}, nil
}

func dedupNames(names []string) []string {
	wanted := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		wanted = append(wanted, n)
	}
	return wanted
}

func dedupPreserveOrder(names []string) []string {
	out := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func elementNames(elems []contentTagElement) []string {
	names := make([]string, 0, len(elems))
	for _, e := range elems {
		names = append(names, e.Tag.Name)
	}
	return names
}

func elementTags(elems []contentTagElement) []archive.Tag {
	tags := make([]archive.Tag, 0, len(elems))
	for _, e := range elems {
		tags = append(tags, e.Tag)
	}
	return tags
}

func tagElementsEqual(a, b []contentTagElement) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Tag != b[i].Tag || !bytes.Equal(a[i].Bytes, b[i].Bytes) {
			return false
		}
	}
	return true
}

// bumpMetadataLastModified returns raw with only its top-level
// "lastModified" member set to now (a decimal string of milliseconds, as the
// device stores it). Every other byte is unchanged.
func bumpMetadataLastModified(raw []byte, now int64) ([]byte, error) {
	if !json.Valid(raw) {
		return nil, errors.New("metadata blob is not valid JSON")
	}
	value, err := json.Marshal(strconv.FormatInt(now, 10))
	if err != nil {
		return nil, err
	}
	return spliceJSONMember(raw, "lastModified", value)
}

// verifyTagSplice is the readback for a prepared .content blob, independent
// of the offsets the splice computed: spliced must differ from original in
// nothing but "tags", "tags" must say what the caller meant, and every
// surviving element must match its original bytes by occurrence, not just by
// name (so duplicate-named elements can't be swapped or collided).
func verifyTagSplice(original, spliced []byte, wantTags []string) error {
	var before, after map[string]json.RawMessage
	if err := json.Unmarshal(original, &before); err != nil {
		return fmt.Errorf("readback: original content blob does not parse: %w", err)
	}
	if err := json.Unmarshal(spliced, &after); err != nil {
		return fmt.Errorf("readback: spliced content blob does not parse: %w", err)
	}

	var tags []archive.Tag
	if err := json.Unmarshal(after["tags"], &tags); err != nil {
		return fmt.Errorf("readback: spliced tags do not parse: %w", err)
	}
	if got := dedupPreserveOrder(tagNames(tags)); !stringsEqual(got, wantTags) {
		return fmt.Errorf("readback: content blob has tags %v, intended %v", got, wantTags)
	}

	origElems, err := jsonArrayMember(original, "tags")
	if err != nil {
		return fmt.Errorf("readback: original tags: %w", err)
	}
	splicedElems, err := jsonArrayMember(spliced, "tags")
	if err != nil {
		return fmt.Errorf("readback: spliced tags: %w", err)
	}
	origByName := make(map[string][]json.RawMessage, len(origElems))
	for _, e := range origElems {
		var t archive.Tag
		if err := json.Unmarshal(e, &t); err != nil {
			return fmt.Errorf("readback: original tag element does not parse: %w", err)
		}
		origByName[t.Name] = append(origByName[t.Name], e)
	}
	consumed := make(map[string]int, len(origByName))
	for _, e := range splicedElems {
		var t archive.Tag
		if err := json.Unmarshal(e, &t); err != nil {
			return fmt.Errorf("readback: spliced tag element does not parse: %w", err)
		}
		bucket := origByName[t.Name]
		idx := consumed[t.Name]
		if idx >= len(bucket) {
			continue // a genuinely new element; no original bytes to match
		}
		consumed[t.Name] = idx + 1
		if !bytes.Equal(bytes.TrimSpace(e), bytes.TrimSpace(bucket[idx])) {
			return fmt.Errorf("readback: tag element %q (occurrence %d) does not match its original bytes", t.Name, idx+1)
		}
	}

	delete(before, "tags")
	delete(after, "tags")
	if len(before) != len(after) {
		return fmt.Errorf("readback: content blob has %d members outside tags, original had %d", len(after), len(before))
	}
	for k, v := range before {
		w, ok := after[k]
		if !ok {
			return fmt.Errorf("readback: content blob lost member %q", k)
		}
		if !bytes.Equal(v, w) {
			return fmt.Errorf("readback: content blob changed member %q", k)
		}
	}
	return nil
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// jsonArrayMember returns the elements of the top-level array member key, or
// nil when the member is absent.
func jsonArrayMember(raw []byte, key string) ([]json.RawMessage, error) {
	span, err := jsonMemberSpan(raw, key)
	if err != nil {
		return nil, err
	}
	if !span.found {
		return nil, nil
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(raw[span.start:span.end], &elements); err != nil {
		return nil, fmt.Errorf("member is not an array: %w", err)
	}
	return elements, nil
}

// memberSpan locates one top-level member of a JSON object. When found, the
// value occupies raw[start:end]. When not found, end is the offset of the
// object's closing brace and empty reports whether the object has no members.
type memberSpan struct {
	start, end int
	found      bool
	empty      bool
}

// jsonMemberSpan walks the top-level members of a JSON object with a token
// decoder, so nested objects and arrays are skipped whole. It refuses
// duplicate keys: a document naming the same member twice is ambiguous.
func jsonMemberSpan(raw []byte, key string) (memberSpan, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return memberSpan{}, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return memberSpan{}, errors.New("blob is not a JSON object")
	}

	span := memberSpan{}
	members := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return memberSpan{}, err
		}
		if d, ok := tok.(json.Delim); ok && d == '}' {
			if !span.found {
				span.end = int(dec.InputOffset()) - 1
				span.empty = members == 0
			}
			return span, nil
		}
		name, ok := tok.(string)
		if !ok {
			return memberSpan{}, fmt.Errorf("unexpected token %v", tok)
		}
		members++

		var value json.RawMessage
		if name != key {
			if err := dec.Decode(&value); err != nil {
				return memberSpan{}, err
			}
			continue
		}
		if span.found {
			return memberSpan{}, fmt.Errorf("duplicate member %q", key)
		}
		start, err := valueStart(raw, int(dec.InputOffset()))
		if err != nil {
			return memberSpan{}, err
		}
		if err := dec.Decode(&value); err != nil {
			return memberSpan{}, err
		}
		end := start + len(bytes.TrimLeft(raw[start:int(dec.InputOffset())], " \t\r\n"))
		span = memberSpan{start: start, end: end, found: true}
		if !bytes.Equal(bytes.TrimSpace(raw[start:end]), bytes.TrimSpace(value)) {
			return memberSpan{}, fmt.Errorf("member %q: span does not match decoded value", key)
		}
	}
}

// valueStart skips the ':' and surrounding whitespace after a member name
// ending at offset, and returns the offset where the value begins.
func valueStart(raw []byte, offset int) (int, error) {
	i := offset
	for i < len(raw) && isJSONSpace(raw[i]) {
		i++
	}
	if i >= len(raw) || raw[i] != ':' {
		return 0, errors.New("malformed member: expected ':'")
	}
	i++
	for i < len(raw) && isJSONSpace(raw[i]) {
		i++
	}
	return i, nil
}

func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

// spliceJSONMember returns raw with the top-level member key set to value and
// every other byte unchanged. An absent member is appended as the last one.
func spliceJSONMember(raw []byte, key string, value []byte) ([]byte, error) {
	span, err := jsonMemberSpan(raw, key)
	if err != nil {
		return nil, err
	}
	name, err := json.Marshal(key)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.Grow(len(raw) + len(value) + len(name) + 3)
	if span.found {
		out.Write(raw[:span.start])
		out.Write(value)
		out.Write(raw[span.end:])
	} else {
		out.Write(raw[:span.end])
		if !span.empty {
			out.WriteByte(',')
		}
		out.Write(name)
		out.WriteByte(':')
		out.Write(value)
		out.Write(raw[span.end:])
	}
	if !json.Valid(out.Bytes()) {
		return nil, fmt.Errorf("splice of %q produced invalid JSON", key)
	}
	return out.Bytes(), nil
}

type TagOp int

const (
	// TagOpReplace makes the document's tags exactly the supplied set.
	TagOpReplace TagOp = iota
	// TagOpAdd adds the supplied tags, leaving existing ones in place.
	TagOpAdd
	// TagOpRemove removes the supplied tags, leaving the rest in place.
	TagOpRemove
)

func (o TagOp) String() string {
	switch o {
	case TagOpAdd:
		return "add"
	case TagOpRemove:
		return "remove"
	default:
		return "replace"
	}
}

// applyTags computes the new document-tag set for an operation, and reports
// whether it differs from what was there. Tags that survive an operation keep
// their original timestamp, so a repeated write is a genuine no-op.
func applyTags(existing []archive.Tag, op TagOp, names []string, now int64) ([]archive.Tag, bool) {
	byName := make(map[string]archive.Tag, len(existing))
	for _, t := range existing {
		byName[t.Name] = t
	}

	wanted := make([]string, 0, len(names))
	named := make(map[string]bool, len(names))
	for _, n := range names {
		if n == "" || named[n] {
			continue
		}
		named[n] = true
		wanted = append(wanted, n)
	}

	var result []archive.Tag
	switch op {
	case TagOpAdd:
		result = make([]archive.Tag, 0, len(existing)+len(wanted))
		result = append(result, existing...)
		for _, n := range wanted {
			if _, present := byName[n]; !present {
				result = append(result, archive.Tag{Name: n, Timestamp: now})
			}
		}
	case TagOpRemove:
		result = make([]archive.Tag, 0, len(existing))
		for _, t := range existing {
			if !named[t.Name] {
				result = append(result, t)
			}
		}
	default:
		result = make([]archive.Tag, 0, len(wanted))
		for _, n := range wanted {
			if prior, present := byName[n]; present {
				result = append(result, prior)
			} else {
				result = append(result, archive.Tag{Name: n, Timestamp: now})
			}
		}
	}

	return result, !tagsEqual(existing, result)
}

func tagsEqual(a, b []archive.Tag) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (d *BlobDoc) MetadataHashAndReader() (hash string, reader io.Reader, err error) {
	jsn, err := json.Marshal(d.Metadata)
	if err != nil {
		return
	}
	sha := sha256.New()
	sha.Write(jsn)
	hash = hex.EncodeToString(sha.Sum(nil))
	log.Trace.Println("new hash", hash)
	reader = bytes.NewReader(jsn)
	found := false
	for _, f := range d.Files {
		if strings.HasSuffix(f.DocumentID, ".metadata") {
			f.Hash = hash
			f.Size = int64(len(jsn))
			found = true
			break
		}
	}
	if !found {
		err = errors.New("metadata not found")
	}

	return
}

func (d *BlobDoc) AddFile(e *Entry) error {
	d.Files = append(d.Files, e)
	size := int64(0)
	for _, f := range d.Files {
		size += f.Size
	}
	d.Size = size
	return d.Rehash()
}

func (t *HashTree) Add(d *BlobDoc) error {
	if len(d.Files) == 0 {
		return errors.New("no files")
	}
	t.Docs = append(t.Docs, d)
	return t.Rehash()
}

func (d *BlobDoc) IndexReader() (io.Reader, error) {
	return d.IndexReaderWithSchema("")
}

func (d *BlobDoc) IndexReaderWithSchema(schema string) (io.Reader, error) {
	if len(d.Files) == 0 {
		return nil, errors.New("no files")
	}

	if schema == "" {
		schema = SchemaVersionV3
	}

	// Same canonical-order requirement as the root index (see
	// HashTree.IndexReader): sort in place so the emitted body matches
	// HashEntries' ordering regardless of how d.Files got here.
	sort.Slice(d.Files, func(i, j int) bool { return d.Files[i].DocumentID < d.Files[j].DocumentID })

	var w bytes.Buffer
	w.WriteString(schema)
	w.WriteString("\n")
	for _, f := range d.Files {
		w.WriteString(f.Line())
		w.WriteString("\n")
	}

	return bytes.NewReader(w.Bytes()), nil
}

// ReadMetadata the document metadata from remote blob
func (d *BlobDoc) ReadMetadata(fileEntry *Entry, r RemoteStorage) error {
	if strings.HasSuffix(fileEntry.DocumentID, ".metadata") {
		log.Trace.Println("Reading metadata: " + d.DocumentID)

		metadata := archive.MetadataFile{}

		meta, err := r.GetReader(fileEntry.Hash, fileEntry.DocumentID)
		if err != nil {
			return err
		}
		defer meta.Close()
		content, err := io.ReadAll(meta)
		if err != nil {
			return err
		}
		err = json.Unmarshal(content, &metadata)
		if err != nil {
			log.Error.Printf("cannot read metadata %s %v", fileEntry.DocumentID, err)
		}
		log.Trace.Println("name from metadata: ", metadata.DocName)
		d.Metadata = metadata
	}

	if strings.HasSuffix(fileEntry.DocumentID, ".content") {
		log.Trace.Println("Reading content: " + d.DocumentID)

		contentData := archive.Content{}

		contentReader, err := r.GetReader(fileEntry.Hash, fileEntry.DocumentID)
		if err != nil {
			log.Warning.Printf("cannot get content reader %s: %v", fileEntry.DocumentID, err)
			return nil
		}
		defer contentReader.Close()

		contentBytes, err := io.ReadAll(contentReader)
		if err != nil {
			log.Warning.Printf("cannot read content bytes %s: %v", fileEntry.DocumentID, err)
			return nil
		}

		err = json.Unmarshal(contentBytes, &contentData)
		if err != nil {
			log.Warning.Printf("cannot parse content JSON %s: %v", fileEntry.DocumentID, err)
			return nil
		}

		// Ensure nil slices become empty arrays
		if contentData.DocumentTags == nil {
			contentData.DocumentTags = []archive.Tag{}
		}
		if contentData.PageTags == nil {
			contentData.PageTags = []archive.PageTag{}
		}

		log.Trace.Printf("parsed content for %s: %d document tags, %d page tags",
			d.DocumentID, len(contentData.DocumentTags), len(contentData.PageTags))
		d.Content = contentData
	}

	return nil
}

func (d *BlobDoc) Line() string {
	return d.LineWithSchema("")
}

func (d *BlobDoc) LineWithSchema(schema string) string {
	var sb strings.Builder
	if d.Hash == "" {
		log.Error.Print("missing hash for: ", d.DocumentID)
	}
	sb.WriteString(d.Hash)
	sb.WriteRune(Delimiter)

	typeField := FileType
	if schema == SchemaVersionV3 {
		typeField = DocType
	}
	sb.WriteString(typeField)
	sb.WriteRune(Delimiter)
	sb.WriteString(d.DocumentID)
	sb.WriteRune(Delimiter)

	numFilesStr := strconv.Itoa(len(d.Files))
	sb.WriteString(numFilesStr)
	sb.WriteRune(Delimiter)
	sb.WriteString(strconv.FormatInt(d.Size, 10))
	return sb.String()
}

// Mirror updates the document to be the same as the remote
func (d *BlobDoc) Mirror(e *Entry, r RemoteStorage) error {
	d.Entry = *e
	entryIndex, err := r.GetReader(e.Hash, addExt(e.DocumentID, archive.DocSchemaExt))
	if err != nil {
		return err
	}
	defer entryIndex.Close()
	entries, _, err := parseIndex(entryIndex)
	if err != nil {
		return fmt.Errorf("blobdoc index error %v", err)
	}

	head := make([]*Entry, 0)
	current := make(map[string]*Entry)
	new := make(map[string]*Entry)

	for _, e := range entries {
		new[e.DocumentID] = e
	}

	//updated and existing
	for _, currentEntry := range d.Files {
		if newEntry, ok := new[currentEntry.DocumentID]; ok {
			if newEntry.Hash != currentEntry.Hash {
				err = d.ReadMetadata(newEntry, r)
				if err != nil {
					return err
				}
				currentEntry.Hash = newEntry.Hash
				currentEntry.Size = newEntry.Size
			}
			head = append(head, currentEntry)
			current[currentEntry.DocumentID] = currentEntry
		}
	}

	//add missing
	for k, newEntry := range new {
		if _, ok := current[k]; !ok {
			err = d.ReadMetadata(newEntry, r)
			if err != nil {
				return err
			}
			head = append(head, newEntry)
		}
	}
	sort.Slice(head, func(i, j int) bool { return head[i].DocumentID < head[j].DocumentID })
	d.Files = head
	return nil

}
func (d *BlobDoc) ToDocument() *model.Document {
	var lastModified string
	unixTime, err := strconv.ParseInt(d.Metadata.LastModified, 10, 64)
	if err == nil {
		//HACK: convert wrong nano timestamps to millis
		if len(d.Metadata.LastModified) > 18 {
			unixTime /= 1000000
		}

		t := time.Unix(unixTime/1000, 0)
		lastModified = t.UTC().Format(time.RFC3339Nano)
	}

	tags := []string{}
	for _, tag := range d.Content.DocumentTags {
		tags = append(tags, tag.Name)
	}

	return &model.Document{
		ID:             d.DocumentID,
		Name:           d.Metadata.DocName,
		Version:        d.Metadata.Version,
		Parent:         d.Metadata.Parent,
		Type:           d.Metadata.CollectionType,
		CurrentPage:    d.Metadata.LastOpenedPage,
		Starred:        d.Metadata.Pinned,
		ModifiedClient: lastModified,
		Tags:           tags,
	}
}
