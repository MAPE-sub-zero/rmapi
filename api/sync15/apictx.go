package sync15

import (
	"archive/zip"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/juruen/rmapi/archive"
	"github.com/juruen/rmapi/filetree"
	"github.com/juruen/rmapi/log"
	"github.com/juruen/rmapi/model"
	"github.com/juruen/rmapi/transport"
	"github.com/juruen/rmapi/util"
)

// An ApiCtx allows you interact with the remote reMarkable API
type ApiCtx struct {
	Http        *transport.HttpClientCtx
	ft          *filetree.FileTreeCtx
	blobStorage *BlobStorage
	hashTree    *HashTree
}

// max number of concurrent requests
var concurrent = 20

func init() {
	c := os.Getenv("RMAPI_CONCURRENT")
	if u, err := strconv.Atoi(c); err == nil {
		concurrent = u
	}
}

func CreateCtx(http *transport.HttpClientCtx) (*ApiCtx, error) {
	apiStorage := NewBlobStorage(http)
	cacheTree, err := loadTree()
	if err != nil {
		fmt.Print(err)
		return nil, err
	}
	err = cacheTree.Mirror(apiStorage, concurrent)
	if err != nil {
		return nil, fmt.Errorf("failed to mirror %v", err)
	}
	saveTree(cacheTree)
	tree := DocumentsFileTree(cacheTree)
	return &ApiCtx{http, tree, apiStorage, cacheTree}, nil
}

func (ctx *ApiCtx) Filetree() *filetree.FileTreeCtx {
	return ctx.ft
}

func (ctx *ApiCtx) Refresh() (string, int64, error) {
	err := ctx.hashTree.Mirror(ctx.blobStorage, concurrent)
	if err != nil {
		return "", 0, err
	}
	ctx.ft = DocumentsFileTree(ctx.hashTree)
	return ctx.hashTree.Hash, ctx.hashTree.Generation, nil
}

// Nuke removes all documents from the account
func (ctx *ApiCtx) Nuke() (err error) {
	err = Sync(ctx.blobStorage, ctx.hashTree, func(t *HashTree) error {
		ctx.hashTree.Docs = nil
		ctx.hashTree.Rehash()
		return nil
	}, true)
	return err
}

// FetchDocument downloads a document given its ID and saves it locally into dstPath
func (ctx *ApiCtx) FetchDocument(docId, dstPath string) error {
	doc, err := ctx.hashTree.FindDoc(docId)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp("", "rmapizip")

	if err != nil {
		log.Error.Println("failed to create tmpfile for zip dir", err)
		return err
	}
	defer tmp.Close()

	w := zip.NewWriter(tmp)
	defer w.Close()
	for _, f := range doc.Files {
		log.Trace.Println("fetching document: ", f.DocumentID)
		blobReader, err := ctx.blobStorage.GetReader(f.Hash, f.DocumentID)
		if err != nil {
			return err
		}
		defer blobReader.Close()
		header := zip.FileHeader{}
		header.Name = f.DocumentID
		header.Modified = time.Now()
		zipWriter, err := w.CreateHeader(&header)
		if err != nil {
			return err
		}
		_, err = io.Copy(zipWriter, blobReader)

		if err != nil {
			return err
		}
	}
	w.Close()
	tmpPath := tmp.Name()
	_, err = util.CopyFile(tmpPath, dstPath)

	if err != nil {
		log.Error.Printf("failed to copy %s to %s, er: %s\n", tmpPath, dstPath, err.Error())
		return err
	}

	defer os.RemoveAll(tmp.Name())

	return nil
}

// CreateDir creates a remote directory with a given name under the parentId directory
func (ctx *ApiCtx) CreateDir(parentId, name string, notify bool) (*model.Document, error) {
	var err error

	files := &archive.DocumentFiles{}

	tmpDir, err := os.MkdirTemp("", "rmupload")
	if err != nil {
		return nil, err
	}
	id := uuid.New().String()
	objectName, filePath, err := archive.CreateMetadata(id, name, parentId, model.DirectoryType, tmpDir, nil)
	if err != nil {
		return nil, err
	}
	files.AddMap(objectName, filePath, archive.MetadataExt)

	objectName, filePath, err = archive.CreateContent(id, "", tmpDir, nil, nil, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	files.AddMap(objectName, filePath, archive.ContentExt)

	doc := NewBlobDoc(name, id, model.DirectoryType, parentId)

	for _, f := range files.Files {
		log.Info.Printf("File %s, path: %s", f.Name, f.Path)
		hash, size, err := FileHashAndSize(f.Path)
		if err != nil {
			return nil, err
		}
		hashStr := hex.EncodeToString(hash)
		fileEntry := &Entry{
			DocumentID: f.Name,
			Hash:       hashStr,
			Type:       FileType,
			Size:       size,
		}
		reader, err := os.Open(f.Path)
		if err != nil {
			return nil, err
		}
		//does not accept rm-file in header
		err = ctx.blobStorage.UploadBlob(hashStr, f.Name, reader)
		reader.Close()

		if err != nil {
			return nil, err
		}

		doc.AddFile(fileEntry)
	}

	log.Info.Println("Uploading new doc index...", doc.Hash)
	indexReader, err := doc.IndexReader()
	if err != nil {
		return nil, err
	}
	// defer indexReader.Close()
	err = ctx.blobStorage.UploadBlob(doc.Hash, addExt(doc.DocumentID, archive.DocSchemaExt), indexReader)
	if err != nil {
		return nil, err
	}

	err = Sync(ctx.blobStorage, ctx.hashTree, func(t *HashTree) error {
		return t.Add(doc)
	}, notify)

	if err != nil {
		return nil, err
	}

	if notify {
		err = ctx.SyncComplete()
		if err != nil {
			return nil, err
		}
	}

	return doc.ToDocument(), nil
}

// Sync applies changes to the local tree and syncs with the remote storage
func Sync(b *BlobStorage, tree *HashTree, operation func(t *HashTree) error, notify bool) error {
	syncTry := 0
	for {
		syncTry++
		if syncTry > 10 {
			log.Error.Println("Something is wrong")
			break
		}
		log.Info.Println("Syncing...")
		err := operation(tree)
		if err != nil {
			return err
		}

		indexReader, err := tree.IndexReader()
		if err != nil {
			return err
		}
		err = b.UploadBlob(tree.Hash, addExt("root", archive.DocSchemaExt), indexReader)
		if err != nil {
			return err
		}
		// TODO
		// defer indexReader.Close()

		log.Info.Println("updating root, old gen: ", tree.Generation)

		newGeneration, err := b.WriteRootIndex(tree.Hash, tree.Generation, notify)

		if err == nil {
			log.Info.Println("wrote root, new gen: ", newGeneration)
			tree.Generation = newGeneration
			break
		}

		if err != transport.ErrWrongGeneration {
			return err
		}

		log.Info.Println("wrong generation, re-reading remote tree")
		//resync and try again
		err = tree.Mirror(b, concurrent)
		if err != nil {
			return err
		}
		log.Warning.Println("remote tree has changed, refresh the file tree")
	}
	return saveTree(tree)
}

// DeleteEntry removes an entry: either an empty directory or a file
func (ctx *ApiCtx) DeleteEntry(node *model.Node, recursive, notify bool) error {
	if node.IsDirectory() && len(node.Children) > 0 && !recursive {
		return errors.New("directory is not empty")
	}

	err := Sync(ctx.blobStorage, ctx.hashTree, func(t *HashTree) error {
		return t.Remove(node.Document.ID)
	}, notify)
	return err
}

// MoveEntry moves an entry (either a directory or a file)
// - src is the source node to be moved
// - dstDir is an existing destination directory
// - name is the new name of the moved entry in the destination directory
func (ctx *ApiCtx) MoveEntry(src, dstDir *model.Node, name string) (*model.Node, error) {
	if dstDir.IsFile() {
		return nil, errors.New("destination directory is a file")
	}
	var err error

	err = Sync(ctx.blobStorage, ctx.hashTree, func(t *HashTree) error {
		doc, err := t.FindDoc(src.Document.ID)
		if err != nil {
			return err
		}
		doc.Metadata.Version++
		doc.Metadata.DocName = name
		doc.Metadata.Parent = dstDir.Id()
		doc.Metadata.MetadataModified = true

		hashStr, reader, err := doc.MetadataHashAndReader()
		if err != nil {
			return err
		}
		err = doc.Rehash()
		if err != nil {
			return err
		}
		err = t.Rehash()

		if err != nil {
			return err
		}

		err = ctx.blobStorage.UploadBlob(hashStr, addExt(doc.DocumentID, archive.MetadataExt), reader)

		if err != nil {
			return err
		}

		log.Info.Println("Uploading new doc index...", doc.Hash)
		indexReader, err := doc.IndexReader()
		if err != nil {
			return err
		}
		// defer indexReader.Close()
		return ctx.blobStorage.UploadBlob(doc.Hash, addExt(doc.DocumentID, archive.DocSchemaExt), indexReader)
	}, true)

	if err != nil {
		return nil, err
	}

	d, err := ctx.hashTree.FindDoc(src.Document.ID)
	if err != nil {
		return nil, err
	}

	return &model.Node{Document: d.ToDocument(), Children: src.Children, Parent: dstDir}, nil
}

// UploadDocument uploads a local document given by sourceDocPath under the parentId directory
func (ctx *ApiCtx) UploadDocument(parentId string, sourceDocPath string, notify bool, coverpage *int, currentPage *int, pageCount *int, contrastFilter *string) (*model.Document, error) {
	//TODO: overwrite file
	name, ext := util.DocPathToName(sourceDocPath)

	if name == "" {
		return nil, errors.New("file name is invalid")
	}

	if !util.IsFileTypeSupported(ext) {
		return nil, errors.New("unsupported file extension: " + ext)
	}

	var err error

	tmpDir, err := os.MkdirTemp("", "rmupload")
	if err != nil {
		return nil, err
	}

	defer os.RemoveAll(tmpDir)

	docFiles, id, err := archive.Prepare(name, parentId, sourceDocPath, ext, tmpDir, coverpage, currentPage, pageCount, contrastFilter)
	if err != nil {
		return nil, err
	}

	doc := NewBlobDoc(name, id, model.DocumentType, parentId)
	for _, f := range docFiles.Files {
		log.Info.Printf("File %s, path: %s", f.Name, f.Path)
		hash, size, err := FileHashAndSize(f.Path)
		if err != nil {
			return nil, err
		}
		hashStr := hex.EncodeToString(hash)
		fileEntry := &Entry{
			DocumentID: f.Name,
			Hash:       hashStr,
			Type:       FileType,
			Size:       size,
		}
		reader, err := os.Open(f.Path)
		if err != nil {
			return nil, err
		}
		err = ctx.blobStorage.UploadBlob(hashStr, fileEntry.DocumentID, reader)

		if err != nil {
			return nil, err
		}

		doc.AddFile(fileEntry)
	}

	log.Info.Printf("Uploading new doc index...%s, size: %d", doc.Hash, doc.Size)
	indexReader, err := doc.IndexReader()
	if err != nil {
		return nil, err
	}
	// defer indexReader.Close()
	err = ctx.blobStorage.UploadBlob(doc.Hash, addExt(doc.DocumentID, archive.DocSchemaExt), indexReader)
	if err != nil {
		return nil, err
	}

	err = Sync(ctx.blobStorage, ctx.hashTree, func(t *HashTree) error {
		return t.Add(doc)
	}, notify)

	if err != nil {
		return nil, err
	}

	return doc.ToDocument(), nil
}

// ReplaceDocumentFile replaces the main document file (e.g. PDF) of an existing document
// identified by docId with the local file given by sourceDocPath. Metadata and annotations
// remain untouched.
func (ctx *ApiCtx) ReplaceDocumentFile(docId, sourceDocPath string, notify bool) error {
	_, ext := util.DocPathToName(sourceDocPath)
	return Sync(ctx.blobStorage, ctx.hashTree, func(t *HashTree) error {
		doc, err := t.FindDoc(docId)
		if err != nil {
			return err
		}

		var fileEntry *Entry
		for _, f := range doc.Files {
			if strings.HasSuffix(f.DocumentID, "."+ext) {
				fileEntry = f
				break
			}
		}
		if fileEntry == nil {
			return fmt.Errorf("document does not contain .%s", ext)
		}

		hash, size, err := FileHashAndSize(sourceDocPath)
		if err != nil {
			return err
		}
		hashStr := hex.EncodeToString(hash)

		r, err := os.Open(sourceDocPath)
		if err != nil {
			return err
		}
		defer r.Close()

		if err := ctx.blobStorage.UploadBlob(hashStr, fileEntry.DocumentID, r); err != nil {
			return err
		}

		fileEntry.Hash = hashStr
		fileEntry.Size = size

		if err := doc.Rehash(); err != nil {
			return err
		}
		if err := t.Rehash(); err != nil {
			return err
		}

		indexReader, err := doc.IndexReader()
		if err != nil {
			return err
		}
		return ctx.blobStorage.UploadBlob(doc.Hash, addExt(doc.DocumentID, archive.DocSchemaExt), indexReader)
	}, notify)
}

// errTagWriteNoop aborts a Sync closure whose tag operation turned out to
// change nothing once the tree was current. Sync always rewrites the root
// index after a successful closure, which would bump the generation for a
// write that wrote nothing; returning an error leaves the remote untouched.
var errTagWriteNoop = errors.New("tags already as requested")

// SupersededError reports that the remote root has advanced past the
// generation this write was based on, and docId is no longer at the
// revision this write produced. The name is optimistic: reMarkable's sync
// protocol gives no way to tell "this write landed, and a later writer has
// since replaced it" apart from "this write never landed, and something
// else advanced the root instead" — both look identical from the readback.
// Treat this as ambiguous — unknown, needs investigation — never as
// confirmation the write succeeded.
type SupersededError struct {
	DocumentID          string
	ExpectedRevision    string // the revision this write produced
	ActualRevision      string // what the remote root now lists
	CommittedGeneration int64
	CurrentGeneration   int64
}

func (e *SupersededError) Error() string {
	return fmt.Sprintf(
		"superseded (ambiguous): remote root now lists revision %s for %s, not the %s this write produced; the root advanced from generation %d to %d, which could mean this write landed and was later replaced, or that it never landed at all — the server does not distinguish the two",
		e.ActualRevision, e.DocumentID, e.ExpectedRevision, e.CommittedGeneration, e.CurrentGeneration)
}

// NotCommittedError reports that a tag write never reached the server at
// all — typically Sync's syncTry>10 fail-open, which returns nil locally
// without ever landing a root write.
type NotCommittedError struct {
	DocumentID       string
	ExpectedRevision string
	ActualRevision   string
}

func (e *NotCommittedError) Error() string {
	return fmt.Sprintf(
		"not committed: remote root lists revision %s for %s, this write produced %s",
		e.ActualRevision, e.DocumentID, e.ExpectedRevision)
}

// UpdateDocumentTags applies a tag operation to one document with a
// stale-revision precondition, and returns a structured before/after report.
//
// Nothing in the tree is mutated until all three blobs (content, metadata,
// doc index) have uploaded: plan.apply runs last inside the closure, so a
// failed upload leaves the tree exactly as it found it. If Sync itself later
// fails (the root write never lands), the applied plan is rolled back.
//
// Sync's return value cannot be trusted for "did it land" on its own: after
// ten generation conflicts it gives up and returns nil. So after Sync the
// write is verified against the remote root, not the local cache — see
// verifyTagWriteLanded.
func (ctx *ApiCtx) UpdateDocumentTags(docId string, op TagOp, tags []string, expectedRevision string) (*TagUpdateResult, error) {
	var result *TagUpdateResult
	var plan *tagUpdatePlan
	var d *BlobDoc
	var attemptedRoot string
	applied := false

	err := Sync(ctx.blobStorage, ctx.hashTree, func(t *HashTree) error {
		applied = false
		var err error
		d, err = t.FindDoc(docId)
		if err != nil {
			return err
		}

		if err := ctx.assertRootContainment(t, docId, d); err != nil {
			return err
		}

		rawMetadata, err := ctx.fetchDocFile(d, archive.MetadataExt)
		if err != nil {
			return err
		}
		// The kind lives in the raw bytes, not d.Metadata: Mirror may skip
		// re-parsing a document whose hash it already recognises.
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(rawMetadata, &kind); err != nil {
			return fmt.Errorf("%s: %w", addExt(docId, archive.MetadataExt), err)
		}
		if kind.Type != model.DocumentType {
			return fmt.Errorf("%s is a %q, not a document", docId, kind.Type)
		}

		rawContent, err := ctx.fetchDocFile(d, archive.ContentExt)
		if err != nil {
			return err
		}

		remoteFiles, err := ctx.fetchRemoteDocIndex(d)
		if err != nil {
			return err
		}

		plan, err = planTagUpdate(d, remoteFiles, rawContent, rawMetadata, op, tags, expectedRevision, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		plan.generation = t.Generation
		result = plan.Result
		if !plan.Result.Changed {
			return errTagWriteNoop
		}

		if err := verifyTagSplice(rawContent, plan.Content, plan.Result.AfterTags); err != nil {
			return err
		}

		if err := ctx.blobStorage.UploadBlob(plan.Result.ContentHash, addExt(d.DocumentID, archive.ContentExt), bytes.NewReader(plan.Content)); err != nil {
			return err
		}
		if err := ctx.blobStorage.UploadBlob(plan.Result.MetadataHash, addExt(d.DocumentID, archive.MetadataExt), bytes.NewReader(plan.Metadata)); err != nil {
			return err
		}

		idx, err := plan.indexReader()
		if err != nil {
			return err
		}
		if err := ctx.blobStorage.UploadBlob(plan.docHash, addExt(d.DocumentID, archive.DocSchemaExt), idx); err != nil {
			return err
		}

		plan.apply(d)
		applied = true
		if err := t.Rehash(); err != nil {
			return err
		}
		// Captured now, not read back from ctx.hashTree.Hash after Sync
		// returns: a generation conflict on this attempt makes Sync mirror
		// the tree before retrying, which overwrites t.Hash with the
		// server's (still-unwritten) state. This is the hash Sync is about
		// to try to commit for this attempt, win or lose.
		attemptedRoot = t.Hash
		return nil
	}, true)

	if errors.Is(err, errTagWriteNoop) {
		log.Info.Println("tags already as requested; nothing to write")
		rootHash, _, rErr := ctx.blobStorage.GetRootIndex()
		if rErr != nil {
			return result, fmt.Errorf("readback: %w", rErr)
		}
		if rootHash != ctx.hashTree.Hash {
			return result, errors.New("local cache does not match the server root; refusing to report a no-op")
		}
		return result, nil
	}
	if err != nil {
		if applied && plan != nil && d != nil {
			plan.rollback(d)
			ctx.hashTree.Rehash()
		}
		return nil, err
	}

	result, err = ctx.verifyTagWriteLanded(docId, plan, attemptedRoot)
	if err != nil {
		return result, err
	}
	ctx.ft = DocumentsFileTree(ctx.hashTree)
	return result, nil
}

// fetchRemoteDocIndex reads a document's file index from the server at its
// currently cached hash — the base a tag write plans against, so plan sizes
// and membership come from what the server holds, not the local cache.
func (ctx *ApiCtx) fetchRemoteDocIndex(d *BlobDoc) ([]*Entry, error) {
	r, err := ctx.blobStorage.GetReader(d.Hash, addExt(d.DocumentID, archive.DocSchemaExt))
	if err != nil {
		return nil, fmt.Errorf("fetch remote index for %s: %w", d.DocumentID, err)
	}
	defer r.Close()
	entries, _, err := parseIndex(r)
	if err != nil {
		return nil, fmt.Errorf("fetch remote index for %s: %w", d.DocumentID, err)
	}
	return entries, nil
}

// assertRootContainment reads the remote root index at the tree's own hash
// and cross-checks the local cache against it, before any upload: every
// document the server lists there must be present locally, and docId's own
// entry must be at the revision the local cache believes it holds. This
// catches a truncated or stale local cache — one that still names a valid,
// existing server root, but no longer agrees with everything that root
// lists — the pre-commit half of "never destroy ink".
func (ctx *ApiCtx) assertRootContainment(t *HashTree, docId string, d *BlobDoc) error {
	rootReader, err := ctx.blobStorage.GetReader(t.Hash, addExt("root", archive.DocSchemaExt))
	if err != nil {
		return fmt.Errorf("root containment check: %w", err)
	}
	defer rootReader.Close()
	remoteRoot, _, err := parseIndex(rootReader)
	if err != nil {
		return fmt.Errorf("root containment check: %w", err)
	}

	localByID := make(map[string]*BlobDoc, len(t.Docs))
	for _, ld := range t.Docs {
		localByID[ld.DocumentID] = ld
	}

	var missing []string
	var docEntry *Entry
	for _, e := range remoteRoot {
		if _, ok := localByID[e.DocumentID]; !ok {
			missing = append(missing, e.DocumentID)
		}
		if e.DocumentID == docId {
			docEntry = e
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"local cache is missing %d document(s) the server has (e.g. %s); delete tree.cache and retry",
			len(missing), missing[0])
	}
	if docEntry != nil && docEntry.Hash != d.Hash {
		return fmt.Errorf(
			"cached revision %s for %s does not match the server's %s; delete tree.cache and retry",
			d.Hash, docId, docEntry.Hash)
	}
	return nil
}

// fetchDocFile reads one of a document's files, by the hash its entry carries.
func (ctx *ApiCtx) fetchDocFile(d *BlobDoc, ext archive.RmExt) ([]byte, error) {
	name := addExt(d.DocumentID, ext)
	for _, f := range d.Files {
		if f.DocumentID != name {
			continue
		}
		r, err := ctx.blobStorage.GetReader(f.Hash, f.DocumentID)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", name, err)
		}
		defer r.Close()
		raw, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		return raw, nil
	}
	return nil, fmt.Errorf("document %s has no %s file", d.DocumentID, name)
}

// verifyTagWriteLanded is the post-write readback: it proves the write from
// the server, not the local cache. The root index itself is trusted via its
// pointer, not re-downloaded on every path — content-addressing means a
// matching root/doc hash already proves which bytes are named. But the two
// blobs this write actually produced are verified by their bytes, not just
// the hash chain: a server that acknowledges a blob PUT and silently drops
// it would otherwise leave the root pointing at a doc index whose .content
// or .metadata 404s — a broken notebook reported as success. See
// verifyWriteSetLanded, which both "landed" branches below call before
// returning success.
//
// If the live root hash already equals attemptedRoot — the root Sync's last
// attempt actually tried to commit — the write landed at the root level.
// attemptedRoot, not ctx.hashTree.Hash, is the right comparison: after a
// generation conflict Sync mirrors the tree before retrying, which can leave
// ctx.hashTree.Hash matching the live (still-unwritten) server root by the
// time Sync gives up, which would otherwise read as a false "landed".
//
// Otherwise something else has changed the root since; the readback follows
// it one level to docId's own entry. If that entry is still at the revision
// this write produced, the write landed regardless of who moved the root.
//
// If not, and the root's generation is still the one this write was based
// on, the write never committed at all (NotCommittedError — Sync's
// syncTry>10 fail-open, most often; unambiguous, since nobody else could
// have advanced the generation either). If the generation has moved past
// that, SupersededError is reported — but that is ambiguous by construction:
// the server gives no way to tell "this write landed, then something else
// replaced it" from "this write never landed, and something else advanced
// the root instead." Callers must treat it as unknown, not as confirmation
// the write succeeded.
func (ctx *ApiCtx) verifyTagWriteLanded(docId string, plan *tagUpdatePlan, attemptedRoot string) (*TagUpdateResult, error) {
	rootHash, generation, err := ctx.blobStorage.GetRootIndex()
	if err != nil {
		return plan.Result, fmt.Errorf("readback: %w", err)
	}
	if rootHash == attemptedRoot {
		if err := ctx.verifyWriteSetLanded(docId, plan); err != nil {
			return plan.Result, err
		}
		return plan.Result, nil
	}

	rootReader, err := ctx.blobStorage.GetReader(rootHash, addExt("root", archive.DocSchemaExt))
	if err != nil {
		return plan.Result, fmt.Errorf("readback: %w", err)
	}
	defer rootReader.Close()
	rootEntries, _, err := parseIndex(rootReader)
	if err != nil {
		return plan.Result, fmt.Errorf("readback: %w", err)
	}

	var docEntry *Entry
	for _, e := range rootEntries {
		if e.DocumentID == docId {
			docEntry = e
			break
		}
	}
	if docEntry == nil {
		return plan.Result, fmt.Errorf("readback: remote root does not list %s", docId)
	}
	if docEntry.Hash == plan.Result.AfterRevision {
		if err := ctx.verifyWriteSetLanded(docId, plan); err != nil {
			return plan.Result, err
		}
		return plan.Result, nil
	}

	if generation > plan.generation {
		return plan.Result, &SupersededError{
			DocumentID:          docId,
			ExpectedRevision:    plan.Result.AfterRevision,
			ActualRevision:      docEntry.Hash,
			CommittedGeneration: plan.generation,
			CurrentGeneration:   generation,
		}
	}
	return plan.Result, &NotCommittedError{
		DocumentID:       docId,
		ExpectedRevision: plan.Result.AfterRevision,
		ActualRevision:   docEntry.Hash,
	}
}

// verifyWriteSetLanded proves, by bytes rather than by hash chain alone,
// that the exact blobs this write produced are retrievable from the server:
// the doc index at plan.Result.AfterRevision, and the .content/.metadata
// blobs it names. Three small GETs of only the bytes this write sent — no
// root index, no ink blobs.
func (ctx *ApiCtx) verifyWriteSetLanded(docId string, plan *tagUpdatePlan) error {
	docReader, err := ctx.blobStorage.GetReader(plan.Result.AfterRevision, addExt(docId, archive.DocSchemaExt))
	if err != nil {
		return fmt.Errorf("readback: %w", err)
	}
	defer docReader.Close()
	docEntries, _, err := parseIndex(docReader)
	if err != nil {
		return fmt.Errorf("readback: %w", err)
	}

	for _, want := range []struct {
		ext  archive.RmExt
		hash string
		sent []byte
	}{
		{archive.ContentExt, plan.Result.ContentHash, plan.Content},
		{archive.MetadataExt, plan.Result.MetadataHash, plan.Metadata},
	} {
		name := addExt(docId, want.ext)
		var fileEntry *Entry
		for _, e := range docEntries {
			if e.DocumentID == name {
				fileEntry = e
				break
			}
		}
		if fileEntry == nil {
			return fmt.Errorf("readback: doc index for %s has no %s entry", docId, want.ext)
		}
		if fileEntry.Hash != want.hash {
			return fmt.Errorf("readback: doc index for %s lists %s at %s, this write produced %s", docId, name, fileEntry.Hash, want.hash)
		}

		got, err := ctx.blobStorage.GetReader(fileEntry.Hash, name)
		if err != nil {
			return fmt.Errorf("readback: %w", err)
		}
		gotBytes, err := io.ReadAll(got)
		got.Close()
		if err != nil {
			return fmt.Errorf("readback: %w", err)
		}
		if !bytes.Equal(gotBytes, want.sent) {
			return fmt.Errorf("readback: %s differs from what this write sent", name)
		}
	}
	return nil
}

// DocumentsFileTree reads your remote documents and builds a file tree
// structure to represent them
func DocumentsFileTree(tree *HashTree) *filetree.FileTreeCtx {

	documents := make([]*model.Document, 0)
	for _, d := range tree.Docs {
		//dont show deleted (already cached)
		if d.Metadata.Deleted {
			continue
		}
		doc := d.ToDocument()
		documents = append(documents, doc)
	}

	fileTree := filetree.CreateFileTreeCtx()

	for _, d := range documents {
		log.Trace.Printf("adding: %s docid: %s ", d.Name, d.ID)
		fileTree.AddDocument(d)
	}
	fileTree.FinishAdd()

	return &fileTree
}

// SyncComplete notfies that somethings has changed (triggers tablet sync)
func (ctx *ApiCtx) SyncComplete() error {
	err := ctx.blobStorage.SyncComplete(ctx.hashTree.Generation)

	//sync can be called once per generation, ignore the error if nothing was changed
	if err == transport.ErrConflict {
		log.Trace.Printf("ignoring error: %v", err)
		return nil
	}

	if err != nil {
		log.Error.Printf("cannot send sync %v", err)
	}
	return nil
}
