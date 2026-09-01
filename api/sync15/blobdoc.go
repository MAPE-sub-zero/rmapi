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
	size := int64(0)
	for _, f := range d.Files {
		size += f.Size
	}
	d.Size = size

	hash, err := HashEntries(d.Files)
	if err != nil {
		return err
	}
	log.Trace.Println("New doc hash: ", hash)
	d.Hash = hash
	return nil
}

func (d *BlobDoc) ContentHashAndReader() (hash string, reader io.Reader, err error) {
	jsn, err := json.Marshal(d.Content)
	if err != nil {
		return
	}
	sha := sha256.New()
	sha.Write(jsn)
	hash = hex.EncodeToString(sha.Sum(nil))
	log.Trace.Println("new content hash", hash)
	reader = bytes.NewReader(jsn)
	found := false
	for _, f := range d.Files {
		if strings.HasSuffix(f.DocumentID, ".content") {
			f.Hash = hash
			f.Size = int64(len(jsn))
			found = true
			break
		}
	}
	if !found {
		err = errors.New("content not found")
	}

	return
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

// TagUpdatePlan is a prepared tag write: the report, plus the blobs to upload.
// A no-op plan has Changed false and nil readers, and must not be uploaded.
type TagUpdatePlan struct {
	Result         *TagUpdateResult
	ContentReader  io.Reader
	MetadataReader io.Reader
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

// PlanTagUpdate enforces the stale-revision precondition, applies op, and
// prepares the blobs to upload. It reports a no-op rather than writing one.
//
// The precondition must be evaluated wherever this is called from inside a
// Sync operation closure: Sync re-runs its closure against a freshly mirrored
// tree when the remote generation moved, so a check made once outside that
// closure would not see the concurrent change it exists to catch.
//
// An empty expectedRevision skips the check, for callers that genuinely intend
// a blind write.
func PlanTagUpdate(doc *BlobDoc, op TagOp, names []string, expectedRevision string, now int64) (*TagUpdatePlan, error) {
	before := doc.Hash
	beforeTags := TagNames(doc.Content.DocumentTags)

	if expectedRevision != "" && expectedRevision != before {
		return nil, &StaleRevisionError{
			DocumentID: doc.DocumentID,
			Expected:   expectedRevision,
			Actual:     before,
		}
	}

	result := &TagUpdateResult{
		DocumentID:     doc.DocumentID,
		Operation:      op.String(),
		BeforeRevision: before,
		AfterRevision:  before,
		BeforeTags:     beforeTags,
		AfterTags:      beforeTags,
		PageTagCount:   len(doc.Content.PageTags),
	}

	if !doc.ApplyDocumentTags(op, names, now) {
		return &TagUpdatePlan{Result: result}, nil
	}

	contentHash, contentReader, err := doc.ContentHashAndReader()
	if err != nil {
		return nil, err
	}
	metadataHash, metadataReader, err := doc.MetadataHashAndReader()
	if err != nil {
		return nil, err
	}
	if err := doc.Rehash(); err != nil {
		return nil, err
	}

	result.Changed = true
	result.AfterRevision = doc.Hash
	result.AfterTags = TagNames(doc.Content.DocumentTags)
	result.PageTagCount = len(doc.Content.PageTags)
	result.ContentHash = contentHash
	result.MetadataHash = metadataHash

	return &TagUpdatePlan{
		Result:         result,
		ContentReader:  contentReader,
		MetadataReader: metadataReader,
	}, nil
}

// VerifyTagWritePayload re-reads a serialised .content blob and checks it says
// what the caller meant. This is the readback: the in-memory struct being right
// is not evidence that the bytes on the wire are, and a tag write that silently
// dropped page tags would be unrecoverable ink loss.
func VerifyTagWritePayload(payload []byte, wantTags []string, wantPageTags int) error {
	var readBack archive.Content
	if err := json.Unmarshal(payload, &readBack); err != nil {
		return fmt.Errorf("readback: content blob does not parse: %w", err)
	}
	got := TagNames(readBack.DocumentTags)
	if len(got) != len(wantTags) {
		return fmt.Errorf("readback: content blob has tags %v, intended %v", got, wantTags)
	}
	for i := range got {
		if got[i] != wantTags[i] {
			return fmt.Errorf("readback: content blob has tags %v, intended %v", got, wantTags)
		}
	}
	if len(readBack.PageTags) != wantPageTags {
		return fmt.Errorf(
			"readback: content blob has %d page tags, expected %d to be preserved",
			len(readBack.PageTags), wantPageTags)
	}
	return nil
}

// TagOp selects how a tag write combines with a document's existing tags.
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

// ApplyDocumentTags applies op to the document's tags in place and reports
// whether anything changed. PageTags and the page .rm payloads are never
// touched. When nothing changes, LastModified is deliberately left alone so a
// no-op write does not alter the document.
func (d *BlobDoc) ApplyDocumentTags(op TagOp, names []string, now int64) bool {
	updated, changed := ApplyTags(d.Content.DocumentTags, op, names, now)
	if !changed {
		return false
	}
	d.Content.DocumentTags = updated
	if d.Content.PageTags == nil {
		d.Content.PageTags = []archive.PageTag{}
	}
	d.Metadata.LastModified = strconv.FormatInt(now, 10)
	d.Metadata.MetadataModified = true
	return true
}

func (d *BlobDoc) SetDocumentTags(tags []string) {
	now := time.Now().UnixMilli()
	docTags := make([]archive.Tag, len(tags))
	for i, t := range tags {
		docTags[i] = archive.Tag{
			Name:      t,
			Timestamp: now,
		}
	}
	d.Content.DocumentTags = docTags
	if d.Content.PageTags == nil {
		d.Content.PageTags = []archive.PageTag{}
	}
	d.Metadata.LastModified = strconv.FormatInt(now, 10)
	d.Metadata.MetadataModified = true
}

func (d *BlobDoc) UpdateDocumentTags(tags []string) error {
	d.SetDocumentTags(tags)
	if _, _, err := d.ContentHashAndReader(); err != nil {
		return err
	}
	if _, _, err := d.MetadataHashAndReader(); err != nil {
		return err
	}
	return d.Rehash()
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
