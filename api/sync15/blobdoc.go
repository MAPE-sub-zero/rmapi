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

// TagUpdatePlan is a prepared tag write: the report, plus the exact bytes to
// upload. A no-op plan has Changed false and nil blobs, and must not be
// uploaded.
type TagUpdatePlan struct {
	Result   *TagUpdateResult
	Content  []byte
	Metadata []byte
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

// TagNames returns just the names of a tag set, in order.
func TagNames(tags []archive.Tag) []string {
	names := make([]string, 0, len(tags))
	for _, t := range tags {
		names = append(names, t.Name)
	}
	return names
}

// PlanTagUpdate enforces the stale-revision precondition, applies op to the
// document's .content and .metadata bytes, and prepares the blobs to upload.
// It reports a no-op rather than writing one.
//
// The inputs are the raw blobs as stored, not the parsed archive.Content and
// archive.MetadataFile: those structs model only part of what the device
// writes, so serialising them back over an existing document would drop every
// key they do not know. The write here replaces one member of each file and
// leaves every other byte as it was.
//
// The precondition must be evaluated wherever this is called from inside a
// Sync operation closure: Sync re-runs its closure against a freshly mirrored
// tree when the remote generation moved, so a check made once outside that
// closure would not see the concurrent change it exists to catch.
//
// An empty expectedRevision skips the check, for callers that genuinely intend
// a blind write.
func PlanTagUpdate(doc *BlobDoc, rawContent, rawMetadata []byte, op TagOp, names []string, expectedRevision string, now int64) (*TagUpdatePlan, error) {
	before := doc.Hash
	if expectedRevision != "" && expectedRevision != before {
		return nil, &StaleRevisionError{
			DocumentID: doc.DocumentID,
			Expected:   expectedRevision,
			Actual:     before,
		}
	}

	content, err := ReplaceContentTags(rawContent, op, names, now)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", addExt(doc.DocumentID, archive.ContentExt), err)
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
		return &TagUpdatePlan{Result: result}, nil
	}

	metadata, err := BumpMetadataLastModified(rawMetadata, now)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", addExt(doc.DocumentID, archive.MetadataExt), err)
	}

	contentHash, err := doc.setFileBlob(archive.ContentExt, content.Bytes)
	if err != nil {
		return nil, err
	}
	metadataHash, err := doc.setFileBlob(archive.MetadataExt, metadata)
	if err != nil {
		return nil, err
	}
	if err := doc.Rehash(); err != nil {
		return nil, err
	}

	result.Changed = true
	result.AfterRevision = doc.Hash
	result.ContentHash = contentHash
	result.MetadataHash = metadataHash

	return &TagUpdatePlan{
		Result:   result,
		Content:  content.Bytes,
		Metadata: metadata,
	}, nil
}

// setFileBlob points the document's entry for the file with extension ext at
// blob, and returns the blob's hash.
func (d *BlobDoc) setFileBlob(ext archive.RmExt, blob []byte) (string, error) {
	sum := sha256.Sum256(blob)
	hash := hex.EncodeToString(sum[:])
	for _, f := range d.Files {
		if strings.HasSuffix(f.DocumentID, "."+string(ext)) {
			f.Hash = hash
			f.Size = int64(len(blob))
			return hash, nil
		}
	}
	return "", fmt.Errorf("document %s has no %s entry", d.DocumentID, ext)
}

// ContentTagsReplacement is the outcome of ReplaceContentTags.
type ContentTagsReplacement struct {
	Bytes        []byte
	BeforeTags   []string
	AfterTags    []string
	PageTagCount int
	Changed      bool
}

// ReplaceContentTags applies op to the top-level "tags" member of a .content
// blob and returns the new blob. Every byte outside that member is unchanged,
// and tags that survive the operation keep their original bytes, timestamps
// included. When op changes nothing, Bytes is raw itself and Changed is false.
//
// The document is treated as an opaque JSON object: keys this version of rmapi
// has never heard of survive because they are never parsed into a struct.
func ReplaceContentTags(raw []byte, op TagOp, names []string, now int64) (*ContentTagsReplacement, error) {
	if !json.Valid(raw) {
		return nil, errors.New("content blob is not valid JSON")
	}

	elements, err := jsonArrayMember(raw, "tags")
	if err != nil {
		return nil, fmt.Errorf("tags: %w", err)
	}
	existing := make([]archive.Tag, 0, len(elements))
	original := make(map[string]json.RawMessage, len(elements))
	for _, e := range elements {
		var t archive.Tag
		if err := json.Unmarshal(e, &t); err != nil {
			return nil, fmt.Errorf("tags: element does not parse as a tag: %w", err)
		}
		existing = append(existing, t)
		if _, dup := original[t.Name]; !dup {
			original[t.Name] = e
		}
	}

	pageTags, err := jsonArrayMember(raw, "pageTags")
	if err != nil {
		return nil, fmt.Errorf("pageTags: %w", err)
	}

	updated, changed := ApplyTags(existing, op, names, now)
	out := &ContentTagsReplacement{
		Bytes:        raw,
		BeforeTags:   TagNames(existing),
		AfterTags:    TagNames(updated),
		PageTagCount: len(pageTags),
		Changed:      changed,
	}
	if !changed {
		return out, nil
	}

	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, t := range updated {
		if i > 0 {
			buf.WriteByte(',')
		}
		if prior, ok := original[t.Name]; ok && prior != nil {
			buf.Write(prior)
			continue
		}
		enc, err := json.Marshal(t)
		if err != nil {
			return nil, err
		}
		buf.Write(enc)
	}
	buf.WriteByte(']')

	out.Bytes, err = spliceJSONMember(raw, "tags", buf.Bytes())
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BumpMetadataLastModified returns the .metadata blob with only its top-level
// "lastModified" member set to now (as the device stores it: a decimal string
// of milliseconds). Every other byte is unchanged.
func BumpMetadataLastModified(raw []byte, now int64) ([]byte, error) {
	if !json.Valid(raw) {
		return nil, errors.New("metadata blob is not valid JSON")
	}
	value, err := json.Marshal(strconv.FormatInt(now, 10))
	if err != nil {
		return nil, err
	}
	return spliceJSONMember(raw, "lastModified", value)
}

// VerifyTagSplice is the readback for a prepared .content blob: it checks,
// independently of the offsets the splice computed, that spliced differs from
// original in nothing but the "tags" member and that the member says what the
// caller meant. It exists so a bug in the splice cannot reach the wire.
func VerifyTagSplice(original, spliced []byte, wantTags []string) error {
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
	if got := TagNames(tags); !stringsEqual(got, wantTags) {
		return fmt.Errorf("readback: content blob has tags %v, intended %v", got, wantTags)
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
// decoder, so nested objects and arrays are skipped whole and never
// interpreted. It refuses duplicate keys: a document that names the same
// member twice is ambiguous, and replacing one of them would leave the other
// to win on the device.
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

// valueStart skips the ':' and surrounding whitespace that follow a member
// name ending at offset, and returns the offset where the value begins.
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

// ApplyTags computes the new document-tag set for an operation, and reports
// whether it differs from what was there.
//
// Tags that survive an operation keep their original timestamp. That is what
// makes a repeated write a genuine no-op: re-running the same command produces
// an identical tag set, so changed is false and nothing is uploaded. Stamping
// every tag with the current time would make each retry look like a change and
// churn the document's revision.
func ApplyTags(existing []archive.Tag, op TagOp, names []string, now int64) ([]archive.Tag, bool) {
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
	// HashTree.IndexReader). Normally d.Files is already sorted, because
	// Rehash() -> HashEntries() sorts in place — but AddFile() appends and not
	// every write path is guaranteed to have rehashed first. Sorting here makes
	// the emitted body canonical regardless of how we got here; it is in-place
	// and idempotent, so it cannot disagree with HashEntries' ordering.
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
